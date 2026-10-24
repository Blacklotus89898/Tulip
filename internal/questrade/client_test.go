package questrade

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeQuestrade issues rotating single-use refresh tokens like the real login
// server, and serves the account endpoints with doc-shaped JSON.
type fakeQuestrade struct {
	t        *testing.T
	mu       sync.Mutex
	valid    map[string]bool // unspent refresh tokens
	n        int
	refreshs atomic.Int32
	api      *httptest.Server
	login    *httptest.Server
}

func newFake(t *testing.T, first string) *fakeQuestrade {
	f := &fakeQuestrade{t: t, valid: map[string]bool{first: true}}
	f.api = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer access-") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/v1/accounts":
			fmt.Fprint(w, `{"accounts":[{"type":"Margin","number":"26598145","status":"Active","isPrimary":true}],"userId":3000124}`)
		case strings.HasSuffix(r.URL.Path, "/balances"):
			fmt.Fprint(w, `{"perCurrencyBalances":[{"currency":"CAD","cash":2500.5,"marketValue":1000,"totalEquity":3500.5,"buyingPower":2500.5,"isRealTime":false}],"combinedBalances":[]}`)
		case strings.HasSuffix(r.URL.Path, "/positions"):
			fmt.Fprint(w, `{"positions":[{"symbol":"XIU.TO","symbolId":38738,"openQuantity":20,"currentMarketValue":1078,"currentPrice":53.9,"averageEntryPrice":52.1,"closedPnl":0,"openPnl":36}]}`)
		case strings.HasSuffix(r.URL.Path, "/executions"):
			if r.URL.Query().Get("startTime") == "" {
				t.Error("executions must be time-bounded")
			}
			fmt.Fprint(w, `{"executions":[{"symbol":"XIU.TO","symbolId":38738,"quantity":20,"side":"Buy","price":52.1,"id":53817310,"orderId":177106005,"timestamp":"2026-10-26T10:12:29.000000-04:00","commission":0,"totalCost":1042}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	f.login = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.refreshs.Add(1)
		rt := r.URL.Query().Get("refresh_token")
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.valid[rt] {
			w.WriteHeader(http.StatusBadRequest) // spent or unknown — like the real server
			return
		}
		delete(f.valid, rt) // single use
		f.n++
		next := fmt.Sprintf("refresh-%d", f.n)
		f.valid[next] = true
		fmt.Fprintf(w, `{"access_token":"access-%d","token_type":"Bearer","expires_in":1800,"refresh_token":%q,"api_server":%q}`, f.n, next, f.api.URL+"/")
	}))
	t.Cleanup(func() { f.api.Close(); f.login.Close() })
	return f
}

func client(f *fakeQuestrade, dir, bootstrap string) *Client {
	// The API server is TLS (the client refuses a non-https api_server); the
	// test client trusts its certificate and also reaches the http login fake.
	return &Client{LoginURL: f.login.URL, StateDir: dir, Bootstrap: bootstrap, HTTP: f.api.Client()}
}

func TestReadOnlyEndpoints(t *testing.T) {
	f := newFake(t, "boot-1")
	c := client(f, t.TempDir(), "boot-1")
	ctx := context.Background()

	accts, err := c.Accounts(ctx)
	if err != nil || len(accts) != 1 || accts[0].Number != "26598145" {
		t.Fatalf("accounts = (%+v, %v)", accts, err)
	}
	bal, err := c.Balances(ctx, "26598145")
	if err != nil || bal[0].Currency != "CAD" || bal[0].Cash != 2500.5 {
		t.Fatalf("balances = (%+v, %v)", bal, err)
	}
	pos, err := c.Positions(ctx, "26598145")
	if err != nil || pos[0].Symbol != "XIU.TO" || pos[0].OpenQuantity != 20 || pos[0].OpenPnL != 36 {
		t.Fatalf("positions = (%+v, %v)", pos, err)
	}
	ex, err := c.Executions(ctx, "26598145", time.Now().Add(-24*time.Hour), time.Now())
	if err != nil || len(ex) != 1 || ex[0].Side != "Buy" || ex[0].OrderID != 177106005 {
		t.Fatalf("executions = (%+v, %v)", ex, err)
	}
	// The offset is honoured: 10:12:29-04:00 is 14:12:29 UTC.
	if got := ex[0].Timestamp.UTC().Format("15:04:05"); got != "14:12:29" {
		t.Errorf("timestamp UTC = %s", got)
	}
	if n := f.refreshs.Load(); n != 1 {
		t.Errorf("one refresh should serve every call while the access token is valid, got %d", n)
	}
}

func TestRotatedTokenIsPersistedAndReused(t *testing.T) {
	f := newFake(t, "boot-1")
	dir := t.TempDir()
	if _, err := client(f, dir, "boot-1").Accounts(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, tokenFile))
	if err != nil || string(raw) != "refresh-1" {
		t.Fatalf("persisted token = (%q, %v)", raw, err)
	}
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(filepath.Join(dir, tokenFile)); st.Mode().Perm() != 0o600 {
			t.Errorf("token file mode = %v, want 0600", st.Mode().Perm())
		}
	}
	// A restart with the SAME (already spent) sealed bootstrap must use the
	// persisted token, not replay the bootstrap.
	if _, err := client(f, dir, "boot-1").Accounts(context.Background()); err != nil {
		t.Fatalf("restart must continue the chain: %v", err)
	}
	// A NEW sealed value = the operator re-bootstrapped: use it.
	f.mu.Lock()
	f.valid["boot-2"] = true
	f.mu.Unlock()
	c := client(f, dir, "boot-2")
	if _, err := c.Accounts(context.Background()); err != nil {
		t.Fatalf("re-bootstrap: %v", err)
	}
}

func TestConcurrentCallsRefreshOnce(t *testing.T) {
	f := newFake(t, "boot-1")
	c := client(f, t.TempDir(), "boot-1")
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Positions(context.Background(), "26598145")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("a concurrent call failed (two refreshes would spend the token twice): %v", err)
		}
	}
	if n := f.refreshs.Load(); n != 1 {
		t.Errorf("20 concurrent calls must share one refresh, got %d", n)
	}
}

func TestPersistFailureBreaksTheChainLoudly(t *testing.T) {
	f := newFake(t, "boot-1")
	dir := t.TempDir()
	c := client(f, dir, "boot-1")
	c.loaded, c.refresh = true, "boot-1" // skip bootstrap bookkeeping
	c.StateDir = filepath.Join(dir, "does-not-exist")
	if _, err := c.Accounts(context.Background()); !errors.Is(err, ErrBroken) {
		t.Fatalf("an unpersistable rotated token must break the chain, got %v", err)
	}
	if _, err := c.Accounts(context.Background()); !errors.Is(err, ErrBroken) {
		t.Errorf("once broken, stay broken until re-bootstrapped, got %v", err)
	}
}

func TestErrorsNeverCarryTokens(t *testing.T) {
	f := newFake(t, "boot-1")
	c := client(f, t.TempDir(), "spent-secret-token")
	_, err := c.Accounts(context.Background())
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("a spent token must be ErrAuth, got %v", err)
	}
	// Unreachable login server: net/http's error would include the URL with
	// the token in its query string — it must be scrubbed.
	c2 := &Client{LoginURL: "http://127.0.0.1:1/oauth2/token", StateDir: t.TempDir(), Bootstrap: "very-secret-token"}
	_, err = c2.Accounts(context.Background())
	if err == nil || strings.Contains(err.Error(), "very-secret-token") || strings.Contains(err.Error(), "refresh_token") {
		t.Errorf("error leaks token material: %v", err)
	}
	if !(&Client{StateDir: t.TempDir()}).Configured() == false {
		t.Error("no token anywhere → not configured")
	}
}
