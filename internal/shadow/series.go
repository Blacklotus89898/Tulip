package shadow

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/Blacklotus89898/Tulip/internal/journal"
	"github.com/Blacklotus89898/Tulip/internal/portfolio"
)

// DayValue is one book's equity on one trading day.
type DayValue struct {
	Day    string  `json:"day"`
	Equity float64 `json:"equity"`
}

// BookSeries is one book's daily marks from the common start — the same
// marks and the same start the scorecard measures, so a chart can never
// disagree with the table.
type BookSeries struct {
	Book   string     `json:"book"`
	Role   string     `json:"role"` // live | benchmark | candidate
	Points []DayValue `json:"points"`
}

// Series returns every book's marks since the first shadow rebalance, live
// and benchmark first, then the candidates by name. since is "" (and the
// slice empty) until the shadow books have started.
func Series(ctx context.Context, j *journal.Journal) (since string, out []BookSeries, err error) {
	evs, err := j.ByKinds(ctx, KindRebalanced, KindMark, LiveMarkKind)
	if err != nil {
		return "", nil, err
	}
	sort.Slice(evs, func(a, b int) bool { return evs[a].ID < evs[b].ID })
	marks := map[string][]DayValue{}
	for _, e := range evs {
		var m struct {
			Book, Day string
			Equity    float64
		}
		if json.Unmarshal(e.Payload, &m) != nil {
			continue
		}
		switch e.Kind {
		case KindRebalanced:
			if since == "" {
				since = startDay(m.Day, e.TS)
			}
		case KindMark, LiveMarkKind:
			marks[m.Book] = append(marks[m.Book], DayValue{m.Day, m.Equity})
		}
	}
	if since == "" {
		return "", nil, nil
	}
	add := func(book, role string) {
		var pts []DayValue
		for _, p := range marks[book] {
			if p.Day >= since {
				pts = append(pts, p)
			}
		}
		out = append(out, BookSeries{Book: book, Role: role, Points: pts})
	}
	add(portfolio.Strategy, "live")
	add(portfolio.Benchmark, "benchmark")
	var shadows []string
	for b := range marks {
		if strings.HasPrefix(b, Prefix) {
			shadows = append(shadows, b)
		}
	}
	sort.Strings(shadows)
	for _, b := range shadows {
		add(b, "candidate")
	}
	return since, out, nil
}
