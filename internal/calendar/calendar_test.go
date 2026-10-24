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
