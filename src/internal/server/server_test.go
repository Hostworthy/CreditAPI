package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atb00ker/CreditAPI/src/internal/config"
	"github.com/atb00ker/CreditAPI/src/internal/logging"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakeclaude"
)

func TestMain(m *testing.M) {
	fakeclaude.MaybeRun()
	os.Exit(m.Run())
}

func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	exe, _ := os.Executable()
	dir := t.TempDir()
	return &config.Config{
		Host: "127.0.0.1", Port: freePort(t), DataDir: dir, AdminPassword: "test-admin-password",
		ClaudeBin: exe, ClaudeHome: filepath.Join(dir, "home"), DefaultModel: "sonnet",
		MaxConcurrent: 1, QueueTimeout: time.Second, RequestTimeout: 10 * time.Second, MaxBodyBytes: 1 << 20,
	}
}

func quietLogs(t *testing.T) *strings.Builder {
	t.Helper()
	var buf strings.Builder
	logger, _ := logging.NewWithWriter(&buf, "debug")
	prev := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestRunServesAndShutsDown(t *testing.T) {
	logs := quietLogs(t)
	t.Setenv(fakeclaude.EnvScenario, "auto")
	cfg := testConfig(t)
	s, err := New(cfg, "1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	url := "http://" + cfg.Addr() + "/healthz"
	var resp *http.Response
	for i := 0; i < 100; i++ {
		if resp, err = http.Get(url); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server did not start: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"claude_login":"unknown"`) {
		t.Fatalf("healthz %d %s", resp.StatusCode, body)
	}

	// Start detects the CLI version in the background.
	deadline := time.Now().Add(5 * time.Second)
	for s.Status.Snapshot().CLIVersion == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if v := s.Status.Snapshot().CLIVersion; v != "0.0.0-fake (Claude Code)" {
		t.Fatalf("cli version %q", v)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	for _, event := range []string{"server.start.succeeded", "config.token.missing", "config.public-url.unset", "server.shutdown.started", "http.request.completed"} {
		if !strings.Contains(logs.String(), logging.Service+"."+event) {
			t.Errorf("log event %s missing", event)
		}
	}
	if strings.Contains(logs.String(), cfg.AdminPassword) {
		t.Error("admin password leaked into logs")
	}
}

func TestRunPortInUse(t *testing.T) {
	quietLogs(t)
	cfg := testConfig(t)
	ln, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	s, err := New(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("expected an error when the port is taken")
	}
}

func TestNewFailsOnUnwritableDataDir(t *testing.T) {
	cfg := testConfig(t)
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = filepath.Join(file, "data")
	if _, err := New(cfg, "test"); err == nil {
		t.Fatal("expected an error for a data dir under a file")
	}
}

func TestRequestLoggingMiddleware(t *testing.T) {
	logs := quietLogs(t)
	h := withRequestLogging(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if logging.RequestIDFrom(r.Context()) == "" {
			t.Error("request id missing from context")
		}
		if _, ok := w.(http.Flusher); !ok {
			t.Error("response writer lost http.Flusher")
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("ResponseController flush: %v", err)
		}
		w.WriteHeader(http.StatusTeapot)
		w.WriteHeader(http.StatusOK) // ignored: the first status wins
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models?x=secret", nil))
	out := logs.String()
	if !strings.Contains(out, `"status":418`) || !strings.Contains(out, `"path":"/v1/models"`) || strings.Contains(out, "secret") {
		t.Fatalf("access log %s", out)
	}
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("X-Request-Id not set")
	}
}

func TestResolveRequestID(t *testing.T) {
	if resolveRequestID("abc-123_x.y") != "abc-123_x.y" {
		t.Fatal("valid id should be kept")
	}
	for _, bad := range []string{"", "has space", strings.Repeat("a", 129), "semi;colon"} {
		if id := resolveRequestID(bad); id == bad || len(id) != 24 {
			t.Errorf("resolveRequestID(%q) = %q", bad, id)
		}
	}
}
