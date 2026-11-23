// Package support holds test-only helpers: a full server running on the fake
// Claude and Cursor CLIs, HTTP helpers and image fixtures.
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

	"github.com/atb00ker/CreditAPI/src/internal/config"
	"github.com/atb00ker/CreditAPI/src/internal/logging"
	"github.com/atb00ker/CreditAPI/src/internal/server"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakeclaude"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakecursor"
)

// AdminPassword is the admin password of every test server.
const AdminPassword = "test-admin-password"

// Options customizes a test server. Zero values mean sensible defaults.
type Options struct {
	// EnvToken and CursorKey set CLAUDE_CODE_OAUTH_TOKEN and CURSOR_API_KEY.
	// A first start always creates a "Claude" subscription; CursorKey also
	// creates a "Cursor" one.
	EnvToken          string
	CursorKey         string
	MaxConcurrent     int
	MaxPending        int
	QueueTimeout      time.Duration
	StrictModels      bool
	PublicURL         string
	ImageMaxDimension int
	MaxImages         int
	MaxBodyBytes      int64
	RequestTimeout    time.Duration
}

// TestServer is a running server whose claude and cursor-agent CLIs are the
// fakes (auto mode).
type TestServer struct {
	*httptest.Server
	App *server.Server
	t   *testing.T
}

// NewServer starts a server for t. The test binary must call fakes.MaybeRun
// in TestMain, because it re-executes itself as both CLIs.
func NewServer(t *testing.T, opts Options) *TestServer {
	t.Helper()
	dir := t.TempDir()
	claudeBin, cursorBin := FakeBins(t, filepath.Join(dir, "bin"))
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", opts.EnvToken)
	t.Setenv("CURSOR_API_KEY", opts.CursorKey)

	logger, _ := logging.NewWithWriter(io.Discard, "error")
	prev := slog.Default()
	slog.SetDefault(logger)

	cfg := &config.Config{
		Host: "127.0.0.1", Port: "0", DataDir: filepath.Join(dir, "data"), AdminPassword: AdminPassword,
		PublicURL: opts.PublicURL, ClaudeBin: claudeBin, CursorBin: cursorBin,
		DefaultModel: "sonnet", Models: []string{"opus", "sonnet", "haiku"},
		CursorDefaultModel: "auto", CursorModels: []string{"auto", "gpt-5", "sonnet-4"},
		ModelAliases: map[string]string{"gpt-4o": "opus"}, StrictModels: opts.StrictModels,
		DefaultSystemPrompt: "You are a helpful assistant.", MaxConcurrent: or(opts.MaxConcurrent, 2), MaxPending: or(opts.MaxPending, 8),
		QueueTimeout: orDur(opts.QueueTimeout, 5*time.Second), RequestTimeout: orDur(opts.RequestTimeout, 30*time.Second),
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

// FakeBins makes `claude` and `cursor-agent` symlinks to the test binary in
// dir and switches both fakes to auto mode.
func FakeBins(t *testing.T, dir string) (claudeBin, cursorBin string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	claudeBin, cursorBin = filepath.Join(dir, "claude"), filepath.Join(dir, "cursor-agent")
	for _, link := range []string{claudeBin, cursorBin} {
		if err := os.Symlink(exe, link); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(fakeclaude.EnvScenario, "auto")
	t.Setenv(fakecursor.EnvMode, "auto")
	return claudeBin, cursorBin
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

// CreateKey creates a virtual API key with access to every subscription
// and returns its secret.
func (s *TestServer) CreateKey(name string) string {
	s.t.Helper()
	_, secret := s.CreateKeyFor(name, nil)
	return secret
}

// CreateKeyFor creates a virtual API key that may use the given subscription
// IDs (nil means all of them) and returns its ID and secret.
func (s *TestServer) CreateKeyFor(name string, subs []string) (id, secret string) {
	s.t.Helper()
	body := map[string]any{"name": name}
	if subs != nil {
		body["subscriptions"] = subs
	}
	code, out := s.Admin("POST", "/admin/api/keys", body)
	if code != http.StatusCreated {
		s.t.Fatalf("create key: %d %v", code, out)
	}
	return out["key"].(map[string]any)["id"].(string), out["secret"].(string)
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

// Ask sends one user message with key and returns the status and reply.
func (s *TestServer) Ask(key, model, text string) (int, string, map[string]any) {
	s.t.Helper()
	resp, out := s.Chat(key, map[string]any{"model": model, "messages": []map[string]any{{"role": "user", "content": text}}})
	return resp.StatusCode, Content(out), out
}

// Subscriptions lists the subscriptions through the admin API.
func (s *TestServer) Subscriptions() []map[string]any {
	s.t.Helper()
	code, out := s.Admin("GET", "/admin/api/subscriptions", nil)
	if code != http.StatusOK {
		s.t.Fatalf("list subscriptions: %d %v", code, out)
	}
	var list []map[string]any
	for _, v := range out["subscriptions"].([]any) {
		list = append(list, v.(map[string]any))
	}
	return list
}

// SubscriptionID returns the ID of the subscription with the given name.
func (s *TestServer) SubscriptionID(name string) string {
	s.t.Helper()
	for _, sub := range s.Subscriptions() {
		if sub["name"] == name {
			return sub["id"].(string)
		}
	}
	s.t.Fatalf("no subscription named %q", name)
	return ""
}

// ClaudeID is the ID of the "Claude" subscription created on first start.
func (s *TestServer) ClaudeID() string { return s.SubscriptionID("Claude") }

// CreateSubscription adds a subscription that the given key IDs may use.
func (s *TestServer) CreateSubscription(name, provider string, keyIDs ...string) string {
	s.t.Helper()
	if keyIDs == nil {
		keyIDs = []string{}
	}
	code, out := s.Admin("POST", "/admin/api/subscriptions", map[string]any{"name": name, "provider": provider, "keys": keyIDs})
	if code != http.StatusCreated {
		s.t.Fatalf("create subscription: %d %v", code, out)
	}
	return out["id"].(string)
}

// WaitLogin polls a subscription's login state until it equals want and
// returns the subscription.
func (s *TestServer) WaitLogin(subID, want string) map[string]any {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var st map[string]any
	for time.Now().Before(deadline) {
		_, out := s.Admin("GET", "/admin/api/subscriptions/"+subID, nil)
		st, _ = out["login"].(map[string]any)
		if st["state"] == want {
			return out
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.t.Fatalf("login state %v, want %q", st, want)
	return nil
}

// SignIn runs the whole fake Claude sign-in for the "Claude" subscription.
func (s *TestServer) SignIn() { s.SignInClaude(s.ClaudeID()) }

// SignInClaude runs the fake Claude sign-in (URL, code, token) for subID.
func (s *TestServer) SignInClaude(subID string) {
	s.t.Helper()
	base := "/admin/api/subscriptions/" + subID + "/login"
	if code, out := s.Admin("POST", base, nil); code != 200 || out["state"] != "waiting_for_code" {
		s.t.Fatalf("start login: %d %v", code, out)
	}
	if code, out := s.Admin("POST", base+"/code", map[string]any{"code": fakeclaude.CodePrefix + "-test#state"}); code != 200 {
		s.t.Fatalf("submit code: %d %v", code, out)
	}
	s.WaitLogin(subID, "success")
}

// SignInCursor runs the fake Cursor sign-in (URL, browser approval) for subID.
func (s *TestServer) SignInCursor(subID string) {
	s.t.Helper()
	if code, out := s.Admin("POST", "/admin/api/subscriptions/"+subID+"/login", nil); code != 200 || out["state"] != "waiting_for_browser" {
		s.t.Fatalf("start login: %d %v", code, out)
	}
	s.WaitLogin(subID, "success")
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
