package shadow

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"

	"github.com/Blacklotus89898/Tulip/internal/journal"
	"github.com/Blacklotus89898/Tulip/internal/portfolio"
)

// LiveMarkKind is desk.KindMark (pinned by a test): the live and benchmark
// books' daily marks. Not imported, so the shadow package can't reach the desk.
const LiveMarkKind = "portfolio.mark"

// Row is one book on the scorecard, measured from the card's common start.
type Row struct {
	Book        string             `json:"book"`
	Role        string             `json:"role"` // live | benchmark | candidate
	StartEquity float64            `json:"start_equity,omitempty"`
	Equity      float64            `json:"equity,omitempty"`
	ReturnPct   float64            `json:"return_pct"`
	MaxDDPct    float64            `json:"max_drawdown_pct"`
	Trades      int                `json:"trades"`
	Sells       int                `json:"sells"` // each realizes a taxable gain/loss (cash account)
	FeesPaid    float64            `json:"fees_paid"`
	RealizedPnL float64            `json:"realized_pnl"`
	Rejected    int                `json:"thorn_rejected,omitempty"`
	Targets     map[string]float64 `json:"last_targets,omitempty"`
	Marks       int                `json:"days_marked"`
}

// Card compares every book over the same period.
type Card struct {
	Since string `json:"since"` // first shadow rebalance day = common start
	Days  int    `json:"days"`
	Rows  []Row  `json:"rows"`
	Note  string `json:"note"`
}

type mark struct {
	Book, Day string
	Equity    float64
}

// Build reads the journal and scores every book from the common start.
// Equity is from daily marks (taken at the first cycle of each trading day).
func Build(ctx context.Context, j *journal.Journal) (*Card, error) {
	evs, err := j.ByKinds(ctx, KindFunded, KindFill, KindRebalanced, KindRejected, KindMark,
		LiveMarkKind, portfolio.KindFunded, portfolio.KindFill)
	if err != nil {
		return nil, err
	}
	sort.Slice(evs, func(a, b int) bool { return evs[a].ID < evs[b].ID })

	card := &Card{}
	marks := map[string][]mark{}
	targets := map[string]map[string]float64{}
	rejected := map[string]int{}
	for _, e := range evs {
		var m struct {
			Book, Day, Month string
			Equity           float64
			Targets          map[string]float64
		}
		if json.Unmarshal(e.Payload, &m) != nil {
			continue
		}
		switch e.Kind {
		case KindRebalanced:
			if card.Since == "" && len(e.TS) >= 10 {
				card.Since = e.TS[:10] // UTC date ≈ Toronto date during TSX hours
			}
			targets[m.Book] = m.Targets
		case KindRejected:
			rejected[m.Book]++
		case KindMark, LiveMarkKind:
			marks[m.Book] = append(marks[m.Book], mark{m.Book, m.Day, m.Equity})
		}
	}
	if card.Since == "" {
		card.Note = "No shadow rebalance yet: the candidates start together in the first rebalance window (first 5 trading days of a month). Until then there is nothing to compare."
		return card, nil
	}

	var shadows []string
	for b := range marks {
		if strings.HasPrefix(b, Prefix) {
			shadows = append(shadows, b)
		}
	}
	sort.Strings(shadows)
	books, err := portfolio.ReplayKinds(evs, KindFunded, KindFill, shadows...)
	if err != nil {
		return nil, err
	}
	live, err := portfolio.Replay(evs, portfolio.Strategy, portfolio.Benchmark)
	if err != nil {
		return nil, err
	}
	trades, sells, fees := map[string]int{}, map[string]int{}, map[string]float64{}
	for _, e := range evs {
		if e.Kind != KindFill && e.Kind != portfolio.KindFill {
			continue
		}
		if len(e.TS) >= 10 && e.TS[:10] < card.Since {
			continue
		}
		var f portfolio.Fill
		if json.Unmarshal(e.Payload, &f) != nil {
			continue
		}
		trades[f.Book]++
		fees[f.Book] += f.Fee
		if f.Side == "sell" {
			sells[f.Book]++
		}
	}

	row := func(book, role string, b *portfolio.Book) Row {
		r := Row{Book: book, Role: role, Trades: trades[book], Sells: sells[book], FeesPaid: round2(fees[book]),
			Rejected: rejected[book], Targets: targets[book]}
		if b != nil {
			r.RealizedPnL = round2(b.RealizedPnL)
		}
		var start, peak, maxDD float64
		for _, m := range marks[book] {
			if m.Day < card.Since {
				continue
			}
			if start == 0 {
				start, peak = m.Equity, m.Equity
			}
			r.Marks++
			r.Equity = m.Equity
			peak = math.Max(peak, m.Equity)
			if peak > 0 {
				maxDD = math.Min(maxDD, m.Equity/peak-1)
			}
		}
		if start > 0 {
			r.StartEquity = start
			r.ReturnPct = round2((r.Equity/start - 1) * 100)
			r.MaxDDPct = round2(maxDD * 100)
		}
		return r
	}
	card.Rows = append(card.Rows, row(portfolio.Strategy, "live", live[portfolio.Strategy]))
	card.Rows = append(card.Rows, row(portfolio.Benchmark, "benchmark", live[portfolio.Benchmark]))
	for _, s := range shadows {
		card.Rows = append(card.Rows, row(s, "candidate", books[s]))
	}
	for _, r := range card.Rows {
		card.Days = max(card.Days, r.Marks)
	}
	card.Note = "Measured from the common start (first shadow rebalance). Candidates trade instantly at the rebalance cycle's quotes; the live book trades when you approve, so live vs shadow:" +
		"trend-filter-200 is the cost of approval timing. Differences under ~12 months are noise — the promotion rule (ADR 0004) needs 12 months and the backtest gate."
	return card, nil
}
