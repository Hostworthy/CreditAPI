package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/atb00ker/CreditAPI/src/internal/logging"
)

// LoginSpec describes how a provider's CLI signs in.
type LoginSpec struct {
	Bin  string
	Args []string
	Env  []string
	// NeedsCode means the CLI waits for a code pasted from the browser
	// (Claude). Without it the CLI finishes by itself once the browser
	// sign-in completes (Cursor).
	NeedsCode bool
	// Token extracts a credential the CLI prints; nil when the CLI stores its
	// credentials itself and exiting successfully means signed in.
	Token func(raw string) string
	// Redact hides secrets in output shown to the browser.
	Redact func(s string) string
	// Save stores an extracted token.
	Save func(token string) error
	// Done runs after a successful sign-in.
	Done func()
}

// LoginState is the phase of an interactive login.
type LoginState string

// Login states.
const (
	LoginIdle       LoginState = "idle"
	LoginStarting   LoginState = "starting"
	LoginWaitCode   LoginState = "waiting_for_code"
	LoginWaitBrowse LoginState = "waiting_for_browser"
	LoginSubmitting LoginState = "submitting"
	LoginSuccess    LoginState = "success"
	LoginFailed     LoginState = "failed"
)

// LoginStatus is a snapshot of the login flow for the UI.
type LoginStatus struct {
	Subscription string     `json:"subscription,omitempty"`
	State        LoginState `json:"state"`
	NeedsCode    bool       `json:"needs_code"`
	URL          string     `json:"url,omitempty"`
	Error        string     `json:"error,omitempty"`
	Output       string     `json:"output,omitempty"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
}

// LoginManager runs one CLI sign-in at a time inside a pseudo-terminal so the
// sign-in URL can be opened in any browser (a phone, for example).
type LoginManager struct {
	timeout time.Duration

	mu        sync.Mutex
	sub       string
	spec      LoginSpec
	state     LoginState
	url       string
	errMsg    string
	output    []byte
	startedAt time.Time
	ptmx      *os.File
	cancel    context.CancelFunc
	gen       int
}

// NewLoginManager creates an idle login manager.
func NewLoginManager() *LoginManager {
	return &LoginManager{timeout: 15 * time.Minute, state: LoginIdle}
}

const maxLoginOutput = 256 << 10

// Start launches a sign-in for subscription, cancelling any running one.
func (m *LoginManager) Start(subscription string, spec LoginSpec) (LoginStatus, error) {
	m.cancelCurrent()

	ctx, cancel := context.WithTimeout(context.Background(), m.timeout)
	cmd := exec.CommandContext(ctx, spec.Bin, spec.Args...)
	cmd.Env = spec.Env
	cmd.Dir = os.TempDir()
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	// A very wide terminal keeps URLs and tokens on a single line.
	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: 1000})
	if err != nil {
		cancel()
		return m.Status(subscription), fmt.Errorf("start %s %s: %w", spec.Bin, strings.Join(spec.Args, " "), err)
	}

	m.mu.Lock()
	m.gen++
	gen := m.gen
	m.sub, m.spec = subscription, spec
	m.state = LoginStarting
	m.url, m.errMsg, m.output = "", "", nil
	m.startedAt = time.Now()
	m.ptmx, m.cancel = ptmx, cancel
	m.mu.Unlock()

	slog.Info(logging.Event("login", "cli", "started"), slog.String("subscription", subscription))
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		m.readLoop(gen, ptmx)
	}()
	go func() {
		err := cmd.Wait()
		// Let the reader drain the final output (which may carry the token)
		// before closing the terminal.
		select {
		case <-readDone:
		case <-time.After(3 * time.Second):
		}
		ptmx.Close()
		ctxErr := ctx.Err() // before cancel, which would always set it
		cancel()
		m.finish(gen, err, ctxErr)
	}()

	// Wait briefly for the URL so the first response can already include it.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		st := m.Status(subscription)
		if st.URL != "" || st.State == LoginFailed || st.State == LoginSuccess {
			return st, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return m.Status(subscription), nil
}

func (m *LoginManager) finish(gen int, waitErr, ctxErr error) {
	m.mu.Lock()
	if m.gen != gen {
		m.mu.Unlock()
		return
	}
	spec, sub := m.spec, m.sub
	succeeded := false
	switch m.state {
	case LoginSuccess, LoginFailed:
	default:
		if spec.Token == nil && waitErr == nil && ctxErr == nil {
			m.state = LoginSuccess
			succeeded = true
		} else {
			m.state = LoginFailed
			switch {
			case ctxErr == context.DeadlineExceeded:
				m.errMsg = "Sign-in timed out. Start again."
			case m.errMsg == "":
				m.errMsg = "The sign-in command exited before the sign-in finished"
				if waitErr != nil {
					m.errMsg += " (" + waitErr.Error() + ")"
				}
				m.errMsg += ". Check the output below and start again."
			}
			slog.Warn(logging.Event("login", "cli", "failed"), slog.String("subscription", sub), slog.String("reason", m.errMsg))
		}
	}
	m.ptmx, m.cancel = nil, nil
	m.mu.Unlock()
	if succeeded {
		if spec.Done != nil {
			spec.Done()
		}
		slog.Info(logging.Event("login", "cli", "succeeded"), slog.String("subscription", sub))
	}
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
						m.state = LoginWaitBrowse
						if m.spec.NeedsCode {
							m.state = LoginWaitCode
						}
					}
				}
			}
			var token string
			if m.spec.Token != nil && m.state != LoginSuccess {
				token = m.spec.Token(raw)
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
	m.mu.Lock()
	spec, sub := m.spec, m.sub
	m.mu.Unlock()
	err := errors.New("no place to save the token")
	if spec.Save != nil {
		err = spec.Save(token)
	}
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
		slog.Info(logging.Event("login", "cli", "succeeded"), slog.String("subscription", sub))
		if spec.Done != nil {
			go spec.Done()
		}
	}
	// The CLI exits on its own after printing; make sure it does.
	if m.cancel != nil {
		time.AfterFunc(2*time.Second, m.cancel)
	}
}

// SubmitCode types the authorization code into the waiting CLI prompt.
func (m *LoginManager) SubmitCode(subscription, code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return errors.New("code is empty")
	}
	if strings.ContainsAny(code, "\r\n\x1b") || len(code) > 4096 {
		return errors.New("code contains invalid characters")
	}
	m.mu.Lock()
	ptmx, state, current := m.ptmx, m.state, m.sub
	m.mu.Unlock()
	if current != subscription || ptmx == nil ||
		(state != LoginWaitCode && state != LoginStarting && state != LoginSubmitting) {
		return errors.New("no sign-in is waiting for a code; start a new sign-in first")
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

// Cancel stops the sign-in for subscription, if it is the running one.
func (m *LoginManager) Cancel(subscription string) {
	m.mu.Lock()
	if m.sub != subscription {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.cancelCurrent()
}

// CancelAll stops any running sign-in.
func (m *LoginManager) CancelAll() { m.cancelCurrent() }

func (m *LoginManager) cancelCurrent() {
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

// Status returns the sign-in state for subscription (idle when another
// subscription's sign-in is the current one).
func (m *LoginManager) Status(subscription string) LoginStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sub != subscription {
		return LoginStatus{Subscription: subscription, State: LoginIdle}
	}
	st := LoginStatus{Subscription: subscription, State: m.state, NeedsCode: m.spec.NeedsCode, URL: m.url, Error: m.errMsg}
	if m.state != LoginIdle {
		out := TidyOutput(string(m.output))
		if m.spec.Redact != nil {
			out = m.spec.Redact(out)
		}
		if len(out) > 6000 {
			out = out[len(out)-6000:]
		}
		st.Output = out
		t := m.startedAt
		st.StartedAt = &t
	}
	return st
}
