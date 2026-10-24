// Package web is the operator's approval page (ADR 0002 §3): the ntfy message
// carries only a link to it — never a usable token — and approving requires the
// operator's passphrase. Declining needs none (it only reduces risk).
package web

import (
	"context"
	"errors"
	"html/template"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/calendar"
	"github.com/Blacklotus89898/Tulip/internal/desk"
)

// Register mounts the pages.
func Register(mux *http.ServeMux, d *desk.Desk) {
	h := &handler{d: d}
	mux.HandleFunc("GET /approve/{id}", h.page)
	mux.HandleFunc("POST /approve/{id}", h.submit)
	mux.HandleFunc("GET /killswitch", h.killPage)
	mux.HandleFunc("POST /killswitch", h.killSubmit)
}

type handler struct{ d *desk.Desk }

func approver(r *http.Request) desk.Approver {
	ip := r.Header.Get("X-Forwarded-For")
	if ip == "" {
		ip, _, _ = net.SplitHostPort(r.RemoteAddr)
	}
	return desk.Approver{IP: strings.TrimSpace(strings.Split(ip, ",")[0]), UserAgent: r.UserAgent()}
}

var page = template.Must(template.New("p").Funcs(template.FuncMap{
	"local": func(t time.Time) string { return t.In(calendar.Toronto).Format("Mon 15:04 MST") },
	"pct":   func(f float64) string { return strconv.FormatFloat(f*100, 'f', -1, 64) },
}).Parse(`<!doctype html><html><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Tulip — {{.Title}}</title>
<style>
body{font-family:system-ui,sans-serif;max-width:32rem;margin:1.5rem auto;padding:0 1rem;color:#222}
h1{font-size:1.3rem} .card{border:1px solid #ddd;border-radius:10px;padding:1rem;margin:1rem 0}
.k{color:#666} .big{font-size:1.6rem;font-weight:600} .msg{padding:.7rem;border-radius:8px;margin:1rem 0}
.ok{background:#e8f6ec} .err{background:#fdecec} input[type=password]{width:100%;padding:.6rem;font-size:1rem;box-sizing:border-box}
button{padding:.7rem 1.2rem;font-size:1rem;border-radius:8px;border:0;margin-right:.5rem}
.approve{background:#1f7a3f;color:#fff} .decline{background:#eee}
small{color:#777}
</style></head><body>
<h1>🌷 Tulip — {{.Title}}</h1>
{{if .Message}}<div class="msg {{if .Error}}err{{else}}ok{{end}}">{{.Message}}</div>{{end}}
{{with .S}}
<div class="card">
  <div class="big">{{.Side}} {{.Qty}} {{.Symbol}}</div>
  <div>at ~{{printf "%.2f" .RefPrice}} (must stay within {{pct .BandPct}}% at execution)</div>
  <p><span class="k">Source:</span> {{.Source}}{{with .Confidence}} · confidence {{printf "%.2f" .}}{{end}}</p>
  <p><span class="k">Why:</span> {{.Rationale}}</p>
  <p><span class="k">Expires:</span> {{local .Expires}} · <span class="k">ID</span> {{.ID}}</p>
</div>
{{if $.Pending}}
<form method="post">
  <input type="hidden" name="hash" value="{{.Hash}}">
  <p><label>Passphrase (approving only)<br><input type="password" name="passphrase" autocomplete="current-password"></label></p>
  <button class="approve" name="action" value="approve">Approve</button>
  <button class="decline" name="action" value="decline">Decline</button>
</form>
<p><small>Paper trading. Approving executes immediately at the live (≈15-min delayed) quote,
only during TSX hours, after Thorn re-checks every limit.</small></p>
{{end}}
{{end}}
{{if .Kill}}
<form method="post">
  <p>The kill switch is <b>{{if .Armed}}ARMED{{else}}off{{end}}</b>.{{if .Armed}} Disarming needs your passphrase.{{end}}</p>
  {{if .Armed}}<p><input type="password" name="passphrase" autocomplete="current-password"></p>
  <button class="approve">Disarm</button>{{end}}
</form>
{{end}}
</body></html>`))

func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

type view struct {
	Title, Message string
	Error, Pending bool
	S              *desk.Suggestion
	Kill, Armed    bool
}

func (h *handler) find(ctx context.Context, id string) (*desk.Suggestion, bool) {
	st, err := h.d.LoadState(ctx)
	if err != nil {
		return nil, false
	}
	for _, s := range st.Pending {
		if s.ID == id {
			return &s, true
		}
	}
	return nil, false
}

func render(w http.ResponseWriter, code int, v view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(code)
	_ = page.Execute(w, v)
}

func (h *handler) page(w http.ResponseWriter, r *http.Request) {
	s, ok := h.find(r.Context(), r.PathValue("id"))
	if !ok {
		render(w, http.StatusNotFound, view{Title: "not pending", Message: "This suggestion is no longer pending (approved, declined, expired or unknown).", Error: true})
		return
	}
	render(w, http.StatusOK, view{Title: "approve?", S: s, Pending: true})
}

func (h *handler) submit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		render(w, http.StatusBadRequest, view{Title: "error", Message: "bad form", Error: true})
		return
	}
	s, _ := h.find(ctx, id)
	switch r.PostForm.Get("action") {
	case "decline":
		if err := h.d.Decline(ctx, id, "operator (web)", "declined on the approval page"); err != nil {
			render(w, http.StatusConflict, view{Title: "decline", Message: err.Error(), Error: true, S: s})
			return
		}
		render(w, http.StatusOK, view{Title: "declined", Message: "Declined — nothing traded.", S: s})
	case "approve":
		fill, err := h.d.Approve(ctx, id, r.PostForm.Get("hash"), r.PostForm.Get("passphrase"), approver(r))
		if err != nil {
			code := http.StatusConflict
			if errors.Is(err, desk.ErrBadPassphrase) || errors.Is(err, desk.ErrLocked) {
				code = http.StatusForbidden
			}
			still, pending := h.find(ctx, id)
			if pending {
				s = still
			}
			render(w, code, view{Title: "not executed", Message: err.Error(), Error: true, S: s, Pending: pending})
			return
		}
		render(w, http.StatusOK, view{Title: "filled", S: s,
			Message: "Paper fill: " + fill.Side + " " + strconv.Itoa(fill.Qty) + " " + fill.Symbol + " at " + num(fill.Price) + " (quote " + num(fill.QuotePrice) + " @ " + fill.QuoteTime + ")"})
	default:
		render(w, http.StatusBadRequest, view{Title: "error", Message: "unknown action", Error: true})
	}
}

func (h *handler) killPage(w http.ResponseWriter, r *http.Request) {
	st, err := h.d.LoadState(r.Context())
	if err != nil {
		render(w, http.StatusInternalServerError, view{Title: "kill switch", Message: err.Error(), Error: true})
		return
	}
	render(w, http.StatusOK, view{Title: "kill switch", Kill: true, Armed: st.KillSwitch, Message: st.KillReason})
}

func (h *handler) killSubmit(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if err := h.d.DisarmKillSwitch(r.Context(), r.PostForm.Get("passphrase"), approver(r)); err != nil {
		render(w, http.StatusForbidden, view{Title: "kill switch", Kill: true, Armed: true, Message: err.Error(), Error: true})
		return
	}
	render(w, http.StatusOK, view{Title: "kill switch", Kill: true, Armed: false, Message: "Disarmed."})
}
