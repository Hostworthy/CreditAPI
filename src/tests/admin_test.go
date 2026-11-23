package integration_test

import (
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"

	"github.com/atb00ker/CreditAPI/src/tests/support"
)

func TestAPIKeyAuth(t *testing.T) {
	s, key := signedIn(t, support.Options{})

	for name, headers := range map[string]map[string]string{
		"missing":   {},
		"wrong":     {"Authorization": "Bearer sk-credit-wrong"},
		"not ours":  {"Authorization": "Bearer sk-ant-oat01-something"},
		"bad shape": {"Authorization": "Basic abc"},
	} {
		resp, out := s.Do("POST", "/v1/chat/completions", hello, headers)
		if resp.StatusCode != 401 || errorOf(t, out)["code"] != "invalid_api_key" {
			t.Errorf("%s: %d %v", name, resp.StatusCode, out)
		}
	}
	for name, headers := range map[string]map[string]string{
		"bearer":    {"Authorization": "Bearer " + key},
		"lowercase": {"Authorization": "bearer " + key},
		"x-api-key": {"X-Api-Key": key},
		"api-key":   {"Api-Key": key},
	} {
		if resp, _ := s.Do("POST", "/v1/chat/completions", hello, headers); resp.StatusCode != 200 {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	// Paths without /v1 work too.
	if resp, _ := s.Do("POST", "/chat/completions", hello, map[string]string{"Authorization": "Bearer " + key}); resp.StatusCode != 200 {
		t.Errorf("no /v1 prefix: %d", resp.StatusCode)
	}
}

func TestModels(t *testing.T) {
	s := support.NewServer(t, support.Options{StrictModels: true})
	key := s.CreateKey("m")
	auth := map[string]string{"Authorization": "Bearer " + key}
	resp, out := s.Do("GET", "/v1/models", nil, auth)
	if resp.StatusCode != 200 || out["object"] != "list" {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	var ids []string
	for _, m := range out["data"].([]any) {
		ids = append(ids, m.(map[string]any)["id"].(string))
	}
	if strings.Join(ids, ",") != "sonnet,opus,haiku,gpt-4o,claude/sonnet,claude/opus,claude/haiku" {
		t.Fatalf("ids %v", ids)
	}
	for _, m := range []string{"haiku", "claude/opus", "gpt-4o"} {
		if resp, _ := s.Do("GET", "/v1/models/"+m, nil, auth); resp.StatusCode != 200 {
			t.Fatalf("get model %s: %d", m, resp.StatusCode)
		}
	}
	if resp, _ := s.Do("GET", "/v1/models/gpt-5-mega", nil, auth); resp.StatusCode != 404 {
		t.Fatalf("unknown model: %d", resp.StatusCode)
	}
	if resp, _ := s.Do("GET", "/v1/models", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("models need a key: %d", resp.StatusCode)
	}
}

func TestKeyLifecycleAndUsage(t *testing.T) {
	s, key := signedIn(t, support.Options{})
	for i := 0; i < 3; i++ {
		s.Chat(key, hello)
	}
	_, out := s.Admin("GET", "/admin/api/keys", nil)
	keys := out["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("keys %v", keys)
	}
	k := keys[0].(map[string]any)
	if k["requests"].(float64) != 3 || k["input_tokens"].(float64) <= 0 || k["output_tokens"].(float64) <= 0 || k["last_used_at"] == nil {
		t.Fatalf("usage %v", k)
	}
	if _, ok := k["hash"]; ok && k["hash"] != "" {
		t.Fatal("hash must not be exposed")
	}
	if !strings.HasPrefix(k["hint"].(string), "sk-credit-") || strings.Contains(k["hint"].(string), key[12:]) {
		t.Fatalf("hint %v", k["hint"])
	}
	id := k["id"].(string)

	if code, out := s.Admin("PATCH", "/admin/api/keys/"+id, map[string]any{"name": "renamed", "disabled": true}); code != 200 || out["key"].(map[string]any)["name"] != "renamed" {
		t.Fatalf("patch: %d %v", code, out)
	}
	if resp, _ := s.Chat(key, hello); resp.StatusCode != 401 {
		t.Fatalf("disabled key: %d", resp.StatusCode)
	}
	s.Admin("PATCH", "/admin/api/keys/"+id, map[string]any{"disabled": false})
	if resp, _ := s.Chat(key, hello); resp.StatusCode != 200 {
		t.Fatalf("re-enabled key: %d", resp.StatusCode)
	}
	if code, _ := s.Admin("PATCH", "/admin/api/keys/"+id, map[string]any{"name": "  "}); code != 400 {
		t.Fatalf("blank name: %d", code)
	}
	if code, _ := s.Admin("POST", "/admin/api/keys", map[string]any{"name": ""}); code != 400 {
		t.Fatalf("create blank: %d", code)
	}
	if code, _ := s.Admin("DELETE", "/admin/api/keys/"+id, nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := s.Admin("DELETE", "/admin/api/keys/"+id, nil); code != 404 {
		t.Fatalf("delete again: %d", code)
	}
	if code, _ := s.Admin("PATCH", "/admin/api/keys/nope", map[string]any{"disabled": true}); code != 404 {
		t.Fatalf("patch unknown: %d", code)
	}
	if resp, _ := s.Chat(key, hello); resp.StatusCode != 401 {
		t.Fatalf("deleted key: %d", resp.StatusCode)
	}
}

func TestAdminSessionCookie(t *testing.T) {
	s := support.NewServer(t, support.Options{PublicURL: "https://ai.example.com"})
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	post := func(path, body string, headers map[string]string) *http.Response {
		req, _ := http.NewRequest("POST", s.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	if resp := post("/admin/api/session", `{"password":"wrong"}`, nil); resp.StatusCode != 401 {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	// PUBLIC_URL is https, so the cookie is Secure; a spoofed
	// X-Forwarded-Proto alone (no TRUSTED_PROXIES) would not do it.
	resp := post("/admin/api/session", `{"password":"`+support.AdminPassword+`"}`, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("sign in: %d", resp.StatusCode)
	}
	cookie := resp.Header.Get("Set-Cookie")
	for _, attr := range []string{"creditapi_admin=", "HttpOnly", "SameSite=Strict", "Secure", "Path=/"} {
		if !strings.Contains(cookie, attr) {
			t.Errorf("cookie missing %s: %s", attr, cookie)
		}
	}
	// The Secure cookie is not sent over plain http by the jar, so use the
	// value directly for the next checks.
	value := strings.SplitN(strings.TrimPrefix(cookie, "creditapi_admin="), ";", 2)[0]
	withCookie := map[string]string{"Cookie": "creditapi_admin=" + value}
	if r, _ := s.Do("GET", "/admin/api/keys", nil, withCookie); r.StatusCode != 200 {
		t.Fatalf("cookie auth: %d", r.StatusCode)
	}
	withCookie["Origin"] = "https://evil.example"
	if r, _ := s.Do("POST", "/admin/api/keys", map[string]any{"name": "x"}, withCookie); r.StatusCode != 403 {
		t.Fatalf("cross-origin write: %d", r.StatusCode)
	}
	if r, _ := s.Do("GET", "/admin/api/keys", nil, map[string]string{"Cookie": "creditapi_admin=1.forged"}); r.StatusCode != 401 {
		t.Fatalf("forged cookie: %d", r.StatusCode)
	}
}

func TestAdminLockout(t *testing.T) {
	s := support.NewServer(t, support.Options{})
	for i := 0; i < 10; i++ {
		if resp, _ := s.Do("GET", "/admin/api/keys", nil, map[string]string{"Authorization": "Bearer wrong-password"}); resp.StatusCode != 401 {
			t.Fatalf("attempt %d: %d", i, resp.StatusCode)
		}
	}
	resp, _ := s.Do("GET", "/admin/api/keys", nil, map[string]string{"Authorization": "Bearer " + support.AdminPassword})
	if resp.StatusCode != 429 {
		t.Fatalf("after 10 failures even the right password should wait: %d", resp.StatusCode)
	}
}

func TestPagesAndHeaders(t *testing.T) {
	s := support.NewServer(t, support.Options{})
	for _, p := range []string{"/admin", "/login"} {
		resp, _ := s.Do("GET", p, nil, nil)
		h := resp.Header
		if resp.StatusCode != 200 || !strings.Contains(h.Get("Content-Security-Policy"), "script-src 'self'") ||
			h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d %v", p, resp.StatusCode, h)
		}
	}
	for _, p := range []string{"/assets/app.js", "/assets/app.css"} {
		if resp, _ := s.Do("GET", p, nil, nil); resp.StatusCode != 200 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	if resp, _ := s.Do("GET", "/assets/..%2fadmin.go", nil, nil); resp.StatusCode == 200 {
		t.Error("asset path traversal")
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(s.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/admin" {
		t.Errorf("root redirect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestRequestIDs(t *testing.T) {
	s := support.NewServer(t, support.Options{})
	resp, _ := s.Do("GET", "/healthz", nil, map[string]string{"X-Request-Id": "trace-123"})
	if resp.Header.Get("X-Request-Id") != "trace-123" {
		t.Fatalf("echo: %q", resp.Header.Get("X-Request-Id"))
	}
	resp, _ = s.Do("GET", "/healthz", nil, map[string]string{"X-Request-Id": "bad id with spaces"})
	if id := resp.Header.Get("X-Request-Id"); id == "" || strings.Contains(id, " ") {
		t.Fatalf("invalid ids are replaced: %q", id)
	}
}
