package desk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// KindReview is Claude's opinion on a pending suggestion, recorded by the
// chat session through MCP (ADR 0002 §1.2, "Claude as reviewer" — session
// mode: no API key in the cluster). A review is advice only: it never
// approves, executes or declines anything. It is journaled next to the rule's
// candidate so the journal can answer "does Claude add anything?".
const KindReview = "claude.review"

// Review is one recorded opinion.
type Review struct {
	SuggestionID string    `json:"suggestion_id"`
	Verdict      string    `json:"verdict"` // agree | disagree | resize
	Qty          int       `json:"qty,omitempty"`
	Confidence   float64   `json:"confidence"`
	Rationale    string    `json:"rationale"`
	By           string    `json:"by"`
	At           time.Time `json:"at"`
}

var validVerdicts = map[string]bool{"agree": true, "disagree": true, "resize": true}

// AddReview records Claude's review of a pending suggestion.
func (d *Desk) AddReview(ctx context.Context, r Review) error {
	if !validVerdicts[r.Verdict] {
		return fmt.Errorf("verdict must be agree, disagree or resize, got %q", r.Verdict)
	}
	if r.Confidence < 0 || r.Confidence > 1 {
		return errors.New("confidence must be between 0 and 1")
	}
	if r.Verdict == "resize" && r.Qty < 1 {
		return errors.New("resize needs qty ≥ 1 (advice only — the suggestion's qty doesn't change)")
	}
	if r.Rationale == "" {
		return errors.New("a review needs a rationale")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.pending(ctx, r.SuggestionID); err != nil {
		return err
	}
	r.At = d.now().UTC()
	_, err := d.J.Append(ctx, KindReview, "", r)
	return err
}

// Reviews returns the reviews of one suggestion, oldest first.
func (d *Desk) Reviews(ctx context.Context, suggestionID string) ([]Review, error) {
	evs, err := d.J.ByKinds(ctx, KindReview)
	if err != nil {
		return nil, err
	}
	var out []Review
	for _, e := range evs {
		var r Review
		if json.Unmarshal(e.Payload, &r) == nil && r.SuggestionID == suggestionID {
			out = append(out, r)
		}
	}
	return out, nil
}
