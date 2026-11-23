package agent

import (
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/atb00ker/CreditAPI/src/internal/fsutil"
	"github.com/atb00ker/CreditAPI/src/internal/logging"
)

// TokenStore holds one account's credential: an OAuth token or API key saved
// from the /login page, or one supplied through an environment variable. A
// saved credential takes precedence, so an expired environment credential can
// be replaced from a phone without redeploying.
type TokenStore struct {
	mu       sync.RWMutex
	path     string
	envToken string
	validate func(string) error
	saved    string
	savedAt  time.Time
	version  int64 // bumped whenever the active credential changes
}

// NewTokenStore loads any credential previously saved at path. validate
// checks credentials passed to Save (nil accepts anything non-empty).
func NewTokenStore(path, envToken string, validate func(string) error) *TokenStore {
	ts := &TokenStore{path: path, envToken: strings.TrimSpace(envToken), validate: validate}
	logging.RegisterSecret(ts.envToken)
	if b, err := os.ReadFile(path); err == nil {
		ts.saved = strings.TrimSpace(string(b))
		logging.RegisterSecret(ts.saved)
		if fi, err := os.Stat(path); err == nil {
			ts.savedAt = fi.ModTime()
		}
	}
	return ts
}

// Version changes every time the credential is saved, removed or replaced
// by a CLI sign-in.
func (t *TokenStore) Version() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.version
}

// Token returns the active credential and where it came from ("login",
// "env" or "").
func (t *TokenStore) Token() (string, string) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.saved != "" {
		return t.saved, "login"
	}
	if t.envToken != "" {
		return t.envToken, "env"
	}
	return "", ""
}

// SavedAt reports when the saved credential was stored.
func (t *TokenStore) SavedAt() time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.savedAt
}

// Save validates and persists a credential with 0600 permissions.
func (t *TokenStore) Save(token string) error {
	token = strings.TrimSpace(token)
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return errors.New("the credential is empty or contains spaces")
	}
	if t.validate != nil {
		if err := t.validate(token); err != nil {
			return err
		}
	}
	logging.RegisterSecret(token)
	if err := fsutil.WriteFileAtomic(t.path, []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	t.mu.Lock()
	t.saved = token
	t.savedAt = time.Now()
	t.version++
	t.mu.Unlock()
	return nil
}

// Touch marks the credential as changed without storing anything, for CLIs
// that keep their own credentials after signing in.
func (t *TokenStore) Touch() {
	t.mu.Lock()
	t.savedAt = time.Now()
	t.version++
	t.mu.Unlock()
}

// Clear deletes the saved credential (the environment one, if any, becomes
// active again).
func (t *TokenStore) Clear() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.saved = ""
	t.savedAt = time.Time{}
	t.version++
	if err := os.Remove(t.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Hint returns a redacted version of a credential for display.
func Hint(token string) string {
	if len(token) < 24 {
		return ""
	}
	return token[:16] + "…" + token[len(token)-4:]
}
