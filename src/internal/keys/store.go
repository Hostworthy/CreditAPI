// Package keys manages virtual API keys that clients use to call the
// OpenAI-compatible endpoints. Only SHA-256 hashes of keys are stored.
package keys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// KeyPrefix is prepended to every generated key so they are easy to spot.
const KeyPrefix = "sk-cmta-"

// Key is a virtual API key record. The secret itself is never stored.
type Key struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Hint         string     `json:"hint"` // e.g. "sk-cmta-Ab3d…x9Qz"
	Hash         string     `json:"hash"`
	CreatedAt    time.Time  `json:"created_at"`
	LastUsedAt   *time.Time `json:"last_used_at,omitempty"`
	Disabled     bool       `json:"disabled"`
	Requests     int64      `json:"requests"`
	InputTokens  int64      `json:"input_tokens"`
	OutputTokens int64      `json:"output_tokens"`
}

// Store is a small JSON-file backed key store safe for concurrent use.
type Store struct {
	mu     sync.Mutex
	path   string
	keys   map[string]*Key // by ID
	byHash map[string]*Key
	dirty  bool
}

type fileFormat struct {
	Keys []*Key `json:"keys"`
}

// Open loads (or creates) the key store at path.
func Open(path string) (*Store, error) {
	s := &Store{path: path, keys: map[string]*Key{}, byHash: map[string]*Key{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f fileFormat
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, k := range f.Keys {
		s.keys[k.ID] = k
		s.byHash[k.Hash] = k
	}
	return s, nil
}

func hashKey(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Create generates a new key and returns the record plus the plaintext secret,
// which is shown to the admin exactly once.
func (s *Store) Create(name string) (*Key, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, "", errors.New("name is required")
	}
	if len(name) > 100 {
		return nil, "", errors.New("name is too long (max 100 characters)")
	}
	secret := KeyPrefix + randomString(32)
	k := &Key{
		ID:        randomHex(6),
		Name:      name,
		Hint:      secret[:len(KeyPrefix)+4] + "…" + secret[len(secret)-4:],
		Hash:      hashKey(secret),
		CreatedAt: time.Now().UTC(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys[k.ID] = k
	s.byHash[k.Hash] = k
	if err := s.saveLocked(); err != nil {
		delete(s.keys, k.ID)
		delete(s.byHash, k.Hash)
		return nil, "", err
	}
	cp := *k
	return &cp, secret, nil
}

// Authenticate returns a copy of the key record matching secret, or nil.
func (s *Store) Authenticate(secret string) *Key {
	if !strings.HasPrefix(secret, KeyPrefix) {
		return nil
	}
	h := hashKey(secret)
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.byHash[h]
	if !ok || k.Disabled {
		return nil
	}
	cp := *k
	return &cp
}

// RecordUsage updates usage counters. Changes are persisted by Flush.
func (s *Store) RecordUsage(id string, inputTokens, outputTokens int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return
	}
	now := time.Now().UTC()
	k.LastUsedAt = &now
	k.Requests++
	k.InputTokens += inputTokens
	k.OutputTokens += outputTokens
	s.dirty = true
}

// List returns copies of all keys sorted by creation time.
func (s *Store) List() []Key {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Key, 0, len(s.keys))
	for _, k := range s.keys {
		cp := *k
		cp.Hash = ""
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Update changes the name and/or disabled flag of a key.
func (s *Store) Update(id string, name *string, disabled *bool) (*Key, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return nil, os.ErrNotExist
	}
	if name != nil {
		n := strings.TrimSpace(*name)
		if n == "" || len(n) > 100 {
			return nil, errors.New("name must be 1-100 characters")
		}
		k.Name = n
	}
	if disabled != nil {
		k.Disabled = *disabled
	}
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	cp := *k
	cp.Hash = ""
	return &cp, nil
}

// Delete removes a key permanently.
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return os.ErrNotExist
	}
	delete(s.keys, id)
	delete(s.byHash, k.Hash)
	return s.saveLocked()
}

// Flush persists pending usage counters, if any.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	return s.saveLocked()
}

func (s *Store) saveLocked() error {
	f := fileFormat{Keys: make([]*Key, 0, len(s.keys))}
	for _, k := range s.keys {
		f.Keys = append(f.Keys, k)
	}
	sort.Slice(f.Keys, func(i, j int) bool { return f.Keys[i].CreatedAt.Before(f.Keys[j].CreatedAt) })
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if err := WriteFileAtomic(s.path, b, 0o600); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// WriteFileAtomic writes data to a temp file in the same directory and renames
// it over path, so readers never observe a partially written file.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
