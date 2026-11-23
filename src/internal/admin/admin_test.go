package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/atb00ker/CreditAPI/src/internal/agent"
	"github.com/atb00ker/CreditAPI/src/internal/agent/claude"
	"github.com/atb00ker/CreditAPI/src/internal/agent/cursor"
	"github.com/atb00ker/CreditAPI/src/internal/config"
	"github.com/atb00ker/CreditAPI/src/internal/keys"
	"github.com/atb00ker/CreditAPI/src/internal/subscriptions"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakeclaude"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakes"
)

const pw = "correct horse battery"

func TestMain(m *testing.M) {
	fakes.MaybeRun()
	os.Exit(m.Run())
}

func newServer(t *testing.T) *httptest.Server {
	return newServerWith(t, "text")
}

// newServerWith serves the admin routes with one "Claude" subscription
// whose CLI is the fake in the given scenario.
func newServerWith(t *testing.T, scenario string) *httptest.Server {
	t.Helper()
	srv, _ := newAdmin(t, scenario)
	return srv
}

func newAdmin(t *testing.T, scenario string) (*httptest.Server, *subscriptions.Manager) {
	t.Helper()
	return newAdminWith(t, scenario, nil)
}

func newAdminWith(t *testing.T, scenario string, tweak func(*config.Config)) (*httptest.Server, *subscriptions.Manager) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(fakeclaude.EnvScenario, scenario)
	t.Setenv("ADMIN_TEST_TOKEN", "tok")
	cfg := &config.Config{DataDir: dir, AdminPassword: pw, DefaultModel: "sonnet", Models: []string{"sonnet"}, MaxConcurrent: 1}
	if tweak != nil {
		tweak(cfg)
	}
	ks, err := keys.Open(filepath.Join(dir, "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	store, _, err := subscriptions.OpenStore(filepath.Join(dir, "subscriptions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("Claude", "claude", nil, "ADMIN_TEST_TOKEN"); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	subs := subscriptions.NewManager(dir, store, &agent.EnvBuilder{}, subscriptions.Routing{},
		&claude.Backend{Bin: exe, WorkDir: dir, Default: "sonnet", List: []string{"sonnet", "haiku"}},
		&cursor.Backend{Bin: "/bin/false", Default: "auto", List: []string{"auto"}})
	a, err := New(cfg, ks, subs, agent.NewSlots(1))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	a.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, subs
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
	bearer := map[string]string{"Authorization": "Bearer " + pw}
	code, out = call(t, http.DefaultClient, "GET", srv.URL+"/admin/api/subscriptions", nil, bearer)
	if code != 200 {
		t.Fatalf("bearer: %d", code)
	}
	subID := out["subscriptions"].([]any)[0].(map[string]any)["id"].(string)
	if code, _ := call(t, http.DefaultClient, "PUT", srv.URL+"/admin/api/subscriptions/"+subID+"/credential", map[string]string{"token": "bad"}, bearer); code != 400 {
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

func firstSubscription(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	_, out := call(t, http.DefaultClient, "GET", srv.URL+"/admin/api/subscriptions", nil, map[string]string{"Authorization": "Bearer " + pw})
	return out["subscriptions"].([]any)[0].(map[string]any)["id"].(string)
}

func TestLoginCheck(t *testing.T) {
	srv := newServerWith(t, "text")
	bearer := map[string]string{"Authorization": "Bearer " + pw}
	base := srv.URL + "/admin/api/subscriptions/" + firstSubscription(t, srv)
	_, out := call(t, http.DefaultClient, "POST", base+"/check", map[string]any{}, bearer)
	if out["state"] != "valid" || out["cached"] != false || out["reply"] != "Hello world" {
		t.Fatalf("first check %v", out)
	}
	_, out = call(t, http.DefaultClient, "POST", base+"/check", map[string]any{}, bearer)
	if out["state"] != "valid" || out["cached"] != true {
		t.Fatalf("second check should reuse the result: %v", out)
	}
	_, out = call(t, http.DefaultClient, "POST", base+"/check", map[string]any{"force": true}, bearer)
	if out["cached"] != false {
		t.Fatalf("forced check %v", out)
	}
	_, out = call(t, http.DefaultClient, "GET", base, nil, bearer)
	if out["auth"].(map[string]any)["state"] != "valid" || out["credential"].(map[string]any)["source"] != "env" {
		t.Fatalf("status %v", out)
	}
	if code, _ := call(t, http.DefaultClient, "POST", srv.URL+"/admin/api/subscriptions/missing/check", map[string]any{}, bearer); code != 404 {
		t.Fatalf("unknown subscription: %d", code)
	}

	srv = newServerWith(t, "auth")
	base = srv.URL + "/admin/api/subscriptions/" + firstSubscription(t, srv)
	_, out = call(t, http.DefaultClient, "POST", base+"/check", map[string]any{}, bearer)
	if out["state"] != "login_required" {
		t.Fatalf("auth check %v", out)
	}
	// Checks always re-test, even right after a failure.
	_, out = call(t, http.DefaultClient, "POST", base+"/check", map[string]any{}, bearer)
	if out["state"] != "login_required" || out["cached"] != false {
		t.Fatalf("repeat auth check %v", out)
	}
}

func TestSubscriptionsAPI(t *testing.T) {
	srv, subs := newAdmin(t, "text")
	bearer := map[string]string{"Authorization": "Bearer " + pw}
	api := srv.URL + "/admin/api"

	_, out := call(t, http.DefaultClient, "POST", api+"/keys", map[string]any{"name": "k1"}, bearer)
	k1 := out["key"].(map[string]any)["id"].(string)
	if grants := out["key"].(map[string]any)["subscriptions"].([]any); len(grants) != 1 {
		t.Fatalf("a new key may use every subscription by default: %v", out)
	}

	code, out := call(t, http.DefaultClient, "POST", api+"/subscriptions", map[string]any{"name": "Cursor (Personal)", "provider": "Cursor", "keys": []string{k1}}, bearer)
	if code != 201 || out["slug"] != "cursor-personal" || out["provider_label"] != "Cursor" || out["priority"] != float64(1) {
		t.Fatalf("create: %d %v", code, out)
	}
	cur := out["id"].(string)
	for _, bad := range []map[string]any{
		{"name": "X", "provider": "gemini"},
		{"name": "", "provider": "claude"},
		{"name": "cursor personal", "provider": "claude"},
		{"name": "Y", "provider": "claude", "keys": []string{"no-such-key"}},
	} {
		if code, _ := call(t, http.DefaultClient, "POST", api+"/subscriptions", bad, bearer); code != 400 {
			t.Errorf("create %v: %d", bad, code)
		}
	}

	_, out = call(t, http.DefaultClient, "GET", api+"/subscriptions", nil, bearer)
	if len(out["subscriptions"].([]any)) != 2 || len(out["providers"].([]any)) != 2 {
		t.Fatalf("list %v", out)
	}

	// Key grants can be set from either side.
	_, out = call(t, http.DefaultClient, "PATCH", api+"/keys/"+k1, map[string]any{"subscriptions": []string{cur}}, bearer)
	if grants := out["key"].(map[string]any)["subscriptions"].([]any); len(grants) != 1 || grants[0] != cur {
		t.Fatalf("key grants %v", out)
	}
	if code, _ := call(t, http.DefaultClient, "PATCH", api+"/keys/"+k1, map[string]any{"subscriptions": []string{"missing"}}, bearer); code != 400 {
		t.Fatalf("unknown subscription grant: %d", code)
	}
	code, out = call(t, http.DefaultClient, "PATCH", api+"/subscriptions/"+cur, map[string]any{"name": "Cursor (Work)", "disabled": true, "keys": []string{}}, bearer)
	if code != 200 || out["name"] != "Cursor (Work)" || out["disabled"] != true || len(out["keys"].([]any)) != 0 {
		t.Fatalf("update: %d %v", code, out)
	}
	if code, _ := call(t, http.DefaultClient, "PATCH", api+"/subscriptions/"+cur, map[string]any{"keys": []string{"nope"}}, bearer); code != 400 {
		t.Fatalf("unknown key: %d", code)
	}

	code, out = call(t, http.DefaultClient, "POST", api+"/subscriptions/"+cur+"/move", map[string]any{"direction": "up"}, bearer)
	if code != 200 || out["subscriptions"].([]any)[0].(map[string]any)["id"] != cur {
		t.Fatalf("move: %d %v", code, out)
	}
	if code, _ := call(t, http.DefaultClient, "POST", api+"/subscriptions/"+cur+"/move", map[string]any{"direction": "sideways"}, bearer); code != 400 {
		t.Fatalf("bad direction: %d", code)
	}

	// Credentials: validated per provider, shown only as a hint.
	if code, _ := call(t, http.DefaultClient, "PUT", api+"/subscriptions/"+cur+"/credential", map[string]any{"credential": "short"}, bearer); code != 400 {
		t.Fatalf("short key: %d", code)
	}
	key := "crsr_0123456789abcdefghijklmnopqrstuvwxyz"
	code, out = call(t, http.DefaultClient, "PUT", api+"/subscriptions/"+cur+"/credential", map[string]any{"credential": key}, bearer)
	cred := out["credential"].(map[string]any)
	if code != 200 || cred["source"] != "login" || cred["name"] != "CURSOR_API_KEY" || strings.Contains(cred["hint"].(string), key) {
		t.Fatalf("credential: %d %v", code, out)
	}

	// Deleting a key removes its grants; deleting a subscription its files.
	call(t, http.DefaultClient, "PATCH", api+"/subscriptions/"+cur, map[string]any{"keys": []string{k1}}, bearer)
	call(t, http.DefaultClient, "DELETE", api+"/keys/"+k1, nil, bearer)
	if sub, _ := subs.Store.Get(cur); len(sub.Keys) != 0 {
		t.Fatalf("grants left after key deletion: %v", sub.Keys)
	}
	if code, _ := call(t, http.DefaultClient, "DELETE", api+"/subscriptions/"+cur, nil, bearer); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := call(t, http.DefaultClient, "GET", api+"/subscriptions/"+cur, nil, bearer); code != 404 {
		t.Fatalf("get deleted: %d", code)
	}
	if code, _ := call(t, http.DefaultClient, "DELETE", api+"/subscriptions/"+cur, nil, bearer); code != 404 {
		t.Fatalf("delete twice: %d", code)
	}
}

func TestAdminBodyLimit(t *testing.T) {
	srv := newServer(t)
	big := map[string]string{"name": strings.Repeat("x", 70<<10)}
	if code, _ := call(t, http.DefaultClient, "POST", srv.URL+"/admin/api/keys", big, map[string]string{"Authorization": "Bearer " + pw}); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", code)
	}
}

func TestSignOutRevokesCopiedCookies(t *testing.T) {
	srv := newServer(t)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	call(t, c, "POST", srv.URL+"/admin/api/session", map[string]string{"password": pw}, nil)
	u, _ := url.Parse(srv.URL)
	copied := jar.Cookies(u)
	if len(copied) != 1 || strings.Count(copied[0].Value, ".") != 2 {
		t.Fatalf("cookie %v", copied)
	}
	// A second sign-in gets a different cookie.
	jar2, _ := cookiejar.New(nil)
	c2 := &http.Client{Jar: jar2}
	call(t, c2, "POST", srv.URL+"/admin/api/session", map[string]string{"password": pw}, nil)
	if jar2.Cookies(u)[0].Value == copied[0].Value {
		t.Fatal("sessions must be unique")
	}
	call(t, c, "DELETE", srv.URL+"/admin/api/session", nil, nil)
	thief, _ := cookiejar.New(nil)
	thief.SetCookies(u, copied)
	if code, _ := call(t, &http.Client{Jar: thief}, "GET", srv.URL+"/admin/api/keys", nil, nil); code != 401 {
		t.Fatalf("copied cookie still works after sign-out: %d", code)
	}
	if code, _ := call(t, c2, "GET", srv.URL+"/admin/api/keys", nil, nil); code != 401 {
		t.Fatalf("sign-out signs every device out: %d", code)
	}
}

func TestLimiterIsBounded(t *testing.T) {
	l := &limiter{fails: map[string][]time.Time{}}
	for i := 0; i < maxTracked+50; i++ {
		l.fail(strconv.Itoa(i))
	}
	if len(l.fails) > maxTracked {
		t.Fatalf("tracked %d addresses", len(l.fails))
	}
}

func TestPublicURLMakesCookieSecure(t *testing.T) {
	srv, _ := newAdminWith(t, "text", func(c *config.Config) { c.PublicURL = "https://ai.example.com" })
	resp, err := http.Post(srv.URL+"/admin/api/session", "application/json", strings.NewReader(`{"password":"`+pw+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if sc := resp.Header.Get("Set-Cookie"); !strings.Contains(sc, "Secure") {
		t.Fatalf("cookie should be Secure when PUBLIC_URL is https: %s", sc)
	}
	// An Origin matching PUBLIC_URL is same-origin.
	req, _ := http.NewRequest("POST", srv.URL+"/admin/api/session", strings.NewReader(`{"password":"`+pw+`"}`))
	req.Header.Set("Origin", "https://ai.example.com")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 200 {
		t.Fatalf("origin from PUBLIC_URL refused: %d", resp.StatusCode)
	}
}

func TestCookieValidation(t *testing.T) {
	dir := t.TempDir()
	a, err := New(&config.Config{DataDir: dir, AdminPassword: pw}, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	withCookie := func(v string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.AddCookie(&http.Cookie{Name: cookieName, Value: v})
		return r
	}
	future := time.Now().Add(time.Hour).Unix()
	good := a.sign(future, "nonce")
	if !a.validCookie(withCookie(good)) {
		t.Fatal("a fresh cookie should be valid")
	}
	expired := a.sign(time.Now().Add(-time.Minute).Unix(), "nonce")
	tampered := strings.Replace(good, "nonce", "other", 1)
	for name, v := range map[string]string{"expired": expired, "tampered": tampered, "two parts": "1.2", "empty": "", "junk": "a.b.c"} {
		if a.validCookie(withCookie(v)) {
			t.Errorf("%s cookie accepted", name)
		}
	}
	other, _ := New(&config.Config{DataDir: t.TempDir(), AdminPassword: pw}, nil, nil, nil)
	if other.validCookie(withCookie(good)) {
		t.Fatal("a cookie from another installation must not be valid")
	}
	if !a.passwordOK(pw) || a.passwordOK(pw+"x") || a.passwordOK("") {
		t.Fatal("password check")
	}
}
