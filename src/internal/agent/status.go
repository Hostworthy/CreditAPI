package agent

import (
	"encoding/json"
	"sync"
	"time"
)

// Status tracks one account's runtime information for the admin pages.
type Status struct {
	mu            sync.Mutex
	rateLimit     json.RawMessage
	rateLimitAt   time.Time
	lastError     string
	lastErrorAt   time.Time
	lastSuccessAt time.Time
	completed     int64
	failed        int64
	cooldownUntil time.Time

	auth AuthInfo
}

// AuthInfo is what we last learned about a login, from a real request or a
// login check.
type AuthInfo struct {
	State     string     `json:"state"` // unknown, valid, login_required
	Reason    string     `json:"reason,omitempty"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
	Source    string     `json:"source,omitempty"` // request or check
	Version   int64      `json:"-"`
}

// RecordAuth stores whether the login worked for a credential version.
func (s *Status) RecordAuth(valid bool, reason string, version int64, source string) {
	now := time.Now()
	info := AuthInfo{State: "valid", CheckedAt: &now, Source: source, Version: version}
	if !valid {
		info.State = "login_required"
		info.Reason = reason
	}
	s.mu.Lock()
	s.auth = info
	s.mu.Unlock()
}

// Auth returns the last known login state for the given credential version.
// Results recorded for an older credential count as unknown.
func (s *Status) Auth(version int64) AuthInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.auth.State == "" || s.auth.Version != version {
		return AuthInfo{State: "unknown"}
	}
	return s.auth
}

// LoginKnownBroken reports whether the login failed recently for this
// credential version.
func (s *Status) LoginKnownBroken(version int64, within time.Duration) (string, bool) {
	a := s.Auth(version)
	if a.State == "login_required" && a.CheckedAt != nil && time.Since(*a.CheckedAt) < within {
		return a.Reason, true
	}
	return "", false
}

// RecordError counts a failed completion.
func (s *Status) RecordError(msg string) {
	s.mu.Lock()
	s.failed++
	s.lastError = msg
	s.lastErrorAt = time.Now()
	s.mu.Unlock()
}

// CoolDown marks the account as resting (usage limit or overload) until t.
func (s *Status) CoolDown(until time.Time) {
	s.mu.Lock()
	if until.After(s.cooldownUntil) {
		s.cooldownUntil = until
	}
	s.mu.Unlock()
}

// CoolingDown reports whether the account is resting.
func (s *Status) CoolingDown() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Now().Before(s.cooldownUntil)
}

// RecordSuccess counts a successful completion.
func (s *Status) RecordSuccess() {
	s.mu.Lock()
	s.completed++
	s.lastSuccessAt = time.Now()
	s.cooldownUntil = time.Time{}
	s.mu.Unlock()
}

// RecordRateLimit keeps the latest usage-window information.
func (s *Status) RecordRateLimit(info json.RawMessage) {
	if len(info) == 0 {
		return
	}
	s.mu.Lock()
	s.rateLimit = append(json.RawMessage(nil), info...)
	s.rateLimitAt = time.Now()
	s.mu.Unlock()
}

// StatusSnapshot is the JSON view of Status.
type StatusSnapshot struct {
	Completed     int64           `json:"completed"`
	Failed        int64           `json:"failed"`
	LastSuccessAt *time.Time      `json:"last_success_at,omitempty"`
	LastError     string          `json:"last_error,omitempty"`
	LastErrorAt   *time.Time      `json:"last_error_at,omitempty"`
	RateLimit     json.RawMessage `json:"rate_limit,omitempty"`
	RateLimitAt   *time.Time      `json:"rate_limit_at,omitempty"`
	CooldownUntil *time.Time      `json:"cooldown_until,omitempty"`
}

// Snapshot returns a copy for JSON encoding.
func (s *Status) Snapshot() StatusSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := StatusSnapshot{Completed: s.completed, Failed: s.failed, LastError: s.lastError, RateLimit: s.rateLimit}
	if !s.lastSuccessAt.IsZero() {
		t := s.lastSuccessAt
		snap.LastSuccessAt = &t
	}
	if !s.lastErrorAt.IsZero() {
		t := s.lastErrorAt
		snap.LastErrorAt = &t
	}
	if time.Now().Before(s.cooldownUntil) {
		t := s.cooldownUntil
		snap.CooldownUntil = &t
	}
	if !s.rateLimitAt.IsZero() {
		t := s.rateLimitAt
		snap.RateLimitAt = &t
	}
	return snap
}
