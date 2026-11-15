// Package calendar knows when an exchange is open. Both exchanges Tulip uses
// are on US Eastern time with a 09:30–16:00 regular session; they differ in
// holidays and early closes. Unknown years fail CLOSED: Tulip never assumes a
// session it can't confirm (ADR 0002 §2).
//
// The package-level functions are the TSX calendar (the original desk). The
// US desk (ADR 0006) uses NYSE through the same methods. A nil *Exchange is
// TSX, so code that never sets an exchange behaves exactly as before.
package calendar

import (
	"errors"
	"fmt"
	"time"
	_ "time/tzdata" // distroless has no zoneinfo
)

// Toronto is the exchange time zone. It is the same zone as New York, so it
// also serves as the day key for the NYSE calendar.
var Toronto = mustLoad("America/Toronto")

func mustLoad(name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loc
}

// ErrUnknownYear means the holiday table doesn't cover this date, so the
// date is treated as closed.
var ErrUnknownYear = errors.New("calendar: no holiday table for this year")

// Exchange is one venue's session calendar.
type Exchange struct {
	Name       string
	holidays   map[int]map[string]bool // full-day closures (observed dates); extend yearly
	earlyClose map[string]bool         // sessions that close at 13:00
}

// TSX: Toronto Stock Exchange full-day closures. No early closes are modelled.
var TSX = &Exchange{
	Name: "TSX",
	holidays: map[int]map[string]bool{
		2026: set("2026-01-01", "2026-02-16", "2026-04-03", "2026-05-18", "2026-07-01",
			"2026-08-03", "2026-09-07", "2026-10-12", "2026-12-25", "2026-12-28"),
		2027: set("2027-01-01", "2027-02-15", "2027-03-26", "2027-05-24", "2027-07-01",
			"2027-08-02", "2027-09-06", "2027-10-11", "2027-12-27", "2027-12-28"),
	},
}

// NYSE: holidays and 13:00 early closes, from nyse.com/markets/hours-calendars
// (retrieved 2026-11-15).
var NYSE = &Exchange{
	Name: "NYSE",
	holidays: map[int]map[string]bool{
		2026: set("2026-01-01", "2026-01-19", "2026-02-16", "2026-04-03", "2026-05-25", "2026-06-19",
			"2026-07-03", "2026-09-07", "2026-11-26", "2026-12-25"),
		2027: set("2027-01-01", "2027-01-18", "2027-02-15", "2027-03-26", "2027-05-31", "2027-06-18",
			"2027-07-05", "2027-09-06", "2027-11-25", "2027-12-24"),
	},
	earlyClose: set("2026-11-27", "2026-12-24", "2027-11-26"),
}

// ByName returns the exchange named name ("TSX" or "NYSE").
func ByName(name string) (*Exchange, bool) {
	switch name {
	case "TSX", "":
		return TSX, true
	case "NYSE":
		return NYSE, true
	}
	return nil, false
}

func (x *Exchange) or() *Exchange {
	if x == nil {
		return TSX
	}
	return x
}

func set(days ...string) map[string]bool {
	m := make(map[string]bool, len(days))
	for _, d := range days {
		m[d] = true
	}
	return m
}

// IsTradingDay reports whether t's Eastern calendar date is a session.
func (x *Exchange) IsTradingDay(t time.Time) (bool, error) {
	x = x.or()
	d := t.In(Toronto)
	h, ok := x.holidays[d.Year()]
	if !ok {
		return false, fmt.Errorf("%w: %s %d", ErrUnknownYear, x.Name, d.Year())
	}
	if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false, nil
	}
	return !h[d.Format("2006-01-02")], nil
}

// SessionBounds returns the regular session open and close for t's date.
func (x *Exchange) SessionBounds(t time.Time) (open, close time.Time) {
	x = x.or()
	d := t.In(Toronto)
	open = time.Date(d.Year(), d.Month(), d.Day(), 9, 30, 0, 0, Toronto)
	close = time.Date(d.Year(), d.Month(), d.Day(), 16, 0, 0, 0, Toronto)
	if x.earlyClose[d.Format("2006-01-02")] {
		close = time.Date(d.Year(), d.Month(), d.Day(), 13, 0, 0, 0, Toronto)
	}
	return open, close
}

// IsOpen reports whether the regular session is open at t.
func (x *Exchange) IsOpen(t time.Time) (bool, error) {
	ok, err := x.IsTradingDay(t)
	if err != nil || !ok {
		return false, err
	}
	open, close := x.SessionBounds(t)
	return !t.Before(open) && t.Before(close), nil
}

// SessionClosed reports whether day's session has finished, i.e. whether a
// daily bar dated that day is complete.
func (x *Exchange) SessionClosed(day, now time.Time) bool {
	_, close := x.SessionBounds(day)
	return !now.Before(close)
}

// IsTradingDay is the TSX calendar (see Exchange.IsTradingDay).
func IsTradingDay(t time.Time) (bool, error) { return TSX.IsTradingDay(t) }

// SessionBounds is the TSX calendar (see Exchange.SessionBounds).
func SessionBounds(t time.Time) (open, close time.Time) { return TSX.SessionBounds(t) }

// IsOpen is the TSX calendar (see Exchange.IsOpen).
func IsOpen(t time.Time) (bool, error) { return TSX.IsOpen(t) }

// SessionClosed is the TSX calendar (see Exchange.SessionClosed). Market data
// uses it to drop today's still-forming bar. On an NYSE early close it waits
// until 16:00, which only delays using that bar.
func SessionClosed(day, now time.Time) bool { return TSX.SessionClosed(day, now) }

// SameDay reports whether a and b fall on the same Eastern date.
func SameDay(a, b time.Time) bool {
	return a.In(Toronto).Format("2006-01-02") == b.In(Toronto).Format("2006-01-02")
}
