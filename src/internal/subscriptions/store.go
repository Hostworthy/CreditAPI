// Package subscriptions manages the AI subscriptions requests are answered
// with (for example "Claude (Personal)" or "Cursor (Work)"), which virtual
// API keys may use each of them, their credentials, and how a request picks
// a subscription.
package subscriptions

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/atb00ker/CreditAPI/src/internal/keys"
)

// Subscription is one AI account. Its position in the list is its priority.
type Subscription struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	// Keys are the IDs of the virtual API keys allowed to use it.
	Keys     []string `json:"keys"`
	Disabled bool     `json:"disabled"`
	// EnvCredential names an environment variable that supplies a credential
	// (used by subscriptions created from CLAUDE_CODE_OAUTH_TOKEN or
	// CURSOR_API_KEY).
	EnvCredential string    `json:"env_credential,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug is the name used to pin a request to this subscription, as in
// "claude-personal/sonnet".
func (s Subscription) Slug() string { return Slugify(s.Name) }

// Slugify turns a name such as "Claude (Personal)" into "claude-personal".
func Slugify(name string) string {
	slug := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if slug == "" {
		return "subscription"
	}
	return slug
}

// HasKey reports whether keyID may use the subscription.
func (s Subscription) HasKey(keyID string) bool { return slices.Contains(s.Keys, keyID) }

func (s *Subscription) clone() Subscription {
	cp := *s
	cp.Keys = append([]string{}, s.Keys...)
	return cp
}

// Store keeps subscriptions in a JSON file, in priority order.
type Store struct {
	mu   sync.Mutex
	path string
	subs []*Subscription
}

type fileFormat struct {
	Subscriptions []*Subscription `json:"subscriptions"`
}

// ErrNotFound is returned for an unknown subscription ID.
var ErrNotFound = errors.New("subscription not found")

// OpenStore loads the store; existed reports whether the file was present.
func OpenStore(path string) (st *Store, existed bool, err error) {
	st = &Store{path: path}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var f fileFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, true, fmt.Errorf("parse %s: %w", path, err)
	}
	st.subs = f.Subscriptions
	return st, true, nil
}

func (st *Store) saveLocked() error {
	b, err := json.MarshalIndent(fileFormat{Subscriptions: st.subs}, "", "  ")
	if err != nil {
		return err
	}
	return keys.WriteFileAtomic(st.path, b, 0o600)
}

// List returns all subscriptions in priority order.
func (st *Store) List() []Subscription {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]Subscription, len(st.subs))
	for i, s := range st.subs {
		out[i] = s.clone()
	}
	return out
}

// Get returns one subscription.
func (st *Store) Get(id string) (Subscription, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if i := st.index(id); i >= 0 {
		return st.subs[i].clone(), true
	}
	return Subscription{}, false
}

func (st *Store) index(id string) int {
	for i, s := range st.subs {
		if s.ID == id {
			return i
		}
	}
	return -1
}

func validName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 60 {
		return "", errors.New("name must be 1-60 characters")
	}
	return name, nil
}

func (st *Store) slugTakenLocked(name, exceptID string) bool {
	slug := Slugify(name)
	for _, s := range st.subs {
		if s.ID != exceptID && s.Slug() == slug {
			return true
		}
	}
	return false
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Create adds a subscription at the lowest priority.
func (st *Store) Create(name, provider string, keyIDs []string, envCredential string) (Subscription, error) {
	name, err := validName(name)
	if err != nil {
		return Subscription{}, err
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.slugTakenLocked(name, "") {
		return Subscription{}, fmt.Errorf("a subscription named like %q already exists", name)
	}
	s := &Subscription{ID: newID(), Name: name, Provider: provider, Keys: dedupe(keyIDs),
		EnvCredential: envCredential, CreatedAt: time.Now().UTC()}
	st.subs = append(st.subs, s)
	if err := st.saveLocked(); err != nil {
		st.subs = st.subs[:len(st.subs)-1]
		return Subscription{}, err
	}
	return s.clone(), nil
}

// Update changes the name, enabled state and/or granted keys.
func (st *Store) Update(id string, name *string, disabled *bool, keyIDs *[]string) (Subscription, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	i := st.index(id)
	if i < 0 {
		return Subscription{}, ErrNotFound
	}
	s := st.subs[i]
	prev := s.clone()
	if name != nil {
		n, err := validName(*name)
		if err != nil {
			return Subscription{}, err
		}
		if st.slugTakenLocked(n, id) {
			return Subscription{}, fmt.Errorf("a subscription named like %q already exists", n)
		}
		s.Name = n
	}
	if disabled != nil {
		s.Disabled = *disabled
	}
	if keyIDs != nil {
		s.Keys = dedupe(*keyIDs)
	}
	if err := st.saveLocked(); err != nil {
		*s = prev
		return Subscription{}, err
	}
	return s.clone(), nil
}

// Move changes the priority of a subscription by delta places (negative is
// higher priority).
func (st *Store) Move(id string, delta int) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	i := st.index(id)
	if i < 0 {
		return ErrNotFound
	}
	j := max(0, min(len(st.subs)-1, i+delta))
	if i == j {
		return nil
	}
	s := st.subs[i]
	st.subs = slices.Delete(st.subs, i, i+1)
	st.subs = slices.Insert(st.subs, j, s)
	return st.saveLocked()
}

// Delete removes a subscription.
func (st *Store) Delete(id string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	i := st.index(id)
	if i < 0 {
		return ErrNotFound
	}
	st.subs = slices.Delete(st.subs, i, i+1)
	return st.saveLocked()
}

// SetKeyGrants makes keyID usable with exactly the given subscriptions.
func (st *Store) SetKeyGrants(keyID string, subIDs []string) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, id := range subIDs {
		if st.index(id) < 0 {
			return fmt.Errorf("%w: %s", ErrNotFound, id)
		}
	}
	for _, s := range st.subs {
		has := slices.Contains(s.Keys, keyID)
		want := slices.Contains(subIDs, s.ID)
		switch {
		case want && !has:
			s.Keys = append(s.Keys, keyID)
		case !want && has:
			s.Keys = slices.DeleteFunc(s.Keys, func(k string) bool { return k == keyID })
		}
	}
	return st.saveLocked()
}

// GrantsOf returns the IDs of subscriptions keyID may use, in priority order
// (disabled ones included).
func (st *Store) GrantsOf(keyID string) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []string
	for _, s := range st.subs {
		if slices.Contains(s.Keys, keyID) {
			out = append(out, s.ID)
		}
	}
	return out
}

// ForKey returns the enabled subscriptions keyID may use, in priority order.
func (st *Store) ForKey(keyID string) []Subscription {
	st.mu.Lock()
	defer st.mu.Unlock()
	var out []Subscription
	for _, s := range st.subs {
		if !s.Disabled && slices.Contains(s.Keys, keyID) {
			out = append(out, s.clone())
		}
	}
	return out
}

func dedupe(ids []string) []string {
	out := []string{}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}
