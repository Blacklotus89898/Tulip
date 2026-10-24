package main

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Blacklotus89898/Tulip/internal/calendar"
	"github.com/Blacklotus89898/Tulip/internal/desk"
	"github.com/Blacklotus89898/Tulip/internal/journal"
	"github.com/Blacklotus89898/Tulip/internal/questrade"
)

// questradeAccount adapts the read-only Questrade client to desk.AccountSource
// for ONE account (ADR 0003 §2) and tracks broker auth health.
type questradeAccount struct {
	c      *questrade.Client
	number string
	j      *journal.Journal

	authOK   atomic.Int32 // 1 ok, 0 failing, -1 not tried yet
	lastOK   atomic.Int64 // unix seconds of the last successful call
	lastErr  atomic.Int64 // unix seconds of the last journaled broker.error
	log      *slog.Logger
	mu       sync.Mutex
	verified bool
}

func newQuestradeAccount(c *questrade.Client, number string, j *journal.Journal) *questradeAccount {
	a := &questradeAccount{c: c, number: number, j: j}
	a.authOK.Store(-1)
	return a
}

// note records auth health; the journal gets transitions only, never tokens.
func (a *questradeAccount) note(ctx context.Context, err error) {
	authErr := errors.Is(err, questrade.ErrAuth) || errors.Is(err, questrade.ErrBroken) || errors.Is(err, questrade.ErrNotConfigured)
	switch {
	case err == nil:
		a.lastOK.Store(time.Now().Unix())
		if a.authOK.Swap(1) != 1 {
			_, _ = a.j.Append(ctx, "broker.auth_ok", "", map[string]string{"broker": "questrade"})
		}
	case authErr:
		if a.authOK.Swap(0) != 0 {
			_, _ = a.j.Append(ctx, "broker.auth_failed", "", map[string]string{"broker": "questrade", "error": err.Error()})
		}
	default:
		// Not auth: network, HTTP 5xx, bad response. Errors are already
		// scrubbed of token material. Journaled at most every 30 min.
		if a.log != nil {
			a.log.Warn("questrade call failed", "err", err)
		}
		if now := time.Now().Unix(); now-a.lastErr.Load() > 1800 {
			a.lastErr.Store(now)
			_, _ = a.j.Append(ctx, "broker.error", "", map[string]string{"broker": "questrade", "error": err.Error()})
		}
	}
}

// verify checks that the configured account is visible to the token (once
// it is, it stays verified; other accounts are never journaled).
func (a *questradeAccount) verify(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.verified {
		return nil
	}
	accts, err := a.c.Accounts(ctx)
	if err != nil {
		return err
	}
	for _, ac := range accts {
		if ac.Number == a.number {
			a.verified = true
			return nil
		}
	}
	return errors.New("questrade: TULIP_QUESTRADE_ACCOUNT is not visible to this token")
}

// Snapshot is the account in CAD: cash and equity from the CAD balance,
// whole-share holdings from positions.
func (a *questradeAccount) Snapshot(ctx context.Context) (*desk.Snapshot, error) {
	snap, err := a.snapshot(ctx)
	a.note(ctx, err)
	return snap, err
}

func (a *questradeAccount) snapshot(ctx context.Context) (*desk.Snapshot, error) {
	if err := a.verify(ctx); err != nil {
		return nil, err
	}
	bals, err := a.c.Balances(ctx, a.number)
	if err != nil {
		return nil, err
	}
	snap := &desk.Snapshot{Held: map[string]int{}, At: time.Now().UTC()}
	found := false
	for _, b := range bals {
		if b.Currency == "CAD" {
			snap.Cash, snap.Equity, found = b.Cash, b.TotalEquity, true
		}
	}
	if !found {
		return nil, errors.New("questrade: no CAD balance on the account")
	}
	pos, err := a.c.Positions(ctx, a.number)
	if err != nil {
		return nil, err
	}
	for _, p := range pos {
		if q := int(math.Floor(p.OpenQuantity)); q > 0 {
			snap.Held[strings.ToUpper(p.Symbol)] = q
		}
	}
	return snap, nil
}

// AuthGauge is tulip_broker_auth_ok: 1 ok, 0 failing, -1 not tried yet.
func (a *questradeAccount) AuthGauge() int32 { return a.authOK.Load() }

// reconcileLoop polls today's executions on trading days, from the open until
// an hour after the close, and hands them to the desk (idempotent).
func reconcileLoop(ctx context.Context, d *desk.Desk, a *questradeAccount, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now().In(calendar.Toronto)
		open, close := calendar.SessionBounds(now)
		trading, err := calendar.IsTradingDay(now)
		if err != nil || !trading || now.Before(open) || now.After(close.Add(time.Hour)) {
			// Off hours: keep the token chain alive (an unused refresh token
			// dies after ~7 days) and the auth gauge current, once a day.
			if time.Since(time.Unix(a.lastOK.Load(), 0)) > 20*time.Hour {
				_, kerr := a.c.Accounts(ctx)
				a.note(ctx, kerr)
			}
			continue
		}
		if n, err := reconcileOnce(ctx, d, a, now); err != nil {
			log.Warn("reconcile failed", "err", err)
		} else if n > 0 {
			log.Info("reconciled", "orders", n)
		}
	}
}

func reconcileOnce(ctx context.Context, d *desk.Desk, a *questradeAccount, now time.Time) (int, error) {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, calendar.Toronto)
	execs, err := a.c.Executions(ctx, a.number, day, now)
	a.note(ctx, err)
	if err != nil {
		return 0, err
	}
	out := make([]desk.Execution, 0, len(execs))
	for _, e := range execs {
		out = append(out, desk.Execution{
			ID: strconv.FormatInt(e.ID, 10), OrderID: strconv.FormatInt(e.OrderID, 10),
			Symbol: strings.ToUpper(e.Symbol), Side: strings.ToLower(e.Side), // Buy/Sell → buy/sell; others never match
			Qty: e.Quantity, Price: e.Price, Fee: e.Commission, At: e.Timestamp,
		})
	}
	return d.Reconcile(ctx, out)
}
