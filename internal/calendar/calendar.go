// Package calendar knows when the TSX is open (America/Toronto, 09:30–16:00,
// Mon–Fri minus exchange holidays). Unknown years fail CLOSED: Tulip never
// assumes a session it can't confirm (ADR 0002 §2).
package calendar

import (
	"errors"
	"fmt"
	"time"
	_ "time/tzdata" // distroless has no zoneinfo
)

// Toronto is the exchange time zone.
var Toronto = mustLoad("America/Toronto")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// ErrUnknownYear: the holiday table doesn't cover this date — treat as closed.
var ErrUnknownYear = errors.New("calendar: no TSX holiday table for this year")

// holidays are full-day TSX closures (observed dates). Extend yearly.
var holidays = map[int]map[string]bool{
	2026: set("2026-01-01", "2026-02-16", "2026-04-03", "2026-05-18", "2026-07-01",
		"2026-08-03", "2026-09-07", "2026-10-12", "2026-12-25", "2026-12-28"),
	2027: set("2027-01-01", "2027-02-15", "2027-03-26", "2027-05-24", "2027-07-01",
		"2027-08-02", "2027-09-06", "2027-10-11", "2027-12-27", "2027-12-28"),
}

func set(days ...string) map[string]bool {
	m := make(map[string]bool, len(days))
	for _, d := range days {
		m[d] = true
	}
	return m
}

// IsTradingDay reports whether the Toronto calendar date of t is a session.
func IsTradingDay(t time.Time) (bool, error) {
	d := t.In(Toronto)
	h, ok := holidays[d.Year()]
	if !ok {
		return false, fmt.Errorf("%w: %d", ErrUnknownYear, d.Year())
	}
	if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false, nil
	}
	return !h[d.Format("2006-01-02")], nil
}

// SessionBounds returns the regular session open and close for t's date.
func SessionBounds(t time.Time) (open, close time.Time) {
	d := t.In(Toronto)
	open = time.Date(d.Year(), d.Month(), d.Day(), 9, 30, 0, 0, Toronto)
	close = time.Date(d.Year(), d.Month(), d.Day(), 16, 0, 0, 0, Toronto)
	return open, close
}

// IsOpen reports whether the regular session is open at t.
func IsOpen(t time.Time) (bool, error) {
	ok, err := IsTradingDay(t)
	if err != nil || !ok {
		return false, err
	}
	open, close := SessionBounds(t)
	return !t.Before(open) && t.Before(close), nil
}

// SessionClosed reports whether t's trading day has finished its session —
// i.e. whether a daily bar dated that day is complete.
func SessionClosed(day, now time.Time) bool {
	_, close := SessionBounds(day)
	return !now.Before(close)
}

// SameDay reports whether a and b fall on the same Toronto date.
func SameDay(a, b time.Time) bool {
	return a.In(Toronto).Format("2006-01-02") == b.In(Toronto).Format("2006-01-02")
}
