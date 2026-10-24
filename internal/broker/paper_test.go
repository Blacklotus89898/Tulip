package broker

import (
	"errors"
	"testing"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/marketdata"
)

func TestPaperFillSlippageAgainstYou(t *testing.T) {
	q := marketdata.Quote{Symbol: "XIU.TO", Price: 50, Time: time.Date(2026, 10, 26, 15, 0, 0, 0, time.UTC)}
	buy, err := Paper{SlippageBps: 20}.Fill("strategy", "buy", "XIU.TO", 10, q, "sug_1")
	if err != nil || buy.Price != 50.1 || buy.QuotePrice != 50 || buy.QuoteTime != "2026-10-26T15:00:00Z" || buy.SuggestionID != "sug_1" {
		t.Errorf("buy = (%+v, %v)", buy, err)
	}
	sell, _ := Paper{SlippageBps: 20}.Fill("strategy", "sell", "XIU.TO", 10, q, "")
	if sell.Price != 49.9 {
		t.Errorf("sell price = %v, want 49.9", sell.Price)
	}
}

func TestPaperRejectsInvalid(t *testing.T) {
	q := marketdata.Quote{Symbol: "XIU.TO", Price: 50}
	for name, f := range map[string]func() error{
		"zero qty":  func() error { _, e := Paper{}.Fill("s", "buy", "XIU.TO", 0, q, ""); return e },
		"bad side":  func() error { _, e := Paper{}.Fill("s", "short", "XIU.TO", 1, q, ""); return e },
		"wrong sym": func() error { _, e := Paper{}.Fill("s", "buy", "VFV.TO", 1, q, ""); return e },
		"zero price": func() error {
			_, e := Paper{}.Fill("s", "buy", "XIU.TO", 1, marketdata.Quote{Symbol: "XIU.TO"}, "")
			return e
		},
	} {
		if err := f(); !errors.Is(err, ErrInvalidOrder) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
