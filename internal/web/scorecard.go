package web

import (
	"context"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/calendar"
	"github.com/Blacklotus89898/Tulip/internal/shadow"
	"github.com/Blacklotus89898/Tulip/internal/strategy"
)

// ScorecardData is what the scorecard page shows: the shadow-book comparison
// (Tulip ADR 0004) and what each candidate would hold now, with its reasons.
type ScorecardData struct {
	Live        string
	Card        *shadow.Card
	Candidates  map[string]strategy.Decision
	AsOf        string
	Unavailable []string
}

// ScorecardLoader builds the data (main wires shadow.Build + the loop).
type ScorecardLoader func(ctx context.Context) (*ScorecardData, error)

// scorecardTTL bounds how often a page view refetches market history.
const scorecardTTL = 10 * time.Minute

// RegisterScorecard mounts GET /scorecard: read-only, no form, nothing to approve.
func RegisterScorecard(mux *http.ServeMux, load ScorecardLoader) {
	h := &scoreHandler{load: load}
	mux.HandleFunc("GET /scorecard", h.page)
}

type scoreHandler struct {
	load ScorecardLoader

	mu    sync.Mutex
	cache *ScorecardData
	at    time.Time
}

func (h *scoreHandler) data(ctx context.Context) (*ScorecardData, time.Time, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cache != nil && time.Since(h.at) < scorecardTTL {
		return h.cache, h.at, nil
	}
	d, err := h.load(ctx)
	if err != nil {
		return nil, time.Time{}, err
	}
	h.cache, h.at = d, time.Now()
	return d, h.at, nil
}

type bookView struct {
	Name, Role      string
	Return, DD      float64
	Trades, Sells   int
	Realized        float64
	Rejected, Marks int
	Started         bool
}

type candView struct {
	Name    string
	Live    bool
	Weights string
	Reasons []string
}

type scoreView struct {
	Live        string
	Since       string
	Days        int
	Note        string
	Books       []bookView
	Candidates  []candView
	AsOf        string
	Unavailable string
	Built       string
	Error       string
}

func (h *scoreHandler) page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	d, at, err := h.data(r.Context())
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = scorePage.Execute(w, scoreView{Error: "Scorecard unavailable: " + err.Error()})
		return
	}
	w.WriteHeader(http.StatusOK)
	_ = scorePage.Execute(w, buildScoreView(d, at))
}

func buildScoreView(d *ScorecardData, at time.Time) scoreView {
	v := scoreView{Live: d.Live, AsOf: d.AsOf, Unavailable: strings.Join(d.Unavailable, ", "),
		Built: at.In(calendar.Toronto).Format("Mon Jan 2 15:04 MST")}
	if c := d.Card; c != nil {
		v.Since, v.Days, v.Note = c.Since, c.Days, c.Note
		for _, r := range c.Rows {
			name := strings.TrimPrefix(r.Book, shadow.Prefix)
			role := r.Role
			switch {
			case r.Role == "live":
				name, role = "your paper book", "live"
			case r.Role == "benchmark":
				name, role = "XIU buy & hold", "benchmark"
			case name == d.Live:
				role = "candidate · live strategy"
			}
			v.Books = append(v.Books, bookView{Name: name, Role: role, Return: r.ReturnPct, DD: r.MaxDDPct,
				Trades: r.Trades, Sells: r.Sells, Realized: r.RealizedPnL, Rejected: r.Rejected, Marks: r.Marks,
				Started: r.StartEquity > 0})
		}
		sort.SliceStable(v.Books, func(i, j int) bool { return v.Books[i].Return > v.Books[j].Return })
	}
	names := make([]string, 0, len(d.Candidates))
	for n := range d.Candidates {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if (names[i] == d.Live) != (names[j] == d.Live) {
			return names[i] == d.Live // the live strategy first
		}
		return names[i] < names[j]
	})
	for _, n := range names {
		dec := d.Candidates[n]
		syms := make([]string, 0, len(dec.Targets))
		for s, w := range dec.Targets {
			if w > 0.0005 {
				syms = append(syms, s)
			}
		}
		sort.Slice(syms, func(i, j int) bool {
			if dec.Targets[syms[i]] != dec.Targets[syms[j]] {
				return dec.Targets[syms[i]] > dec.Targets[syms[j]]
			}
			return syms[i] < syms[j]
		})
		var parts, reasons []string
		for _, s := range syms {
			parts = append(parts, strings.TrimSuffix(s, ".TO")+" "+pct0(dec.Targets[s]))
		}
		rs := make([]string, 0, len(dec.Reasons))
		for s := range dec.Reasons {
			rs = append(rs, s)
		}
		sort.Strings(rs)
		for _, s := range rs {
			reasons = append(reasons, dec.Reasons[s])
		}
		weights := strings.Join(parts, " · ")
		if weights == "" {
			weights = "all cash"
		}
		v.Candidates = append(v.Candidates, candView{Name: n, Live: n == d.Live, Weights: weights, Reasons: reasons})
	}
	return v
}

func pct0(f float64) string { return strconvF(f*100, 0) + "%" }

var scorePage = template.Must(template.New("s").Funcs(template.FuncMap{
	"signed": func(f float64) string {
		s := strconvF(f, 1)
		if f > 0 {
			s = "+" + s
		}
		return s + "%"
	},
	"money": func(f float64) string {
		s := "$" + strconvF(abs(f), 0)
		if f < 0 {
			return "−" + s
		}
		return s
	},
}).Parse(`<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Tulip Scorecard</title>
<style>
:root{--fg:#222;--muted:#666;--line:#ddd;--bg:#fff;--card:#fafafa;--up:#1f7a3f;--down:#b3261e;--badge:#eef}
@media (prefers-color-scheme: dark){:root{--fg:#e8e8e8;--muted:#9a9a9a;--line:#333;--bg:#121212;--card:#1b1b1b;--up:#6fcf8e;--down:#f2a097;--badge:#262640}}
body{font-family:system-ui,sans-serif;max-width:34rem;margin:1.2rem auto;padding:0 1rem;color:var(--fg);background:var(--bg)}
h1{font-size:1.3rem;margin-bottom:.2rem} h2{font-size:1.05rem;margin-top:1.6rem}
.k{color:var(--muted)} .card{border:1px solid var(--line);border-radius:10px;padding:.8rem 1rem;margin:.7rem 0;background:var(--card)}
.name{font-weight:600} .badge{font-size:.75rem;background:var(--badge);border-radius:6px;padding:.1rem .4rem;margin-left:.3rem;color:var(--muted)}
.big{font-size:1.4rem;font-weight:600;font-variant-numeric:tabular-nums} .up{color:var(--up)} .down{color:var(--down)}
.row{display:flex;gap:1rem;flex-wrap:wrap;font-variant-numeric:tabular-nums;color:var(--muted);font-size:.92rem}
details{margin-top:.4rem} summary{cursor:pointer;color:var(--muted)} li{margin:.35rem 0;line-height:1.4}
.note{font-size:.88rem;color:var(--muted);line-height:1.45} .err{background:#fdecec;color:#222;padding:.7rem;border-radius:8px}
</style></head><body>
<h1>🌷 Tulip scorecard</h1>
{{if .Error}}<p class="err">{{.Error}}</p>{{else}}
<div class="k">Live strategy: <b>{{.Live}}</b> · read-only · updated {{.Built}}</div>
{{if .Since}}
<h2>Since {{.Since}} <span class="k">({{.Days}} trading days)</span></h2>
{{range .Books}}
<div class="card">
  <div><span class="name">{{.Name}}</span><span class="badge">{{.Role}}</span></div>
  {{if .Started}}
  <div class="big {{if gt .Return 0.0}}up{{else if lt .Return 0.0}}down{{end}}">{{signed .Return}}</div>
  <div class="row"><span>max drop {{signed .DD}}</span><span>{{.Trades}} trades</span><span>{{.Sells}} sells</span><span>realized {{money .Realized}}</span>{{if .Rejected}}<span>{{.Rejected}} refused by Thorn</span>{{end}}</div>
  {{else}}<div class="k">not started — no position since the common start (approvals off?)</div>{{end}}
</div>
{{end}}
{{else}}
<div class="card note">{{.Note}}</div>
{{end}}
<h2>What each would hold now</h2>
<div class="k">Prices up to {{.AsOf}}{{if .Unavailable}} · unavailable: {{.Unavailable}}{{end}}</div>
{{range .Candidates}}
<div class="card">
  <div><span class="name">{{.Name}}</span>{{if .Live}}<span class="badge">live</span>{{end}}</div>
  <div>{{.Weights}}</div>
  <details><summary>why</summary><ul>{{range .Reasons}}<li>{{.}}</li>{{end}}</ul></details>
</div>
{{end}}
{{if .Since}}<p class="note">{{.Note}}</p>{{end}}
<p class="note">Sells realize taxable gains or losses in a cash account. Promotion needs 12 months of shadow data plus the backtest gate, and you decide it (ADR 0004).</p>
{{end}}
</body></html>`))

func strconvF(f float64, prec int) string { return strconv.FormatFloat(f, 'f', prec, 64) }

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
