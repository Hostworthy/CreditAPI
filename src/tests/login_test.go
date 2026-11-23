package integration_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/atb00ker/CreditAPI/src/tests/support"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakeclaude"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakecursor"
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
	s := support.NewServer(t, support.Options{PublicURL: "https://ai.example.com"})
	key := s.CreateKey("my-app")
	sub := s.ClaudeID()
	api := "/admin/api/subscriptions/" + sub

	resp, out := s.Do("GET", "/healthz", nil, nil)
	if _, listed := out["subscriptions"]; listed || out["subscription_count"] != float64(1) {
		t.Fatalf("anonymous healthz must not list subscriptions: %v", out)
	}
	resp, out = s.Do("GET", "/healthz", nil, map[string]string{"Authorization": "Bearer " + support.AdminPassword})
	subs := out["subscriptions"].([]any)
	if resp.StatusCode != 200 || out["logins_ok"] != true || len(subs) != 1 || subs[0].(map[string]any)["login"] != "unknown" {
		t.Fatalf("healthz %d %v", resp.StatusCode, out)
	}

	// 1. Requests fail with a 401 that links to the login page for this
	// subscription.
	resp, out = s.Chat(key, hello)
	e := errorOf(t, out)
	if resp.StatusCode != 401 || e["code"] != "subscription_login_required" || e["type"] != "authentication_error" {
		t.Fatalf("chat without login: %d %v", resp.StatusCode, out)
	}
	if !strings.Contains(e["message"].(string), "https://ai.example.com/login?subscription="+sub) ||
		!strings.Contains(e["message"].(string), "Claude is signed out") {
		t.Fatalf("message should name the subscription and link to PUBLIC_URL/login: %v", e["message"])
	}
	_, out = s.Do("GET", "/healthz", nil, nil)
	if out["logins_ok"] != false || out["logins_needed"] != float64(1) {
		t.Fatalf("healthz should report the broken login: %v", out)
	}

	// 2. While the login is known to be broken, requests fail fast.
	start := time.Now()
	resp, _ = s.Chat(key, hello)
	if resp.StatusCode != 401 || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("fail-fast: %d after %v", resp.StatusCode, time.Since(start))
	}

	// 3. /login's check reports that a sign-in is needed.
	_, out = s.Admin("POST", api+"/check", map[string]any{})
	if out["state"] != "login_required" || out["cached"] != false {
		t.Fatalf("check: %v", out)
	}

	// 4. A wrong code fails; a new sign-in gets a new link.
	_, first := s.Admin("POST", api+"/login", nil)
	if first["state"] != "waiting_for_code" || first["needs_code"] != true || !strings.HasPrefix(first["url"].(string), fakeclaude.LoginURLPrefix) {
		t.Fatalf("start login: %v", first)
	}
	if code, _ := s.Admin("POST", api+"/login/code", map[string]any{"code": "wrong-code"}); code != 200 {
		t.Fatalf("submit wrong code: %d", code)
	}
	st := s.WaitLogin(sub, "failed")["login"].(map[string]any)
	if !strings.Contains(st["output"].(string), "Invalid code") || st["error"] == "" {
		t.Fatalf("failed login should explain itself: %v", st)
	}
	if code, _ := s.Admin("POST", api+"/login/code", map[string]any{"code": "late"}); code != http.StatusConflict {
		t.Fatalf("code after failure: %d", code)
	}
	_, second := s.Admin("POST", api+"/login", nil)
	if second["url"] == first["url"] {
		t.Fatal("a new sign-in should get a new link")
	}

	// 5. The right code signs in and saves the token.
	if code, _ := s.Admin("POST", api+"/login/code", map[string]any{"code": fakeclaude.CodePrefix + "-ok#state"}); code != 200 {
		t.Fatalf("submit code: %d", code)
	}
	status := s.WaitLogin(sub, "success")
	token := status["credential"].(map[string]any)
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
	_, out = s.Admin("POST", api+"/check", map[string]any{})
	if out["state"] != "valid" || out["cached"] != true || out["source"] != "request" {
		t.Fatalf("check after request: %v", out)
	}
	_, out = s.Admin("POST", api+"/check", map[string]any{"force": true})
	if out["state"] != "valid" || out["cached"] != false || out["reply"] == "" {
		t.Fatalf("forced check: %v", out)
	}
	_, out = s.Do("GET", "/healthz", nil, map[string]string{"Authorization": "Bearer " + support.AdminPassword})
	if out["logins_ok"] != true || out["subscriptions"].([]any)[0].(map[string]any)["login"] != "valid" {
		t.Fatalf("healthz after login: %v", out)
	}

	// 8. Removing the login makes requests fail again.
	if code, _ := s.Admin("DELETE", api+"/credential", nil); code != 200 {
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
	api := "/admin/api/subscriptions/" + s.ClaudeID()

	_, out := s.Admin("GET", api, nil)
	if out["credential"].(map[string]any)["source"] != "env" {
		t.Fatalf("token source: %v", out["credential"])
	}
	resp, out := s.Chat(key, hello)
	if resp.StatusCode != 401 || !strings.Contains(errorOf(t, out)["message"].(string), s.URL+"/login?subscription=") {
		t.Fatalf("expired env token: %d %v", resp.StatusCode, out)
	}

	s.SignIn()
	resp, _ = s.Chat(key, hello)
	if resp.StatusCode != 200 {
		t.Fatalf("login should override the expired env token: %d", resp.StatusCode)
	}
	// Removing the saved login falls back to the (expired) env token.
	s.Admin("DELETE", api+"/credential", nil)
	_, out = s.Admin("GET", api, nil)
	if out["credential"].(map[string]any)["source"] != "env" {
		t.Fatalf("fallback source: %v", out["credential"])
	}
}

func TestValidEnvToken(t *testing.T) {
	s := support.NewServer(t, support.Options{EnvToken: fakeclaude.ValidToken})
	_, out := s.Admin("POST", "/admin/api/subscriptions/"+s.ClaudeID()+"/check", map[string]any{})
	if out["state"] != "valid" || out["source"] != "check" {
		t.Fatalf("check: %v", out)
	}
}

func TestManualToken(t *testing.T) {
	s := support.NewServer(t, support.Options{})
	api := "/admin/api/subscriptions/" + s.ClaudeID()
	if code, out := s.Admin("PUT", api+"/credential", map[string]any{"token": "not-a-token"}); code != 400 {
		t.Fatalf("invalid token accepted: %d %v", code, out)
	}
	if code, _ := s.Admin("PUT", api+"/credential", map[string]any{"credential": fakeclaude.ValidToken}); code != 200 {
		t.Fatal("valid token rejected")
	}
	_, out := s.Admin("POST", api+"/check", map[string]any{})
	if out["state"] != "valid" {
		t.Fatalf("check after manual token: %v", out)
	}
}

func TestLoginCancel(t *testing.T) {
	s := support.NewServer(t, support.Options{})
	api := "/admin/api/subscriptions/" + s.ClaudeID()
	s.Admin("POST", api+"/login", nil)
	if code, out := s.Admin("DELETE", api+"/login", nil); code != 200 || out["state"] != "idle" {
		t.Fatalf("cancel: %d %v", code, out)
	}
	if code, _ := s.Admin("POST", api+"/login/code", map[string]any{"code": fakeclaude.CodePrefix}); code != http.StatusConflict {
		t.Fatalf("code after cancel: %d", code)
	}
	if code, _ := s.Admin("POST", "/admin/api/subscriptions/missing/login", nil); code != 404 {
		t.Fatalf("login for unknown subscription: %d", code)
	}
}

// TestCursorLoginJourney signs a Cursor subscription in through the browser
// flow (no code to paste) and out again.
func TestCursorLoginJourney(t *testing.T) {
	s := support.NewServer(t, support.Options{})
	id, key := s.CreateKeyFor("cursor-app", []string{})
	cur := s.CreateSubscription("Cursor (Personal)", "cursor", id)
	api := "/admin/api/subscriptions/" + cur

	resp, out := s.Chat(key, hello)
	e := errorOf(t, out)
	if resp.StatusCode != 401 || !strings.Contains(e["message"].(string), "/login?subscription="+cur) ||
		!strings.Contains(e["message"].(string), "Cursor (Personal) is signed out") {
		t.Fatalf("chat without login: %d %v", resp.StatusCode, out)
	}

	_, st := s.Admin("POST", api+"/login", nil)
	if st["state"] != "waiting_for_browser" || st["needs_code"] == true || !strings.HasPrefix(st["url"].(string), fakecursor.LoginURLPrefix) {
		t.Fatalf("start login: %v", st)
	}
	if code, _ := s.Admin("POST", api+"/login/code", map[string]any{"code": "x"}); code != http.StatusConflict {
		t.Fatalf("a browser sign-in takes no code: %d", code)
	}
	s.WaitLogin(cur, "success")

	// The new sign-in clears the remembered failure immediately.
	resp, out = s.Chat(key, map[string]any{"messages": []map[string]any{{"role": "user", "content": "[fake:whoami]"}}})
	if resp.StatusCode != 200 || support.Content(out) != "Served by cursor subscription "+cur {
		t.Fatalf("chat after login: %d %v", resp.StatusCode, out)
	}
	_, out = s.Admin("POST", api+"/check", map[string]any{"force": true})
	if out["state"] != "valid" {
		t.Fatalf("check: %v", out)
	}

	// Signing out runs `cursor-agent logout`.
	if code, _ := s.Admin("DELETE", api+"/credential", nil); code != 200 {
		t.Fatal("sign out failed")
	}
	if resp, _ := s.Chat(key, hello); resp.StatusCode != 401 {
		t.Fatalf("chat after sign-out: %d", resp.StatusCode)
	}
}

func TestCursorLoginDenied(t *testing.T) {
	s := support.NewServer(t, support.Options{})
	t.Setenv(fakecursor.EnvLogin, "deny")
	cur := s.CreateSubscription("Cursor", "cursor")
	s.Admin("POST", "/admin/api/subscriptions/"+cur+"/login", nil)
	st := s.WaitLogin(cur, "failed")["login"].(map[string]any)
	if !strings.Contains(st["output"].(string), "denied") {
		t.Fatalf("status %v", st)
	}
}

func TestCursorAPIKey(t *testing.T) {
	s := support.NewServer(t, support.Options{})
	id, key := s.CreateKeyFor("app", []string{})
	cur := s.CreateSubscription("Cursor (Work)", "cursor", id)
	api := "/admin/api/subscriptions/" + cur
	if code, _ := s.Admin("PUT", api+"/credential", map[string]any{"credential": fakecursor.ExpiredKey}); code != 200 {
		t.Fatal("key rejected")
	}
	if resp, _ := s.Chat(key, hello); resp.StatusCode != 401 {
		t.Fatalf("expired key: %d", resp.StatusCode)
	}
	_, out := s.Admin("PUT", api+"/credential", map[string]any{"credential": fakecursor.ValidKey})
	if c := out["credential"].(map[string]any); c["source"] != "login" || c["name"] != "CURSOR_API_KEY" {
		t.Fatalf("credential %v", c)
	}
	if resp, out := s.Chat(key, hello); resp.StatusCode != 200 || support.Content(out) != "Fake reply to: hello" {
		t.Fatalf("chat with key: %d %v", resp.StatusCode, out)
	}
}
