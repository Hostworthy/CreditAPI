package agent

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// EnvBuilder produces environments for CLI subprocesses.
type EnvBuilder struct {
	// Drop lists variables never forwarded (the server's own configuration
	// and credentials).
	Drop []string
	// Defaults are set unless the server environment sets them.
	Defaults map[string]string
}

// Base returns the server environment minus dropped variables, with HOME set
// to home and the defaults applied. The MCP bridge listens on loopback, so
// loopback is always excluded from any proxy.
func (e *EnvBuilder) Base(home string) []string {
	// Config locations other than HOME would let subscriptions share CLI
	// state and bypass the per-subscription settings, so they are never
	// forwarded.
	drop := map[string]bool{"HOME": true, "TERM": true, "BROWSER": true,
		"CLAUDE_CONFIG_DIR": true, "CURSOR_CONFIG_DIR": true, "XDG_CONFIG_HOME": true,
		"XDG_DATA_HOME": true, "XDG_STATE_HOME": true, "XDG_CACHE_HOME": true}
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
			kv = k + "=" + v + ",127.0.0.1,localhost"
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+home)
	for k, v := range e.Defaults {
		if !have[k] {
			env = append(env, k+"="+v)
		}
	}
	if proxied && !have["NO_PROXY"] && !have["no_proxy"] {
		env = append(env, "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
	}
	return env
}

// Slots limits how many CLI processes run at once across all subscriptions.
type Slots struct {
	sem      chan struct{}
	mu       sync.Mutex
	inFlight int
}

// NewSlots allows n concurrent processes.
func NewSlots(n int) *Slots { return &Slots{sem: make(chan struct{}, n)} }

// Acquire reserves a slot, waiting up to timeout.
func (s *Slots) Acquire(ctx context.Context, timeout time.Duration) (func(), error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case s.sem <- struct{}{}:
		s.mu.Lock()
		s.inFlight++
		s.mu.Unlock()
		var once sync.Once
		return func() {
			once.Do(func() {
				<-s.sem
				s.mu.Lock()
				s.inFlight--
				s.mu.Unlock()
			})
		}, nil
	case <-t.C:
		return nil, &Error{Kind: KindBusy, Status: 429, Message: "server is busy: all slots are in use, try again shortly (raise MAX_CONCURRENT to allow more)"}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// InFlight reports how many slots are taken.
func (s *Slots) InFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight
}

// Proc is a CLI process in its own process group, killed when its context
// ends.
type Proc struct {
	Cmd    *exec.Cmd
	Stdout io.ReadCloser
	Stderr *TailBuffer
	stop   func() bool
	once   sync.Once
}

// StartProc starts bin with args. The whole process group is killed when ctx
// ends or Kill is called.
func StartProc(ctx context.Context, bin string, args, env []string, dir string, stdin io.Reader) (*Proc, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	p := &Proc{Cmd: cmd, Stdout: stdout, Stderr: &TailBuffer{Max: 8 << 10}}
	cmd.Stderr = p.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p.stop = context.AfterFunc(ctx, p.Kill)
	return p, nil
}

// Kill stops the process group.
func (p *Proc) Kill() {
	p.once.Do(func() { _ = syscall.Kill(-p.Cmd.Process.Pid, syscall.SIGKILL) })
}

// Wait drains stdout and waits for the process to exit.
func (p *Proc) Wait() error {
	_, _ = io.Copy(io.Discard, p.Stdout)
	err := p.Cmd.Wait()
	p.stop()
	return err
}

// CLIVersion runs `bin --version` and returns the trimmed output.
func CLIVersion(ctx context.Context, bin string, env []string) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "unavailable (" + err.Error() + ")"
	}
	return strings.TrimSpace(string(out))
}

// RunRoot returns a private directory (mode 0700) for one provider's
// per-request files, under the system temp directory:
// <tmp>/creditapi-<uid>/<provider>. Keeping providers apart lets the Cursor
// rules deny reads of Claude's prompts and bridge addresses.
func RunRoot(provider string) (string, error) {
	dir := RunRootPath(provider)
	for _, d := range []string{filepath.Dir(dir), dir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return "", err
		}
		if err := os.Chmod(d, 0o700); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// RunRootPath is RunRoot without creating anything.
func RunRootPath(provider string) string {
	return filepath.Join(os.TempDir(), "creditapi-"+strconv.Itoa(os.Getuid()), provider)
}
