// Package support holds test-only helpers: a full server running on the fake
// Claude CLI, HTTP helpers and image fixtures.
package support

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atb00ker/claude-max-to-api/src/internal/config"
	"github.com/atb00ker/claude-max-to-api/src/internal/logging"
	"github.com/atb00ker/claude-max-to-api/src/internal/server"
	"github.com/atb00ker/claude-max-to-api/src/tests/support/fakeclaude"
)

// AdminPassword is the admin password of every test server.
const AdminPassword = "test-admin-password"

// Options customizes a test server. Zero values mean sensible defaults.
type Options struct {
	EnvToken          string
	MaxConcurrent     int
	QueueTimeout      time.Duration
	StrictModels      bool
	PublicURL         string
	ImageMaxDimension int
	MaxImages         int
	MaxBodyBytes      int64
}

// TestServer is a running server whose claude CLI is the fake (auto mode).
type TestServer struct {
	*httptest.Server
	App *server.Server
	t   *testing.T
}

// NewServer starts a server for t. The test binary must call
// fakeclaude.MaybeRun in TestMain, because it re-executes itself as the CLI.
func NewServer(t *testing.T, opts Options) *TestServer {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeclaude.EnvScenario, "auto")

	logger, _ := logging.NewWithWriter(io.Discard, "error")
	prev := slog.Default()
	slog.SetDefault(logger)

	dir := t.TempDir()
	cfg := &config.Config{
		Host: "127.0.0.1", Port: "0", DataDir: dir, AdminPassword: AdminPassword,
		PublicURL: opts.PublicURL, ClaudeBin: exe, ClaudeHome: filepath.Join(dir, "claude-home"),
		EnvToken: opts.EnvToken, DefaultModel: "sonnet", Models: []string{"opus", "sonnet", "haiku"},
		ModelAliases: map[string]string{"gpt-4o": "opus"}, StrictModels: opts.StrictModels,
		DefaultSystemPrompt: "You are a helpful assistant.", MaxConcurrent: or(opts.MaxConcurrent, 2),
		QueueTimeout: orDur(opts.QueueTimeout, 5*time.Second), RequestTimeout: 30 * time.Second,
		ImageMaxDimension: or(opts.ImageMaxDimension, 1568), MaxImages: or(opts.MaxImages, 20),
		AllowImageURLs: true, MaxBodyBytes: orInt64(opts.MaxBodyBytes, 10<<20), LogLevel: "error",
	}
	app, err := server.New(cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = app.Close()
		slog.SetDefault(prev)
	})
	return &TestServer{Server: ts, App: app, t: t}
}

func or(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func orInt64(v, def int64) int64 {
	if v == 0 {
		return def
	}
	return v
}

func orDur(v, def time.Duration) time.Duration {
	if v == 0 {
		return def
	}
	return v
}

// Do sends a request with a JSON body (nil for none) and decodes a JSON
// response body when there is one.
func (s *TestServer) Do(method, path string, body any, headers map[string]string) (*http.Response, map[string]any) {
	s.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			s.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, s.URL+path, rd)
	if err != nil {
		s.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.Client().Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

// Admin calls the admin API with the admin password as bearer token.
func (s *TestServer) Admin(method, path string, body any) (int, map[string]any) {
	s.t.Helper()
	resp, out := s.Do(method, path, body, map[string]string{"Authorization": "Bearer " + AdminPassword})
	return resp.StatusCode, out
}

// CreateKey creates a virtual API key and returns its secret.
func (s *TestServer) CreateKey(name string) string {
	s.t.Helper()
	code, out := s.Admin("POST", "/admin/api/keys", map[string]any{"name": name})
	if code != http.StatusCreated {
		s.t.Fatalf("create key: %d %v", code, out)
	}
	return out["secret"].(string)
}

// Chat posts to /v1/chat/completions with key.
func (s *TestServer) Chat(key string, body map[string]any) (*http.Response, map[string]any) {
	s.t.Helper()
	return s.Do("POST", "/v1/chat/completions", body, map[string]string{"Authorization": "Bearer " + key})
}

// Stream posts a streaming chat request and returns the SSE data events and
// whether the stream ended with [DONE].
func (s *TestServer) Stream(key string, body map[string]any) (*http.Response, []map[string]any, bool) {
	s.t.Helper()
	body["stream"] = true
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", s.URL+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := s.Client().Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	var events []map[string]any
	done := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			done = true
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			s.t.Fatalf("bad SSE chunk %q", data)
		}
		events = append(events, m)
	}
	return resp, events, done
}

// WaitLogin polls the login state until it equals want.
func (s *TestServer) WaitLogin(want string) map[string]any {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var st map[string]any
	for time.Now().Before(deadline) {
		_, out := s.Admin("GET", "/admin/api/claude", nil)
		st = out["login"].(map[string]any)
		if st["state"] == want {
			return out
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.t.Fatalf("login state %v, want %q", st, want)
	return nil
}

// SignIn runs the whole fake Claude sign-in through the admin API.
func (s *TestServer) SignIn() {
	s.t.Helper()
	if code, out := s.Admin("POST", "/admin/api/claude/login", nil); code != 200 || out["state"] != "waiting_for_code" {
		s.t.Fatalf("start login: %d %v", code, out)
	}
	if code, out := s.Admin("POST", "/admin/api/claude/login/code", map[string]any{"code": fakeclaude.CodePrefix + "-test#state"}); code != 200 {
		s.t.Fatalf("submit code: %d %v", code, out)
	}
	s.WaitLogin("success")
}

// Content extracts choices[0].message.content from a chat response.
func Content(out map[string]any) string {
	choices, _ := out["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	s, _ := msg["content"].(string)
	return s
}

// PNGDataURL returns a solid-colour w×h PNG as a data URL.
func PNGDataURL(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{200, 120, 30, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}
