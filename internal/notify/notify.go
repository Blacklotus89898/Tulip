// Package notify pages the operator about new suggestions through ntfy, on a
// topic of its own. The message holds the terms and a link to the approval
// page — never a credential: the LAN ntfy is unauthenticated, so anything in
// a message is readable by any LAN client (ADR 0002 §3, §4).
package notify

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/calendar"
	"github.com/Blacklotus89898/Tulip/internal/desk"
)

// Ntfy posts to <URL>/<Topic>.
type Ntfy struct {
	URL       string // e.g. http://ntfy.monitoring.svc
	Topic     string // e.g. tulip-approvals
	PublicURL string // base the phone can open, e.g. http://192.168.0.108:30995
	Client    *http.Client
}

// Suggestion sends one approval request. Errors are returned for the caller
// to journal; a failed page never blocks the loop.
func (n Ntfy) Suggestion(ctx context.Context, s desk.Suggestion) error {
	if n.URL == "" || n.Topic == "" {
		return nil
	}
	link := strings.TrimRight(n.PublicURL, "/") + "/approve/" + s.ID
	body := fmt.Sprintf("%s %d %s at ~%.2f (%s)\n%s\nFull justification on the review page. Expires %s.",
		strings.ToUpper(s.Side), s.Qty, s.Symbol, s.RefPrice, s.Source, Summary(s.Rationale),
		s.Expires.In(calendar.Toronto).Format("15:04 MST"))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(n.URL, "/")+"/"+n.Topic, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", fmt.Sprintf("Tulip: %s %d %s?", s.Side, s.Qty, s.Symbol))
	req.Header.Set("Tags", "tulip,chart_with_upwards_trend")
	req.Header.Set("Priority", "4")
	req.Header.Set("Click", link)
	req.Header.Set("Actions", "view, Review, "+link)
	cl := n.Client
	if cl == nil {
		cl = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy: HTTP %d", resp.StatusCode)
	}
	return nil
}

// maxSummary bounds the justification in the push: ntfy turns long bodies
// into attachments, which this server rejects (Homelab decision 2026-10-14).
const maxSummary = 280

// Summary is the push-sized part of a justification: its first line (the
// "Why"), cut at a word boundary. The review page shows all of it.
func Summary(rationale string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(rationale), "\n")
	if r := []rune(line); len(r) > maxSummary {
		cut := string(r[:maxSummary])
		if i := strings.LastIndex(cut, " "); i > maxSummary/2 {
			cut = cut[:i]
		}
		line = cut + "…"
	}
	return line
}

// Message sends an informational push (the monthly scorecard): normal
// priority, a click-through link, no actions.
func (n Ntfy) Message(ctx context.Context, title, body, path string) error {
	if n.URL == "" || n.Topic == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(n.URL, "/")+"/"+n.Topic, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", title)
	req.Header.Set("Tags", "tulip,bar_chart")
	req.Header.Set("Priority", "3")
	if path != "" {
		req.Header.Set("Click", strings.TrimRight(n.PublicURL, "/")+path)
	}
	cl := n.Client
	if cl == nil {
		cl = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy: HTTP %d", resp.StatusCode)
	}
	return nil
}
