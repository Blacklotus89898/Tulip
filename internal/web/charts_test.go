package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Blacklotus89898/Tulip/internal/backtest"
	"github.com/Blacklotus89898/Tulip/internal/shadow"
	"github.com/Blacklotus89898/Tulip/internal/strategy"
)

func chartsMux(liveCalls, btCalls *int, btErr error) *http.ServeMux {
	mux := http.NewServeMux()
	RegisterCharts(mux, ChartsSources{
		Live: func(context.Context) (*LiveCharts, error) {
			*liveCalls++
			return &LiveCharts{Live: "trend-filter-200", Symbols: []string{"XIU.TO", "VFV.TO", "ZAG.TO"}, Since: "2026-11-02", AsOf: "2026-11-04",
				Books: []shadow.BookSeries{{Book: "strategy", Role: "live", Points: []shadow.DayValue{{Day: "2026-11-02", Equity: 10000}, {Day: "2026-11-03", Equity: 10060}}}},
				Decisions: map[string]strategy.Decision{
					"equal-weight":     {Targets: map[string]float64{"XIU.TO": 1.0 / 3, "VFV.TO": 1.0 / 3, "ZAG.TO": 1.0 / 3}, Reasons: map[string]string{"XIU.TO": "equal"}},
					"trend-filter-200": {Targets: map[string]float64{"XIU.TO": 0.5, "ZAG.TO": 0.5}, Reasons: map[string]string{"VFV.TO": "below its 200-day average"}},
				},
				Counts: map[string]int64{"suggestion.created": 4, "suggestion.rejected": 1, "approval.accepted": 2, "order.filled": 2, "claude.review": 3}}, nil
		},
		Backtest: func(context.Context) (*BacktestSet, error) {
			*btCalls++
			if btErr != nil {
				return nil, btErr
			}
			return &BacktestSet{Universe: []string{"XIU.TO", "ZAG.TO"}, Start: "2014-04-01", End: "2026-11-04", HoldoutStart: "2022-12-01",
				Runs: []BacktestRun{{Name: "trend-filter-200", Live: true, Curve: []backtest.Point{{Day: "2014-04-01", Equity: 10000}}}}}, nil
		},
	})
	return mux
}

func get(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestChartsPageIsReadOnly(t *testing.T) {
	var l, b int
	rec := get(t, chartsMux(&l, &b, nil), "/charts")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, "charts.json") || !strings.Contains(body, `name="viewport"`) {
		t.Fatalf("page: %d", rec.Code)
	}
	for _, bad := range []string{"<form", "passphrase", "/approve", "killswitch", "<script src", `src="http`, `href="http`, "fetch(\"http"} {
		if strings.Contains(body, bad) {
			t.Errorf("charts page must be read-only and self-contained; found %q", bad)
		}
	}
	if rec.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Error("charts page needs the same framing/CSP lockdown as the other pages")
	}
}

func TestChartsJSONShapesAndCaches(t *testing.T) {
	var l, b int
	mux := chartsMux(&l, &b, nil)
	var v struct {
		Live       string
		Since      string
		Books      []shadow.BookSeries
		Backtest   *BacktestSet
		Strategies []Strategy
		Pipeline   []Step
	}
	if err := json.Unmarshal(get(t, mux, "/charts.json").Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Since != "2026-11-02" || len(v.Books) != 1 || v.Backtest == nil || len(v.Backtest.Runs) != 1 {
		t.Fatalf("view = %+v", v)
	}
	if len(v.Strategies) != 2 || v.Strategies[0].Name != "trend-filter-200" || !v.Strategies[0].Live {
		t.Fatalf("live strategy first: %+v", v.Strategies)
	}
	tf := v.Strategies[0]
	if tf.Rule == "" || tf.Test == "" || len(tf.Symbols) != 3 || !tf.Symbols[2].Safe || tf.Symbols[1].Weight != 0 || tf.Symbols[1].Reason == "" {
		t.Errorf("trend-filter diagram = %+v", tf)
	}
	counts := map[string]int64{}
	for _, s := range v.Pipeline {
		counts[s.ID] = s.Count
	}
	if counts["proposed"] != 5 || counts["thorn-propose"] != 1 || counts["approved"] != 2 || counts["filled"] != 2 || counts["review"] != 3 {
		t.Errorf("pipeline counts = %v", counts)
	}
	get(t, mux, "/charts.json")
	if l != 1 || b != 1 {
		t.Errorf("live loaded %d×, backtest %d× — both must be cached", l, b)
	}
}

func TestBacktestOutageDoesNotBreakThePage(t *testing.T) {
	var l, b int
	mux := chartsMux(&l, &b, errors.New("yahoo 429"))
	rec := get(t, mux, "/charts.json")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "backtest unavailable: yahoo 429") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	get(t, mux, "/charts.json")
	if b != 1 {
		t.Errorf("a failed backtest is retried hourly, not per view (%d calls)", b)
	}
}

func TestEveryCandidateHasARuleForItsDiagram(t *testing.T) {
	for _, a := range strategy.Candidates([]string{"XIU.TO", "VFV.TO", "XEF.TO", "ZAG.TO"}) {
		if r, ok := rules[a.Name()]; !ok || r[0] == "" || r[1] == "" {
			t.Errorf("%s has no rule description for the diagram", a.Name())
		}
	}
}
