// Package approval holds the human side of the gate (ADR 0002 §3): the
// operator approves with a passphrase only they know. The cluster stores a
// PBKDF2 hash of it (TULIP_APPROVAL_PASSPHRASE_HASH), so reading the secret
// does not grant approval — an attacker (or an agent with cluster-admin)
// would have to brute-force the passphrase. Attempts are rate-limited.
package approval

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	iterations = 600_000 // OWASP 2023 guidance for PBKDF2-HMAC-SHA256
	keyLen     = 32
	prefix     = "pbkdf2-sha256"
)

// HashPassphrase returns "pbkdf2-sha256$<iter>$<salt>$<key>" (base64 raw-url).
func HashPassphrase(passphrase string) (string, error) {
	if len(passphrase) < 12 {
		return "", errors.New("passphrase must be at least 12 characters")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, passphrase, salt, iterations, keyLen)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	return fmt.Sprintf("%s$%d$%s$%s", prefix, iterations, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// Verify checks a passphrase against a stored hash in constant time.
func Verify(stored, passphrase string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != prefix {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 100_000 {
		return false
	}
	enc := base64.RawURLEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, passphrase, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// Limiter locks approvals after too many wrong passphrases.
type Limiter struct {
	Max    int           // failures allowed per window (default 5)
	Window time.Duration // default 15m
	Now    func() time.Time

	mu       sync.Mutex
	failures []time.Time
}

func (l *Limiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Limiter) prune() {
	win := l.Window
	if win == 0 {
		win = 15 * time.Minute
	}
	cut := l.now().Add(-win)
	kept := l.failures[:0]
	for _, f := range l.failures {
		if f.After(cut) {
			kept = append(kept, f)
		}
	}
	l.failures = kept
}

// Locked reports whether approvals are temporarily refused.
func (l *Limiter) Locked() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune()
	max := l.Max
	if max == 0 {
		max = 5
	}
	return len(l.failures) >= max
}

// Fail records a wrong passphrase.
func (l *Limiter) Fail() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures = append(l.failures, l.now())
}
