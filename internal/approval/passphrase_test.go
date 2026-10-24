package approval

import (
	"strings"
	"testing"
	"time"
)

func TestHashAndVerify(t *testing.T) {
	h, err := HashPassphrase("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$600000$") {
		t.Errorf("format = %q", h)
	}
	if !Verify(h, "correct horse battery staple") {
		t.Error("right passphrase must verify")
	}
	for _, wrong := range []string{"", "correct horse battery stapl", "CORRECT HORSE BATTERY STAPLE"} {
		if Verify(h, wrong) {
			t.Errorf("%q must not verify", wrong)
		}
	}
	h2, _ := HashPassphrase("correct horse battery staple")
	if h == h2 {
		t.Error("salts must differ between hashes")
	}
	for _, bad := range []string{"", "plain-text-secret", "pbkdf2-sha256$10$x$y", "md5$1$a$b"} {
		if Verify(bad, "anything") {
			t.Errorf("malformed or weak stored hash %q must never verify", bad)
		}
	}
	if _, err := HashPassphrase("short"); err == nil {
		t.Error("short passphrases are refused")
	}
}

func TestLimiter(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	l := &Limiter{Max: 3, Window: 10 * time.Minute, Now: func() time.Time { return now }}
	for i := 0; i < 3; i++ {
		if l.Locked() {
			t.Fatalf("locked after %d failures", i)
		}
		l.Fail()
	}
	if !l.Locked() {
		t.Fatal("3 failures in the window must lock")
	}
	now = now.Add(11 * time.Minute)
	if l.Locked() {
		t.Error("the lock lifts once failures age out of the window")
	}
}
