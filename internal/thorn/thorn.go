// Package thorn is Tulip's deterministic risk gate (ADR 0001 §2): plain Go,
// no model, no I/O. It runs when a suggestion is proposed AND again when an
// approved suggestion executes (prices and the book move in between). Every
// rule is evaluated and every failure reported, so the journal shows why.
package thorn

import (
	"fmt"
	"math"
	"strings"
)

// Stage of the check.
type Stage string

const (
	Propose Stage = "propose"
	Execute Stage = "execute"
)

// Config holds the limits. All money in the account currency (CAD).
type Config struct {
	Allowlist         []string // symbols Tulip may trade
	MaxOrderValue     float64  // per order
	MaxPositionWeight float64  // post-trade position value / equity
	// MaxWeightBySymbol overrides MaxPositionWeight per symbol — e.g. the
	// bond ETF a trend filter rotates into may hold up to 100%.
	MaxWeightBySymbol    map[string]float64
	DailyLossCap         float64 // positive number; below -cap, buys are blocked
	ConfidenceFloor      float64 // applies only when a confidence is present
	MaxSuggestionsPerDay int     // propose stage only
}

// DefaultConfig is conservative on purpose.
func DefaultConfig() Config {
	return Config{
		Allowlist:            []string{"XIU.TO", "VFV.TO", "XEF.TO", "ZAG.TO"},
		MaxOrderValue:        2500,
		MaxPositionWeight:    0.25,
		DailyLossCap:         250,
		ConfidenceFloor:      0.6,
		MaxSuggestionsPerDay: 3,
	}
}

// Request is everything Thorn needs to judge one order.
type Request struct {
	Stage            Stage
	Symbol           string
	Side             string // buy | sell
	Qty              int
	Price            float64  // reference (propose) or live quote (execute)
	Held             int      // current whole shares of Symbol
	Cash             float64  // current cash
	Equity           float64  // current total value
	DayPnL           float64  // today's P&L (negative = loss)
	Confidence       *float64 // nil = rules-only (no floor applied)
	SuggestionsToday int
	KillSwitch       bool
}

// Verdict is Thorn's answer.
type Verdict struct {
	Allowed bool     `json:"allowed"`
	Reasons []string `json:"reasons,omitempty"` // every rule that failed
}

// Check evaluates every rule.
func Check(cfg Config, r Request) Verdict {
	var reasons []string
	fail := func(format string, a ...any) { reasons = append(reasons, fmt.Sprintf(format, a...)) }

	if !contains(cfg.Allowlist, r.Symbol) {
		fail("symbol %s is not on the allowlist", r.Symbol)
	}
	if r.Side != "buy" && r.Side != "sell" {
		fail("unknown side %q", r.Side)
	}
	if r.Qty < 1 {
		fail("quantity must be at least 1 whole share")
	}
	if r.Price <= 0 || math.IsNaN(r.Price) {
		fail("no valid price")
	}
	value := float64(r.Qty) * r.Price
	switch r.Side {
	case "buy":
		if value > cfg.MaxOrderValue {
			fail("order value %.2f exceeds the per-order cap %.2f", value, cfg.MaxOrderValue)
		}
		if value > r.Cash {
			fail("order value %.2f exceeds cash %.2f", value, r.Cash)
		}
		capW := cfg.MaxPositionWeight
		for sym, w := range cfg.MaxWeightBySymbol {
			if strings.EqualFold(sym, r.Symbol) {
				capW = w
			}
		}
		if r.Equity > 0 && float64(r.Held+r.Qty)*r.Price/r.Equity > capW {
			fail("position would be %.0f%% of equity (cap %.0f%%)",
				100*float64(r.Held+r.Qty)*r.Price/r.Equity, 100*capW)
		}
		if r.DayPnL < -cfg.DailyLossCap {
			fail("daily loss %.2f is past the cap %.2f — buys are blocked, sells still allowed", -r.DayPnL, cfg.DailyLossCap)
		}
	case "sell":
		if r.Qty > r.Held {
			fail("sell %d exceeds held %d (no shorting)", r.Qty, r.Held)
		}
	}
	if r.Confidence != nil && *r.Confidence < cfg.ConfidenceFloor {
		fail("confidence %.2f is below the floor %.2f", *r.Confidence, cfg.ConfidenceFloor)
	}
	if r.Stage == Propose && r.SuggestionsToday >= cfg.MaxSuggestionsPerDay {
		fail("already %d suggestions today (cap %d)", r.SuggestionsToday, cfg.MaxSuggestionsPerDay)
	}
	// The kill switch is checked last and always — at execution it is the
	// final word (ADR 0002 §3).
	if r.KillSwitch {
		fail("kill switch is armed")
	}
	return Verdict{Allowed: len(reasons) == 0, Reasons: reasons}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
