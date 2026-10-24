package journal

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func open(t *testing.T) *Journal {
	t.Helper()
	j, err := Open(context.Background(), filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}

func TestAppendAndList(t *testing.T) {
	j := open(t)
	ctx := context.Background()
	for i, k := range []string{"observation", "candidate", "observation"} {
		if _, err := j.Append(ctx, k, "c1", map[string]int{"n": i}); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := j.List(ctx, Query{})
	if len(all) != 3 || all[0].Kind != "observation" || all[0].ID < all[2].ID {
		t.Fatalf("List newest-first: %+v", all)
	}
	obs, _ := j.List(ctx, Query{Kind: "observation"})
	if len(obs) != 2 {
		t.Errorf("kind filter: %d", len(obs))
	}
	tail, _ := j.List(ctx, Query{AfterID: all[2].ID})
	if len(tail) != 2 || tail[0].ID > tail[1].ID {
		t.Errorf("AfterID tail should be oldest-first: %+v", tail)
	}
	var p map[string]int
	_ = json.Unmarshal(all[0].Payload, &p)
	if p["n"] != 2 {
		t.Errorf("payload round-trip: %v", p)
	}
	c, _ := j.Counts(ctx)
	if c["observation"] != 2 || c["candidate"] != 1 {
		t.Errorf("counts = %v", c)
	}
}

// The database itself refuses edits — append-only is not a convention.
func TestAppendOnlyIsEnforced(t *testing.T) {
	j := open(t)
	ctx := context.Background()
	e, _ := j.Append(ctx, "fill", "", map[string]string{"symbol": "XIU.TO"})
	if _, err := j.db.ExecContext(ctx, `UPDATE events SET kind = 'oops' WHERE id = ?`, e.ID); err == nil {
		t.Error("UPDATE must be rejected")
	}
	if _, err := j.db.ExecContext(ctx, `DELETE FROM events WHERE id = ?`, e.ID); err == nil {
		t.Error("DELETE must be rejected")
	}
}

func TestClaimApprovalIsSingleUseUnderConcurrency(t *testing.T) {
	j := open(t)
	ctx := context.Background()
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() { results <- j.ClaimApproval(ctx, "sug_1") }()
	}
	won, used := 0, 0
	for i := 0; i < 8; i++ {
		switch err := <-results; {
		case err == nil:
			won++
		case err == ErrAlreadyUsed:
			used++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 || used != 7 {
		t.Errorf("exactly one approval may win: won=%d used=%d", won, used)
	}
	if _, err := j.db.ExecContext(ctx, `DELETE FROM approvals_used`); err == nil {
		t.Error("approvals must not be deletable")
	}
}

func TestByKinds(t *testing.T) {
	j := open(t)
	ctx := context.Background()
	for _, k := range []string{"a", "b", "c", "a"} {
		_, _ = j.Append(ctx, k, "", map[string]string{})
	}
	evs, _ := j.ByKinds(ctx, "a", "c")
	if len(evs) != 3 || evs[0].Kind != "a" || evs[0].ID > evs[2].ID {
		t.Errorf("ByKinds oldest-first: %+v", evs)
	}
}
