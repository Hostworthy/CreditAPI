package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/atb00ker/claude-max-to-api/src/internal/keys"
	"github.com/atb00ker/claude-max-to-api/src/internal/logging"
)

// TokenStore holds the OAuth token used to run `claude -p`. A token saved via
// the /login flow (or pasted manually) takes precedence over the
// CLAUDE_CODE_OAUTH_TOKEN environment variable, so an expired env token can be
// replaced from a phone without redeploying.
type TokenStore struct {
	mu       sync.RWMutex
	path     string
	envToken string
	saved    string
	savedAt  time.Time
	version  int64 // bumped whenever the active token changes
}

// TokenLifetime is how long `claude setup-token` tokens are valid.
const TokenLifetime = 365 * 24 * time.Hour

// Version changes every time a token is saved or removed.
func (t *TokenStore) Version() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.version
}

// NewTokenStore loads any token previously saved at path.
func NewTokenStore(path, envToken string) *TokenStore {
	ts := &TokenStore{path: path, envToken: envToken}
	if b, err := os.ReadFile(path); err == nil {
		ts.saved = strings.TrimSpace(string(b))
		if fi, err := os.Stat(path); err == nil {
			ts.savedAt = fi.ModTime()
		}
	}
	return ts
}

// Token returns the active token and where it came from ("login", "env" or "").
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

// SavedAt reports when the saved token was stored.
func (t *TokenStore) SavedAt() time.Time {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.savedAt
}

// Save persists a token with 0600 permissions.
func (t *TokenStore) Save(token string) error {
	token = strings.TrimSpace(token)
	if !strings.HasPrefix(token, "sk-ant-") || len(token) < 40 || strings.ContainsAny(token, " \t\r\n") {
		return errors.New("that does not look like a Claude OAuth token (expected sk-ant-oat01-...)")
	}
	if err := keys.WriteFileAtomic(t.path, []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	t.mu.Lock()
	t.saved = token
	t.savedAt = time.Now()
	t.version++
	t.mu.Unlock()
	return nil
}

// Clear deletes the saved token (the env token, if any, becomes active again).
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

// Hint returns a redacted version of a token for display.
func Hint(token string) string {
	if len(token) < 24 {
		return ""
	}
	return token[:16] + "…" + token[len(token)-4:]
}

// LoginState is the phase of an interactive login.
type LoginState string

const (
	LoginIdle       LoginState = "idle"
	LoginStarting   LoginState = "starting"
	LoginWaitCode   LoginState = "waiting_for_code"
	LoginSubmitting LoginState = "submitting"
	LoginSuccess    LoginState = "success"
	LoginFailed     LoginState = "failed"
)

// LoginStatus is a snapshot of the login flow for the UI.
type LoginStatus struct {
	State     LoginState `json:"state"`
	URL       string     `json:"url,omitempty"`
	Error     string     `json:"error,omitempty"`
	Output    string     `json:"output,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
}

// LoginManager runs `claude setup-token` inside a pseudo-terminal so the OAuth
// URL can be shown in a browser (e.g. on a phone) and the code pasted back.
type LoginManager struct {
	bin     string
	env     func() []string
	tokens  *TokenStore
	timeout time.Duration

	mu        sync.Mutex
	state     LoginState
	url       string
	errMsg    string
	output    []byte
	startedAt time.Time
	ptmx      *os.File
	cmd       *exec.Cmd
	cancel    context.CancelFunc
	gen       int
}

// NewLoginManager creates a login manager. env returns the environment for
// the claude process.
func NewLoginManager(bin string, env func() []string, tokens *TokenStore) *LoginManager {
	return &LoginManager{bin: bin, env: env, tokens: tokens, timeout: 15 * time.Minute, state: LoginIdle}
}

const maxLoginOutput = 256 << 10

// Start launches a new login, cancelling any running one.
func (m *LoginManager) Start() (LoginStatus, error) {
	m.Cancel()

	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	cmd := exec.CommandContext(ctx, m.bin, "setup-token")
	cmd.Env = m.env()
	cmd.Dir = os.TempDir()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	// A very wide terminal keeps the URL and token on a single line.
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: 1000})
	if err != nil {
		cancel()
		return m.Status(), fmt.Errorf("start %s setup-token: %w", m.bin, err)
	}

	m.mu.Lock()
	m.gen++
	gen := m.gen
	m.state = LoginStarting
	m.url, m.errMsg, m.output = "", "", nil
	m.startedAt = time.Now()
	m.ptmx, m.cmd, m.cancel = ptmx, cmd, cancel
	m.mu.Unlock()

	readDone := make(chan struct{})
	slog.Info(logging.Event("login", "setup-token", "started"))
	go func() {
		defer close(readDone)
		m.readLoop(gen, ptmx)
	}()
	go func() {
		err := cmd.Wait()
		// Let the reader drain the final output (which carries the token)
		// before closing the terminal.
		select {
		case <-readDone:
		case <-time.After(3 * time.Second):
		}
		ptmx.Close()
		cancel()
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.gen != gen {
			return
		}
		switch m.state {
		case LoginSuccess, LoginFailed:
		default:
			m.state = LoginFailed
			if ctx.Err() == context.DeadlineExceeded {
				m.errMsg = "Login timed out. Start again."
			} else if m.errMsg == "" {
				m.errMsg = "claude setup-token exited before a token was received"
				if err != nil {
					m.errMsg += " (" + err.Error() + ")"
				}
				m.errMsg += ". Check the output below; if the code was rejected, start again and paste a fresh code."
			}
			slog.Warn(logging.Event("login", "setup-token", "failed"), slog.String("reason", m.errMsg))
		}
		m.ptmx, m.cmd, m.cancel = nil, nil, nil
	}()

	// Wait briefly for the URL so the first response can already include it.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st := m.Status()
		if st.URL != "" || st.State == LoginFailed {
			return st, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return m.Status(), nil
}

func (m *LoginManager) readLoop(gen int, r io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			m.mu.Lock()
			if m.gen != gen {
				m.mu.Unlock()
				return
			}
			m.output = append(m.output, buf[:n]...)
			if len(m.output) > maxLoginOutput {
				m.output = m.output[len(m.output)-maxLoginOutput:]
			}
			raw := string(m.output)
			if m.url == "" {
				if u := ExtractLoginURL(raw); u != "" {
					m.url = u
					if m.state == LoginStarting {
						m.state = LoginWaitCode
					}
				}
			}
			var token string
			if m.state != LoginSuccess {
				token = ExtractToken(raw)
			}
			m.mu.Unlock()
			if token != "" {
				m.finishWithToken(gen, token)
			}
		}
		if err != nil {
			return
		}
	}
}

func (m *LoginManager) finishWithToken(gen int, token string) {
	err := m.tokens.Save(token)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gen != gen {
		return
	}
	if err != nil {
		m.state = LoginFailed
		m.errMsg = "received a token but could not save it: " + err.Error()
	} else {
		m.state = LoginSuccess
		slog.Info(logging.Event("login", "setup-token", "succeeded"))
	}
	// setup-token exits on its own after printing; make sure it does.
	if m.cancel != nil {
		cancel := m.cancel
		time.AfterFunc(2*time.Second, cancel)
	}
}

// SubmitCode types the authorization code into the waiting CLI prompt.
func (m *LoginManager) SubmitCode(code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return errors.New("code is empty")
	}
	if strings.ContainsAny(code, "\r\n\x1b") || len(code) > 4096 {
		return errors.New("code contains invalid characters")
	}
	m.mu.Lock()
	ptmx := m.ptmx
	state := m.state
	m.mu.Unlock()
	if ptmx == nil || (state != LoginWaitCode && state != LoginStarting && state != LoginSubmitting) {
		return errors.New("no login is waiting for a code; start a new login first")
	}
	if _, err := ptmx.Write([]byte(code)); err != nil {
		return err
	}
	// Send Enter separately so the TUI does not treat it as part of a paste.
	time.Sleep(300 * time.Millisecond)
	if _, err := ptmx.Write([]byte("\r")); err != nil {
		return err
	}
	m.mu.Lock()
	if m.state != LoginSuccess && m.state != LoginFailed {
		m.state = LoginSubmitting
	}
	m.mu.Unlock()
	return nil
}

// Cancel stops a running login, if any.
func (m *LoginManager) Cancel() {
	m.mu.Lock()
	cancel := m.cancel
	if m.state != LoginSuccess {
		m.gen++
		m.state = LoginIdle
		m.url, m.errMsg, m.output = "", "", nil
	}
	m.cancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Status returns a snapshot of the login flow.
func (m *LoginManager) Status() LoginStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := LoginStatus{State: m.state, URL: m.url, Error: m.errMsg}
	if m.state != LoginIdle {
		out := TidyOutput(string(m.output))
		out = tokenRe.ReplaceAllStringFunc(out, Hint)
		if len(out) > 6000 {
			out = out[len(out)-6000:]
		}
		st.Output = out
		t := m.startedAt
		st.StartedAt = &t
	}
	return st
}

// TokenPath returns the default location of the saved token.
func TokenPath(dataDir string) string { return filepath.Join(dataDir, "claude-oauth-token") }
