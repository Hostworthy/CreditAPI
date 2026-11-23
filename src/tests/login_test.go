package integration_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/atb00ker/CreditAPI/src/tests/support"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakeclaude"
)

var hello = map[string]any{"messages": []map[string]any{{"role": "user", "content": "hello"}}}

func errorOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	e, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error body, got %v", out)
	}
	return e
}

// TestLoginJourney follows a fresh install: no login, requests fail with a
// link, the phone sign-in runs, everything works, the login is removed.
func TestLoginJourney(t *testing.T) {
	s := support.NewServer(t, support.Options{PublicURL: "https://claude.example.com"})
	key := s.CreateKey("my-app")

	resp, out := s.Do("GET", "/healthz", nil, nil)
	if resp.StatusCode != 200 || out["claude_token"] != false || out["claude_login"] != "unknown" {
		t.Fatalf("healthz %d %v", resp.StatusCode, out)
	}

	// 1. Requests fail with a 401 that links to the login page.
	resp, out = s.Chat(key, hello)
	e := errorOf(t, out)
	if resp.StatusCode != 401 || e["code"] != "claude_login_required" || e["type"] != "authentication_error" {
		t.Fatalf("chat without login: %d %v", resp.StatusCode, out)
	}
	if !strings.Contains(e["message"].(string), "https://claude.example.com/login") {
		t.Fatalf("message should link to PUBLIC_URL/login: %v", e["message"])
	}

	// 2. While the login is known to be broken, requests fail fast.
	start := time.Now()
	resp, _ = s.Chat(key, hello)
	if resp.StatusCode != 401 || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("fail-fast: %d after %v", resp.StatusCode, time.Since(start))
	}

	// 3. /login's check reports that a sign-in is needed.
	_, out = s.Admin("POST", "/admin/api/claude/check", map[string]any{})
	if out["state"] != "login_required" || out["cached"] != false {
		t.Fatalf("check: %v", out)
	}

	// 4. A wrong code fails; a new sign-in gets a new link.
	_, first := s.Admin("POST", "/admin/api/claude/login", nil)
	if first["state"] != "waiting_for_code" || !strings.HasPrefix(first["url"].(string), fakeclaude.LoginURLPrefix) {
		t.Fatalf("start login: %v", first)
	}
	if code, _ := s.Admin("POST", "/admin/api/claude/login/code", map[string]any{"code": "wrong-code"}); code != 200 {
		t.Fatalf("submit wrong code: %d", code)
	}
	st := s.WaitLogin("failed")["login"].(map[string]any)
	if !strings.Contains(st["output"].(string), "Invalid code") || st["error"] == "" {
		t.Fatalf("failed login should explain itself: %v", st)
	}
	if code, _ := s.Admin("POST", "/admin/api/claude/login/code", map[string]any{"code": "late"}); code != http.StatusConflict {
		t.Fatalf("code after failure: %d", code)
	}
	_, second := s.Admin("POST", "/admin/api/claude/login", nil)
	if second["url"] == first["url"] {
		t.Fatal("a new sign-in should get a new link")
	}

	// 5. The right code signs in and saves the token.
	if code, _ := s.Admin("POST", "/admin/api/claude/login/code", map[string]any{"code": fakeclaude.CodePrefix + "-ok#state"}); code != 200 {
		t.Fatalf("submit code: %d", code)
	}
	status := s.WaitLogin("success")
	token := status["token"].(map[string]any)
	if token["source"] != "login" || !strings.HasPrefix(token["hint"].(string), "sk-ant-oat01-fak") || token["expires_about"] == nil {
		t.Fatalf("token after login: %v", token)
	}
	if strings.Contains(status["login"].(map[string]any)["output"].(string), fakeclaude.TokenPrefix) {
		t.Fatal("full token must not be shown in the CLI output")
	}

	// 6. The new token clears the fail-fast state immediately.
	resp, out = s.Chat(key, hello)
	if resp.StatusCode != 200 || support.Content(out) != "Fake reply to: hello" {
		t.Fatalf("chat after login: %d %v", resp.StatusCode, out)
	}

	// 7. The check reuses the successful request instead of probing again.
	_, out = s.Admin("POST", "/admin/api/claude/check", map[string]any{})
	if out["state"] != "valid" || out["cached"] != true || out["source"] != "request" {
		t.Fatalf("check after request: %v", out)
	}
	_, out = s.Admin("POST", "/admin/api/claude/check", map[string]any{"force": true})
	if out["state"] != "valid" || out["cached"] != false || out["reply"] == "" {
		t.Fatalf("forced check: %v", out)
	}
	_, out = s.Do("GET", "/healthz", nil, nil)
	if out["claude_login"] != "valid" || out["claude_token"] != true {
		t.Fatalf("healthz after login: %v", out)
	}

	// 8. Removing the login makes requests fail again.
	if code, _ := s.Admin("DELETE", "/admin/api/claude/token", nil); code != 200 {
		t.Fatalf("remove token: %d", code)
	}
	resp, _ = s.Chat(key, hello)
	if resp.StatusCode != 401 {
		t.Fatalf("chat after logout: %d", resp.StatusCode)
	}
}

func TestExpiredEnvTokenReplacedFromLoginPage(t *testing.T) {
	s := support.NewServer(t, support.Options{EnvToken: fakeclaude.ExpiredToken})
	key := s.CreateKey("app")

	_, out := s.Admin("GET", "/admin/api/claude", nil)
	if out["token"].(map[string]any)["source"] != "env" {
		t.Fatalf("token source: %v", out["token"])
	}
	resp, out := s.Chat(key, hello)
	if resp.StatusCode != 401 || !strings.Contains(errorOf(t, out)["message"].(string), s.URL+"/login") {
		t.Fatalf("expired env token: %d %v", resp.StatusCode, out)
	}

	s.SignIn()
	resp, _ = s.Chat(key, hello)
	if resp.StatusCode != 200 {
		t.Fatalf("login should override the expired env token: %d", resp.StatusCode)
	}
	// Removing the saved login falls back to the (expired) env token.
	s.Admin("DELETE", "/admin/api/claude/token", nil)
	_, out = s.Admin("GET", "/admin/api/claude", nil)
	if out["token"].(map[string]any)["source"] != "env" {
		t.Fatalf("fallback source: %v", out["token"])
	}
}

func TestValidEnvToken(t *testing.T) {
	s := support.NewServer(t, support.Options{EnvToken: fakeclaude.ValidToken})
	_, out := s.Admin("POST", "/admin/api/claude/check", map[string]any{})
	if out["state"] != "valid" || out["source"] != "check" {
		t.Fatalf("check: %v", out)
	}
}

func TestManualToken(t *testing.T) {
	s := support.NewServer(t, support.Options{})
	if code, out := s.Admin("PUT", "/admin/api/claude/token", map[string]any{"token": "not-a-token"}); code != 400 {
		t.Fatalf("invalid token accepted: %d %v", code, out)
	}
	if code, _ := s.Admin("PUT", "/admin/api/claude/token", map[string]any{"token": fakeclaude.ValidToken}); code != 200 {
		t.Fatal("valid token rejected")
	}
	_, out := s.Admin("POST", "/admin/api/claude/check", map[string]any{})
	if out["state"] != "valid" {
		t.Fatalf("check after manual token: %v", out)
	}
}

func TestLoginCancel(t *testing.T) {
	s := support.NewServer(t, support.Options{})
	s.Admin("POST", "/admin/api/claude/login", nil)
	if code, out := s.Admin("DELETE", "/admin/api/claude/login", nil); code != 200 || out["state"] != "idle" {
		t.Fatalf("cancel: %d %v", code, out)
	}
	if code, _ := s.Admin("POST", "/admin/api/claude/login/code", map[string]any{"code": fakeclaude.CodePrefix}); code != http.StatusConflict {
		t.Fatalf("code after cancel: %d", code)
	}
}
