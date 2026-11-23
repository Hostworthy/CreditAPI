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
	"github.com/atb00ker/CreditAPI/src/tests/support/fakecursor"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakes"
)

func TestMain(m *testing.M) {
	fakes.MaybeRun()
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

// testConfig points both CLIs at the test binary (through symlinks whose
// names pick the fake) in a fresh data directory.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	exe, _ := os.Executable()
	dir := t.TempDir()
	bins := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bins, 0o755)
	for _, name := range []string{"claude", "cursor-agent"} {
		if err := os.Symlink(exe, filepath.Join(bins, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(fakeclaude.EnvScenario, "auto")
	t.Setenv(fakecursor.EnvMode, "auto")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("CURSOR_API_KEY", "")
	return &config.Config{
		Host: "127.0.0.1", Port: freePort(t), DataDir: filepath.Join(dir, "data"), AdminPassword: "test-admin-password",
		ClaudeBin: filepath.Join(bins, "claude"), CursorBin: filepath.Join(bins, "cursor-agent"),
		DefaultModel: "sonnet", Models: []string{"sonnet", "haiku"}, CursorDefaultModel: "auto", CursorModels: []string{"auto"},
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
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+cfg.AdminPassword)
		if resp, err = http.DefaultClient.Do(req); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("server did not start: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"logins_ok":true`) ||
		!strings.Contains(string(body), `"name":"Claude","provider":"claude","login":"unknown"`) {
		t.Fatalf("healthz %d %s", resp.StatusCode, body)
	}

	// Start detects the CLI versions in the background.
	deadline := time.Now().Add(5 * time.Second)
	for (s.Subs.Version("claude") == "" || s.Subs.Version("cursor") == "") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if v := s.Subs.Version("claude"); v != "0.0.0-fake (Claude Code)" {
		t.Fatalf("claude version %q", v)
	}
	if v := s.Subs.Version("cursor"); v != fakecursor.Version {
		t.Fatalf("cursor version %q", v)
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
	for _, event := range []string{"server.start.succeeded", "config.credential.missing", "config.public-url.unset", "server.shutdown.started", "http.request.completed"} {
		if !strings.Contains(logs.String(), logging.Service+"."+event) {
			t.Errorf("log event %s missing", event)
		}
	}
	if strings.Contains(logs.String(), cfg.AdminPassword) {
		t.Error("admin password leaked into logs")
	}
}

func TestFirstStartMigratesSingleSubscriptionInstall(t *testing.T) {
	quietLogs(t)
	cfg := testConfig(t)
	t.Setenv("CURSOR_API_KEY", fakecursor.ValidKey)
	_ = os.MkdirAll(filepath.Join(cfg.DataDir, "claude-home", ".claude"), 0o700)
	_ = os.WriteFile(filepath.Join(cfg.DataDir, "claude-oauth-token"), []byte(fakeclaude.ValidToken+"\n"), 0o600)
	_ = os.WriteFile(filepath.Join(cfg.DataDir, "keys.json"),
		[]byte(`{"keys":[{"id":"k1","name":"old","hash":"x","hint":"sk-credit-…abcd","created_at":"2026-01-01T00:00:00Z"}]}`), 0o600)
	s, err := New(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	list := s.Subs.Store.List()
	if len(list) != 2 || list[0].Name != "Claude" || list[1].Name != "Cursor" {
		t.Fatalf("subscriptions %+v", list)
	}
	for _, sub := range list {
		if !sub.HasKey("k1") {
			t.Fatalf("existing key not granted %+v", sub)
		}
	}
	acc, _, _ := s.Subs.Account(list[0])
	if tok, src := acc.Tokens.Token(); tok != fakeclaude.ValidToken || src != "login" {
		t.Fatalf("token %q %q", tok, src)
	}
	cur, _, _ := s.Subs.Account(list[1])
	if tok, src := cur.Tokens.Token(); tok != fakecursor.ValidKey || src != "env" {
		t.Fatalf("cursor key %q %q", tok, src)
	}

	// A second start keeps the subscriptions as they are.
	_ = s.Close()
	again, err := New(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if n := len(again.Subs.Store.List()); n != 2 {
		t.Fatalf("subscriptions after restart: %d", n)
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
