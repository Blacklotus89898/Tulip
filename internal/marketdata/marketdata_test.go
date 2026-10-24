package marketdata

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/calendar"
)

// chartJSON builds a Yahoo-shaped response; a nil close becomes JSON null.
func chartJSON(days []string, closes []*float64, price float64, quoteTime time.Time) string {
	var ts, c []string
	for i, d := range days {
		t, _ := time.ParseInLocation("2006-01-02 15:04", d+" 09:30", calendar.Toronto)
		ts = append(ts, fmt.Sprint(t.Unix()))
		if closes[i] == nil {
			c = append(c, "null")
		} else {
			c = append(c, fmt.Sprint(*closes[i]))
		}
	}
	series := "[" + strings.Join(c, ",") + "]"
	return fmt.Sprintf(`{"chart":{"result":[{"meta":{"currency":"CAD","regularMarketPrice":%v,"regularMarketTime":%d},
		"timestamp":[%s],"indicators":{"quote":[{"open":%s,"high":%s,"low":%s,"close":%s,"volume":%s}]}}],"error":null}}`,
		price, quoteTime.Unix(), strings.Join(ts, ","), series, series, series, series, series)
}

func f(v float64) *float64 { return &v }

func serve(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("Yahoo requests must send a User-Agent")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDailyDropsTodaysFormingBarAndNulls(t *testing.T) {
	now, _ := time.ParseInLocation("2006-01-02 15:04", "2026-10-26 11:00", calendar.Toronto) // Monday, session open
	body := chartJSON([]string{"2026-10-21", "2026-10-22", "2026-10-23", "2026-10-26"},
		[]*float64{f(54.48), nil, f(53.90), f(54.20)}, 54.2, now)
	y := &Yahoo{Base: serve(t, 200, body).URL, Now: func() time.Time { return now }}

	bars, err := y.Daily(context.Background(), "XIU.TO")
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != 2 || bars[1].Close != 53.90 || bars[1].Day.Format("2006-01-02") != "2026-10-23" {
		t.Fatalf("want 10-21 and 10-23 only (null skipped, today's forming bar dropped): %+v", bars)
	}

	after := now.Add(6 * time.Hour) // 17:00 — today's session closed
	y2 := &Yahoo{Base: serve(t, 200, body).URL, Now: func() time.Time { return after }}
	bars, _ = y2.Daily(context.Background(), "XIU.TO")
	if len(bars) != 3 {
		t.Errorf("after the close, today's bar is complete: %d bars", len(bars))
	}
}

func TestFailsClosed(t *testing.T) {
	for name, srv := range map[string]*httptest.Server{
		"429":   serve(t, 429, "slow down"),
		"empty": serve(t, 200, `{"chart":{"result":[],"error":null}}`),
		"junk":  serve(t, 200, `<html>`),
	} {
		y := &Yahoo{Base: srv.URL}
		if _, err := y.Daily(context.Background(), "XIU.TO"); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: Daily err = %v, want ErrUnavailable", name, err)
		}
		if _, err := y.Quote(context.Background(), "XIU.TO"); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: Quote err = %v, want ErrUnavailable", name, err)
		}
	}
}

func TestQuoteCarriesItsTime(t *testing.T) {
	qt := time.Date(2026, 10, 23, 19, 59, 56, 0, time.UTC)
	body := chartJSON([]string{"2026-10-23"}, []*float64{f(53.9)}, 53.9, qt)
	q, err := (&Yahoo{Base: serve(t, 200, body).URL}).Quote(context.Background(), "XIU.TO")
	if err != nil || q.Price != 53.9 || !q.Time.Equal(qt) || q.Currency != "CAD" {
		t.Fatalf("quote = (%+v, %v)", q, err)
	}
}
