package calendar

import (
	"errors"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, Toronto)
	if err != nil {
		panic(err)
	}
	return t
}

func TestIsOpen(t *testing.T) {
	cases := []struct {
		when string
		open bool
	}{
		{"2026-10-26 10:00", true},  // Monday mid-session
		{"2026-10-26 09:29", false}, // before the open
		{"2026-10-26 16:00", false}, // close is exclusive
		{"2026-10-24 12:00", false}, // Saturday
		{"2026-10-12 12:00", false}, // Thanksgiving
		{"2026-12-28 12:00", false}, // Boxing Day observed
	}
	for _, c := range cases {
		got, err := IsOpen(at(c.when))
		if err != nil || got != c.open {
			t.Errorf("IsOpen(%s) = (%v, %v), want %v", c.when, got, err, c.open)
		}
	}
}

func TestUnknownYearFailsClosed(t *testing.T) {
	open, err := IsOpen(at("2030-03-04 11:00"))
	if open || !errors.Is(err, ErrUnknownYear) {
		t.Errorf("unknown year must be closed with ErrUnknownYear, got (%v, %v)", open, err)
	}
}

func TestSessionClosed(t *testing.T) {
	day := at("2026-10-26 00:00")
	if SessionClosed(day, at("2026-10-26 15:59")) {
		t.Error("15:59: today's bar is still forming")
	}
	if !SessionClosed(day, at("2026-10-26 16:00")) {
		t.Error("16:00: today's bar is complete")
	}
}

// The US desk (ADR 0006) must trade when only the TSX is closed, and stand
// aside when only NYSE is.
func TestNYSEDiffersFromTSX(t *testing.T) {
	cases := []struct {
		when      string
		tsx, nyse bool
	}{
		{"2026-07-01 11:00", false, true},  // Canada Day
		{"2026-05-18 11:00", false, true},  // Victoria Day
		{"2026-10-12 11:00", false, true},  // Thanksgiving (Canada)
		{"2026-11-26 11:00", true, false},  // Thanksgiving (US)
		{"2026-07-03 11:00", true, false},  // Independence Day observed
		{"2026-06-19 11:00", true, false},  // Juneteenth
		{"2026-04-03 11:00", false, false}, // Good Friday: both closed
		{"2026-11-16 11:00", true, true},
	}
	for _, c := range cases {
		tsx, err1 := TSX.IsOpen(at(c.when))
		nyse, err2 := NYSE.IsOpen(at(c.when))
		if err1 != nil || err2 != nil || tsx != c.tsx || nyse != c.nyse {
			t.Errorf("%s: TSX %v NYSE %v (errs %v %v), want TSX %v NYSE %v", c.when, tsx, nyse, err1, err2, c.tsx, c.nyse)
		}
	}
}

func TestNYSEEarlyClose(t *testing.T) {
	if open, _ := NYSE.IsOpen(at("2026-11-27 12:59")); !open {
		t.Error("NYSE should be open at 12:59 on the day after Thanksgiving")
	}
	if open, _ := NYSE.IsOpen(at("2026-11-27 13:00")); open {
		t.Error("NYSE closes at 13:00 on the day after Thanksgiving")
	}
	if !NYSE.SessionClosed(at("2026-12-24 00:00"), at("2026-12-24 13:00")) {
		t.Error("the 2026-12-24 NYSE bar is complete at 13:00")
	}
	if open, _ := TSX.IsOpen(at("2026-11-27 14:00")); !open {
		t.Error("TSX has no early close modelled on 2026-11-27")
	}
}

func TestNilExchangeIsTSX(t *testing.T) {
	var x *Exchange
	for _, w := range []string{"2026-07-01 11:00", "2026-11-26 11:00", "2026-10-26 10:00"} {
		a, _ := x.IsOpen(at(w))
		b, _ := IsOpen(at(w))
		if a != b {
			t.Errorf("nil exchange disagrees with TSX at %s", w)
		}
	}
	if _, err := NYSE.IsOpen(at("2030-03-04 11:00")); !errors.Is(err, ErrUnknownYear) {
		t.Errorf("NYSE unknown year must fail closed, got %v", err)
	}
	if x, ok := ByName("NYSE"); !ok || x != NYSE {
		t.Error("ByName(NYSE)")
	}
	if _, ok := ByName("LSE"); ok {
		t.Error("ByName must refuse unknown exchanges")
	}
}
