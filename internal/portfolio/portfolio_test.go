package portfolio

import (
	"encoding/json"
	"testing"

	"github.com/Blacklotus89898/Tulip/internal/journal"
)

func ev(id int64, kind string, v any) journal.Event {
	b, _ := json.Marshal(v)
	return journal.Event{ID: id, Kind: kind, Payload: b}
}

func TestReplay(t *testing.T) {
	events := []journal.Event{
		ev(4, KindFill, Fill{Book: Strategy, Symbol: "XIU.TO", Side: "sell", Qty: 4, Price: 55}),
		ev(1, KindFunded, Funded{Book: Strategy, Cash: 10000}),
		ev(2, KindFunded, Funded{Book: Benchmark, Cash: 10000}),
		ev(3, KindFill, Fill{Book: Strategy, Symbol: "XIU.TO", Side: "buy", Qty: 10, Price: 50, Fee: 1}),
		ev(5, KindFunded, Funded{Book: Strategy, Cash: 99999}), // a second funding is ignored
		ev(6, "candidate", map[string]string{"x": "y"}),        // unrelated kinds ignored
	}
	books, err := Replay(events, Strategy, Benchmark)
	if err != nil {
		t.Fatal(err)
	}
	s := books[Strategy]
	// 10000 - (10*50 + 1) + 4*55 = 9719
	if s.Cash != 9719 || s.Held("XIU.TO") != 6 || s.RealizedPnL != 20 || s.Fills != 2 {
		t.Errorf("strategy = %+v", s)
	}
	if eq := s.Equity(map[string]float64{"XIU.TO": 60}); eq != 9719+6*60 {
		t.Errorf("equity = %v", eq)
	}
	if b := books[Benchmark]; b.Cash != 10000 || len(b.Positions) != 0 {
		t.Errorf("benchmark = %+v", b)
	}
}

func TestReplayRejectsShorting(t *testing.T) {
	_, err := Replay([]journal.Event{
		ev(1, KindFunded, Funded{Book: Strategy, Cash: 1000}),
		ev(2, KindFill, Fill{Book: Strategy, Symbol: "XIU.TO", Side: "sell", Qty: 1, Price: 50}),
	}, Strategy)
	if err == nil {
		t.Error("selling an unheld symbol must be an error (no shorting)")
	}
}
