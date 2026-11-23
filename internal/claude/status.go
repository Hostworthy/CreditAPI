package claude

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Status tracks runtime information shown on the admin dashboard.
type Status struct {
	mu            sync.Mutex
	rateLimit     json.RawMessage
	rateLimitAt   time.Time
	lastError     string
	lastErrorAt   time.Time
	lastSuccessAt time.Time
	completed     int64
	failed        int64
	inFlight      int
	cliVersion    string
}

// StatusSnapshot is the JSON view of Status.
type StatusSnapshot struct {
	CLIVersion    string          `json:"cli_version"`
	InFlight      int             `json:"in_flight"`
	Completed     int64           `json:"completed"`
	Failed        int64           `json:"failed"`
	LastSuccessAt *time.Time      `json:"last_success_at,omitempty"`
	LastError     string          `json:"last_error,omitempty"`
	LastErrorAt   *time.Time      `json:"last_error_at,omitempty"`
	RateLimit     json.RawMessage `json:"rate_limit,omitempty"`
	RateLimitAt   *time.Time      `json:"rate_limit_at,omitempty"`
}

func (s *Status) active(delta int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.inFlight += delta
	s.mu.Unlock()
}

func (s *Status) recordError(msg string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.failed++
	s.lastError = msg
	s.lastErrorAt = time.Now()
	s.mu.Unlock()
}

func (s *Status) recordSuccess() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.completed++
	s.lastSuccessAt = time.Now()
	s.mu.Unlock()
}

func (s *Status) recordRateLimit(info json.RawMessage) {
	if s == nil || len(info) == 0 {
		return
	}
	s.mu.Lock()
	s.rateLimit = append(json.RawMessage(nil), info...)
	s.rateLimitAt = time.Now()
	s.mu.Unlock()
}

// Snapshot returns a copy for JSON encoding.
func (s *Status) Snapshot() StatusSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := StatusSnapshot{
		CLIVersion: s.cliVersion,
		InFlight:   s.inFlight,
		Completed:  s.completed,
		Failed:     s.failed,
		LastError:  s.lastError,
		RateLimit:  s.rateLimit,
	}
	if !s.lastSuccessAt.IsZero() {
		t := s.lastSuccessAt
		snap.LastSuccessAt = &t
	}
	if !s.lastErrorAt.IsZero() {
		t := s.lastErrorAt
		snap.LastErrorAt = &t
	}
	if !s.rateLimitAt.IsZero() {
		t := s.rateLimitAt
		snap.RateLimitAt = &t
	}
	return snap
}

// DetectVersion runs `claude --version` and remembers the answer.
func (s *Status) DetectVersion(bin string, env []string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Env = env
	out, err := cmd.Output()
	v := strings.TrimSpace(string(out))
	if err != nil {
		v = "unavailable (" + err.Error() + ")"
	}
	s.mu.Lock()
	s.cliVersion = v
	s.mu.Unlock()
	return v
}

// EnvBuilder produces environments for claude subprocesses.
type EnvBuilder struct {
	Home   string
	Tokens *TokenStore
	// Drop lists variables never forwarded to claude (the server's own config).
	Drop []string
}

// ForRun returns the environment for `claude -p`, including the OAuth token.
func (e *EnvBuilder) ForRun() []string {
	env := e.base()
	if tok, _ := e.Tokens.Token(); tok != "" {
		env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+tok)
	}
	return env
}

// ForLogin returns the environment for `claude setup-token`.
func (e *EnvBuilder) ForLogin() []string {
	env := e.base()
	return append(env, "TERM=xterm-256color", "BROWSER=/bin/false", "COLUMNS=1000", "LINES=50")
}

func (e *EnvBuilder) base() []string {
	drop := map[string]bool{"HOME": true, "CLAUDE_CODE_OAUTH_TOKEN": true, "TERM": true, "BROWSER": true}
	for _, k := range e.Drop {
		drop[k] = true
	}
	var env []string
	have := map[string]bool{}
	proxied := false
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if drop[k] {
			continue
		}
		have[k] = true
		switch strings.ToUpper(k) {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY":
			if v != "" {
				proxied = true
			}
		case "NO_PROXY":
			// The MCP bridge listens on loopback; never send it via a proxy.
			kv = k + "=" + v + ",127.0.0.1,localhost"
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+e.Home)
	defaults := map[string]string{
		"DISABLE_AUTOUPDATER":                      "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"ENABLE_TOOL_SEARCH":                       "false",
	}
	for k, v := range defaults {
		if !have[k] {
			env = append(env, k+"="+v)
		}
	}
	if proxied && !have["NO_PROXY"] && !have["no_proxy"] {
		env = append(env, "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
	}
	return env
}
