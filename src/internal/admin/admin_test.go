package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atb00ker/CreditAPI/src/internal/claude"
	"github.com/atb00ker/CreditAPI/src/internal/config"
	"github.com/atb00ker/CreditAPI/src/internal/keys"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakeclaude"
)

const pw = "correct horse battery"

func TestMain(m *testing.M) {
	fakeclaude.MaybeRun()
	os.Exit(m.Run())
}

func newServer(t *testing.T) *httptest.Server {
	return newServerWith(t, "text")
}

func newServerWith(t *testing.T, scenario string) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{DataDir: dir, AdminPassword: pw, DefaultModel: "sonnet", Models: []string{"sonnet"}, MaxConcurrent: 1}
	ks, err := keys.Open(filepath.Join(dir, "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	tokens := claude.NewTokenStore(filepath.Join(dir, "tok"), "")
	exe, _ := os.Executable()
	runner := claude.NewRunner(1)
	runner.Status = &claude.Status{}
	runner.Bin, runner.WorkDir, runner.Tokens = exe, dir, tokens
	runner.Env = func() []string { return append(os.Environ(), fakeclaude.EnvScenario+"="+scenario) }
	login := claude.NewLoginManager("false", func() []string { return nil }, tokens)
	a, err := New(cfg, ks, tokens, login, runner, runner.Status)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func call(t *testing.T, c *http.Client, method, url string, body any, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestSessionAndKeys(t *testing.T) {
	srv := newServer(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	if code, _ := call(t, c, "GET", srv.URL+"/admin/api/keys", nil, nil); code != 401 {
		t.Fatalf("unauthenticated list: %d", code)
	}
	if code, _ := call(t, c, "POST", srv.URL+"/admin/api/session", map[string]string{"password": "nope"}, nil); code != 401 {
		t.Fatalf("bad password: %d", code)
	}
	if code, _ := call(t, c, "POST", srv.URL+"/admin/api/session", map[string]string{"password": pw}, nil); code != 200 {
		t.Fatalf("sign in: %d", code)
	}
	if _, out := call(t, c, "GET", srv.URL+"/admin/api/me", nil, nil); out["authenticated"] != true {
		t.Fatalf("me: %v", out)
	}
	code, out := call(t, c, "POST", srv.URL+"/admin/api/keys", map[string]string{"name": "my-app"}, nil)
	if code != 201 || !strings.HasPrefix(out["secret"].(string), keys.KeyPrefix) {
		t.Fatalf("create: %d %v", code, out)
	}
	id := out["key"].(map[string]any)["id"].(string)

	// Cross-site request carrying the cookie is refused.
	if code, _ := call(t, c, "DELETE", srv.URL+"/admin/api/keys/"+id, nil, map[string]string{"Origin": "https://evil.example"}); code != 403 {
		t.Fatalf("cross-origin delete: %d", code)
	}
	if code, _ := call(t, c, "PATCH", srv.URL+"/admin/api/keys/"+id, map[string]any{"disabled": true}, nil); code != 200 {
		t.Fatalf("disable: %d", code)
	}
	if code, _ := call(t, c, "DELETE", srv.URL+"/admin/api/keys/"+id, nil, nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}

	// Bearer admin password works without a cookie.
	if code, _ := call(t, http.DefaultClient, "GET", srv.URL+"/admin/api/claude", nil, map[string]string{"Authorization": "Bearer " + pw}); code != 200 {
		t.Fatalf("bearer: %d", code)
	}
	if code, _ := call(t, http.DefaultClient, "PUT", srv.URL+"/admin/api/claude/token", map[string]string{"token": "bad"}, map[string]string{"Authorization": "Bearer " + pw}); code != 400 {
		t.Fatalf("bad token should be rejected: %d", code)
	}

	call(t, c, "DELETE", srv.URL+"/admin/api/session", nil, nil)
	if code, _ := call(t, c, "GET", srv.URL+"/admin/api/keys", nil, nil); code != 401 {
		t.Fatalf("after sign out: %d", code)
	}
}

func TestPagesAndHeaders(t *testing.T) {
	srv := newServer(t)
	for _, p := range []string{"/admin", "/login", "/assets/app.js", "/assets/app.css"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", p, resp.StatusCode)
		}
		if strings.HasSuffix(p, "n") && !strings.Contains(resp.Header.Get("Content-Security-Policy"), "script-src 'self'") {
			t.Fatalf("%s missing CSP", p)
		}
	}
	resp, _ := http.Get(srv.URL + "/assets/..%2fadmin.go")
	if resp.StatusCode == 200 {
		t.Fatal("path traversal served a file")
	}
}

func TestLimiter(t *testing.T) {
	l := &limiter{fails: map[string][]time.Time{}}
	for i := 0; i < maxFails; i++ {
		if l.blocked("1.2.3.4") {
			t.Fatal("blocked too early")
		}
		l.fail("1.2.3.4")
	}
	if !l.blocked("1.2.3.4") || l.blocked("5.6.7.8") {
		t.Fatal("limiter")
	}
}

func TestLoginCheck(t *testing.T) {
	srv := newServerWith(t, "text")
	bearer := map[string]string{"Authorization": "Bearer " + pw}
	_, out := call(t, http.DefaultClient, "POST", srv.URL+"/admin/api/claude/check", map[string]any{}, bearer)
	if out["state"] != "valid" || out["cached"] != false || out["reply"] != "Hello world" {
		t.Fatalf("first check %v", out)
	}
	_, out = call(t, http.DefaultClient, "POST", srv.URL+"/admin/api/claude/check", map[string]any{}, bearer)
	if out["state"] != "valid" || out["cached"] != true {
		t.Fatalf("second check should reuse the result: %v", out)
	}
	_, out = call(t, http.DefaultClient, "POST", srv.URL+"/admin/api/claude/check", map[string]any{"force": true}, bearer)
	if out["cached"] != false {
		t.Fatalf("forced check %v", out)
	}
	_, out = call(t, http.DefaultClient, "GET", srv.URL+"/admin/api/claude", nil, bearer)
	if out["auth"].(map[string]any)["state"] != "valid" {
		t.Fatalf("status %v", out)
	}

	srv = newServerWith(t, "auth")
	_, out = call(t, http.DefaultClient, "POST", srv.URL+"/admin/api/claude/check", map[string]any{}, bearer)
	if out["state"] != "login_required" {
		t.Fatalf("auth check %v", out)
	}
	// Checks always re-test, even right after a failure.
	_, out = call(t, http.DefaultClient, "POST", srv.URL+"/admin/api/claude/check", map[string]any{}, bearer)
	if out["state"] != "login_required" || out["cached"] != false {
		t.Fatalf("repeat auth check %v", out)
	}
}
