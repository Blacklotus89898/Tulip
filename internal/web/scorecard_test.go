package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Blacklotus89898/Tulip/internal/shadow"
	"github.com/Blacklotus89898/Tulip/internal/strategy"
)

func TestScorecardPageIsReadOnlyAndCached(t *testing.T) {
	calls := 0
	load := func(context.Context) (*ScorecardData, error) {
		calls++
		return &ScorecardData{
			Live: "trend-filter-200", AsOf: "2026-11-30",
			Card: &shadow.Card{Since: "2026-11-02", Days: 21, Note: "noise under 12 months", Rows: []shadow.Row{
				{Book: "strategy", Role: "live"},
				{Book: "benchmark", Role: "benchmark", StartEquity: 10000, ReturnPct: 1.5, MaxDDPct: -2.1, Trades: 0},
				{Book: "shadow:trend-filter-200", Role: "candidate", StartEquity: 10000, ReturnPct: 2.25, MaxDDPct: -1.4, Trades: 3, Sells: 1, RealizedPnL: -4},
			}},
			Candidates: map[string]strategy.Decision{
				"trend-filter-200": {Targets: map[string]float64{"XIU.TO": 0.5, "ZAG.TO": 0.5}, Reasons: map[string]string{"XIU.TO": "XIU.TO is above its 200-day average <b>"}},
				"equal-weight":     {Targets: map[string]float64{}, Reasons: map[string]string{}},
			},
		}, nil
	}
	mux := http.NewServeMux()
	RegisterScorecard(mux, load)
	get := func() string {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/scorecard", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		return rec.Body.String()
	}
	body := get()
	for _, want := range []string{"Since 2026-11-02", "&#43;2.2%", "max drop -1.4%", "1 sells", "realized −$4",
		"your paper book", "not started", "XIU buy &amp; hold", "candidate · live strategy",
		"XIU 50% · ZAG 50%", "all cash", "&lt;b&gt;", `name="viewport"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(body, "<form") || strings.Contains(body, "passphrase") {
		t.Error("the scorecard must be read-only")
	}
	// The live strategy's candidate card comes first.
	if i, j := strings.Index(body, ">trend-filter-200<"), strings.Index(body, ">equal-weight<"); i < 0 || j < 0 || i > j {
		t.Errorf("live strategy should be listed first (%d, %d)", i, j)
	}
	get()
	if calls != 1 {
		t.Errorf("loader called %d times — page views must be cached", calls)
	}
}
