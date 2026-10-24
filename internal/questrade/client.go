// Package questrade is Tulip's READ-ONLY Questrade client (ADR 0003): accounts,
// balances, positions and executions. Questrade's API does not let retail
// clients place orders, and this package has no code that tries.
//
// Token custody is the hard part: a refresh token is single-use. Refreshes
// are serialised; the new refresh token is persisted (atomic, 0600) BEFORE the
// new access token is used; tokens and the API server URL never appear in
// errors, logs or the journal.
package questrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Errors (never carrying token material).
var (
	ErrNotConfigured = errors.New("questrade: no refresh token (seal one to bootstrap)")
	ErrBroken        = errors.New("questrade: token chain broken — a refreshed token could not be persisted; generate a new token and re-seal it")
	ErrAuth          = errors.New("questrade: authentication failed (token expired, revoked or already used) — generate a new token and re-seal it")
)

// Client talks to one Questrade login server and the API server it hands out.
type Client struct {
	LoginURL  string // default https://login.questrade.com/oauth2/token
	StateDir  string // where the rotated refresh token lives (the PVC), e.g. /data
	Bootstrap string // the sealed refresh token (only used to (re)bootstrap)
	HTTP      *http.Client
	Now       func() time.Time

	mu        sync.Mutex
	refresh   string
	access    string
	apiServer string
	expires   time.Time
	broken    bool
	loaded    bool
	bootHash  string // set while the sealed bootstrap is unproven; recorded after its first successful refresh
}

const (
	tokenFile = "questrade-refresh-token"
	bootFile  = "questrade-bootstrap.sha256"
)

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func hashOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// load picks the refresh token to use: the sealed bootstrap if it changed
// since the last bootstrap (the operator re-sealed), else the persisted one.
func (c *Client) load() error {
	if c.loaded {
		return nil
	}
	c.loaded = true
	persisted, _ := os.ReadFile(filepath.Join(c.StateDir, tokenFile))
	lastBoot, _ := os.ReadFile(filepath.Join(c.StateDir, bootFile))
	switch {
	case c.Bootstrap != "" && hashOf(c.Bootstrap) != strings.TrimSpace(string(lastBoot)):
		// Recorded only once a refresh with it succeeded: a failed first
		// attempt (then a restart) must try the sealed token again.
		c.refresh, c.bootHash = c.Bootstrap, hashOf(c.Bootstrap)
	case len(persisted) > 0:
		c.refresh = strings.TrimSpace(string(persisted))
	default:
		return ErrNotConfigured
	}
	return nil
}

// Configured reports whether a token exists to try (without spending it).
func (c *Client) Configured() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.load() == nil && !c.broken
}

// writeAtomic: temp file in the same dir, fsync, rename, 0600.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after a successful rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// scrub drops anything that could carry a token or the API server URL.
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s request failed: %v", ue.Op, ue.Err)
	}
	return err
}

// apiError extracts only Questrade's documented {"code","message"} from an
// error body — never the raw body, which could echo request material.
func apiError(r io.Reader) string {
	var e struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if json.NewDecoder(io.LimitReader(r, 4096)).Decode(&e) != nil || (e.Code == 0 && e.Message == "") {
		return ""
	}
	msg := e.Message
	if len(msg) > 120 {
		msg = msg[:120]
	}
	return fmt.Sprintf(" (code %d: %s)", e.Code, msg)
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	APIServer    string `json:"api_server"`
}

// ensure returns a valid access token and API server, refreshing if needed.
// Callers hold no lock; ensure serialises refreshes.
func (c *Client) ensure(ctx context.Context, force bool) (string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.broken {
		return "", "", ErrBroken
	}
	if err := c.load(); err != nil {
		return "", "", err
	}
	if !force && c.access != "" && c.now().Before(c.expires.Add(-30*time.Second)) {
		return c.access, c.apiServer, nil
	}

	login := c.LoginURL
	if login == "" {
		login = "https://login.questrade.com/oauth2/token"
	}
	u := login + "?grant_type=refresh_token&refresh_token=" + url.QueryEscape(c.refresh)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", "", scrub(err)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return "", "", scrub(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
		return "", "", ErrAuth
	}
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("questrade: token refresh HTTP %d%s", resp.StatusCode, apiError(resp.Body))
	}
	var tr tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&tr); err != nil {
		return "", "", errors.New("questrade: token refresh: unreadable response")
	}
	if tr.AccessToken == "" || tr.RefreshToken == "" || !strings.HasPrefix(tr.APIServer, "https://") {
		return "", "", errors.New("questrade: token refresh: incomplete response")
	}
	// The old refresh token is spent the moment the server answered. Persist
	// the new one BEFORE using anything — if this fails, the chain is broken.
	if err := writeAtomic(filepath.Join(c.StateDir, tokenFile), []byte(tr.RefreshToken)); err != nil {
		c.broken = true
		return "", "", ErrBroken
	}
	c.refresh = tr.RefreshToken
	if c.bootHash != "" {
		// The bootstrap is proven and spent; from now on the persisted token
		// is the chain. Best effort: on failure it is retried next refresh.
		if writeAtomic(filepath.Join(c.StateDir, bootFile), []byte(c.bootHash)) == nil {
			c.bootHash = ""
		}
	}
	c.access = tr.AccessToken
	c.apiServer = strings.TrimRight(tr.APIServer, "/") + "/"
	c.expires = c.now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return c.access, c.apiServer, nil
}

// get performs an authenticated GET, refreshing once on 401.
func (c *Client) get(ctx context.Context, path string, out any) error {
	for attempt := 0; attempt < 2; attempt++ {
		access, server, err := c.ensure(ctx, attempt > 0)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+strings.TrimLeft(path, "/"), nil)
		if err != nil {
			return scrub(err)
		}
		req.Header.Set("Authorization", "Bearer "+access)
		resp, err := c.http().Do(req)
		if err != nil {
			return scrub(err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			continue // access token expired early: refresh once
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("questrade: %s: HTTP %d%s", strings.SplitN(path, "?", 2)[0], resp.StatusCode, apiError(strings.NewReader(string(body))))
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("questrade: %s: unreadable response", strings.SplitN(path, "?", 2)[0])
		}
		return nil
	}
	return ErrAuth
}

// ---- read-only endpoints ----

// Account is one brokerage account.
type Account struct {
	Type   string `json:"type"`
	Number string `json:"number"`
	Status string `json:"status"`
}

// Accounts lists the accounts the token can see.
func (c *Client) Accounts(ctx context.Context) ([]Account, error) {
	var r struct {
		Accounts []Account `json:"accounts"`
	}
	err := c.get(ctx, "v1/accounts", &r)
	return r.Accounts, err
}

// Balance is one currency's balance.
type Balance struct {
	Currency    string  `json:"currency"`
	Cash        float64 `json:"cash"`
	MarketValue float64 `json:"marketValue"`
	TotalEquity float64 `json:"totalEquity"`
	BuyingPower float64 `json:"buyingPower"`
	IsRealTime  bool    `json:"isRealTime"`
}

// Balances returns per-currency balances for the account.
func (c *Client) Balances(ctx context.Context, account string) ([]Balance, error) {
	var r struct {
		PerCurrencyBalances []Balance `json:"perCurrencyBalances"`
	}
	err := c.get(ctx, "v1/accounts/"+url.PathEscape(account)+"/balances", &r)
	return r.PerCurrencyBalances, err
}

// Position is one holding.
type Position struct {
	Symbol             string  `json:"symbol"`
	SymbolID           int64   `json:"symbolId"`
	OpenQuantity       float64 `json:"openQuantity"`
	CurrentMarketValue float64 `json:"currentMarketValue"`
	CurrentPrice       float64 `json:"currentPrice"`
	AverageEntryPrice  float64 `json:"averageEntryPrice"`
	OpenPnL            float64 `json:"openPnl"`
	ClosedPnL          float64 `json:"closedPnl"`
}

// Positions returns the account's positions.
func (c *Client) Positions(ctx context.Context, account string) ([]Position, error) {
	var r struct {
		Positions []Position `json:"positions"`
	}
	err := c.get(ctx, "v1/accounts/"+url.PathEscape(account)+"/positions", &r)
	return r.Positions, err
}

// Execution is one fill (an order may have several).
type Execution struct {
	Symbol     string    `json:"symbol"`
	SymbolID   int64     `json:"symbolId"`
	Quantity   float64   `json:"quantity"`
	Side       string    `json:"side"` // Buy | Sell | Short | Cov | BTO | STC | ...
	Price      float64   `json:"price"`
	ID         int64     `json:"id"`
	OrderID    int64     `json:"orderId"`
	Timestamp  time.Time `json:"timestamp"` // ISO with offset — parsed as such
	Commission float64   `json:"commission"`
	TotalCost  float64   `json:"totalCost"`
}

// Executions returns fills between start and end.
func (c *Client) Executions(ctx context.Context, account string, start, end time.Time) ([]Execution, error) {
	q := url.Values{}
	q.Set("startTime", start.Format(time.RFC3339))
	q.Set("endTime", end.Format(time.RFC3339))
	var r struct {
		Executions []Execution `json:"executions"`
	}
	err := c.get(ctx, "v1/accounts/"+url.PathEscape(account)+"/executions?"+q.Encode(), &r)
	return r.Executions, err
}
