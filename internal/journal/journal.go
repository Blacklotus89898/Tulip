// Package journal is Tulip's append-only event log (ADR 0001 §5, ADR 0002 §2):
// every observation, candidate, brain verdict, Thorn verdict, human decision,
// fill and expiry is an event. Events are inserted, never updated or deleted —
// state (positions, cash, pending suggestions) is derived from them. SQLite in
// WAL mode with a single writer, the Petal registry pattern.
package journal

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// Event is one journal entry.
type Event struct {
	ID      int64           `json:"id"`
	TS      string          `json:"ts"`
	Kind    string          `json:"kind"`
	Cycle   string          `json:"cycle,omitempty"` // the loop iteration that produced it
	Payload json.RawMessage `json:"payload"`
}

// Journal wraps the SQLite event store.
type Journal struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS events (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	ts      TEXT NOT NULL,
	kind    TEXT NOT NULL,
	cycle   TEXT NOT NULL DEFAULT '',
	payload TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS idx_events_kind ON events(kind, id);
CREATE INDEX IF NOT EXISTS idx_events_cycle ON events(cycle);
-- Append-only by construction: the database itself refuses edits.
CREATE TRIGGER IF NOT EXISTS events_no_update BEFORE UPDATE ON events
BEGIN SELECT RAISE(ABORT, 'journal is append-only'); END;
CREATE TRIGGER IF NOT EXISTS events_no_delete BEFORE DELETE ON events
BEGIN SELECT RAISE(ABORT, 'journal is append-only'); END;
`

// Open opens (creating if needed) the journal at path.
func Open(ctx context.Context, path string) (*Journal, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // one writer: no SQLITE_BUSY
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, err
	}
	return &Journal{db: db}, nil
}

// Close closes the database.
func (j *Journal) Close() error { return j.db.Close() }

// Append records an event. payload is JSON-encoded.
func (j *Journal) Append(ctx context.Context, kind, cycle string, payload any) (*Event, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("journal: encode %s: %w", kind, err)
	}
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := j.db.ExecContext(ctx, `INSERT INTO events (ts, kind, cycle, payload) VALUES (?, ?, ?, ?)`, ts, kind, cycle, string(raw))
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Event{ID: id, TS: ts, Kind: kind, Cycle: cycle, Payload: raw}, nil
}

// Query narrows List. Zero values match everything.
type Query struct {
	Kind    string // exact
	Cycle   string // exact
	AfterID int64  // events with id > AfterID (tailing)
	Limit   int    // default 50, max 500
}

// List returns matching events, newest first (or oldest first when tailing
// with AfterID, so a reader can resume in order).
func (j *Journal) List(ctx context.Context, q Query) ([]Event, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 500 {
		q.Limit = 500
	}
	sqlq := `SELECT id, ts, kind, cycle, payload FROM events WHERE 1=1`
	var args []any
	if q.Kind != "" {
		sqlq += ` AND kind = ?`
		args = append(args, q.Kind)
	}
	if q.Cycle != "" {
		sqlq += ` AND cycle = ?`
		args = append(args, q.Cycle)
	}
	order := "DESC"
	if q.AfterID > 0 {
		sqlq += ` AND id > ?`
		args = append(args, q.AfterID)
		order = "ASC"
	}
	sqlq += ` ORDER BY id ` + order + ` LIMIT ?`
	args = append(args, q.Limit)
	rows, err := j.db.QueryContext(ctx, sqlq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var payload string
		if err := rows.Scan(&e.ID, &e.TS, &e.Kind, &e.Cycle, &payload); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

// Counts returns the number of events per kind (for /metrics).
func (j *Journal) Counts(ctx context.Context) (map[string]int64, error) {
	rows, err := j.db.QueryContext(ctx, `SELECT kind, count(*) FROM events GROUP BY kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var n int64
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, rows.Err()
}
