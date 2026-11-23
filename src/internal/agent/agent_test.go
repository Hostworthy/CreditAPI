package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atb00ker/CreditAPI/src/tests/support/fakeclaude"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakecursor"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakes"
)

func TestMain(m *testing.M) {
	fakes.MaybeRun()
	os.Exit(m.Run())
}

// Shape copied from real `claude setup-token` output (values replaced).
const realSetupTokenSample = "Browser didn't open?\x1b[23GUse the URL\x1b[35Gbelow\x1b[41Gto\x1b[44Gsign\x1b[49Gin\x1b[52G\x1b[38;5;246m(c to copy)\x1b[39m\r\r\n\r\r\n" +
	"\x1b]8;id=1h0vfy3;https://claude.ai/oauth/authorize?Abc-123_xyz\x07\x1b[38;5;246mhttps://claude.ai/oauth/authorize?Abc-123_xyz\x1b[39m\x1b]8;;\x07\r\r\n\r\r\n" +
	"\x1b[2GPaste\x1b[8Gcode\x1b[13Ghere\x1b[18Gif\x1b[21Gprompted\r\r\n\x1b[2G\x1b[38;5;153m╭──────────╮\x1b[39m\r\r\n"

func TestExtractLoginURL(t *testing.T) {
	if got := ExtractLoginURL(realSetupTokenSample); got != "https://claude.ai/oauth/authorize?Abc-123_xyz" {
		t.Fatalf("got %q", got)
	}
	// Without hyperlinks, fall back to plain text.
	plain := "Visit: https://claude.ai/oauth/authorize?code=true&state=x\nthen paste"
	if got := ExtractLoginURL(plain); got != "https://claude.ai/oauth/authorize?code=true&state=x" {
		t.Fatalf("plain got %q", got)
	}
	colored := "Open the following link to sign in:\r\n\x1b[36mhttps://cursor.com/loginDeepControl?challenge=x&uuid=y\x1b[39m\r\n"
	if got := ExtractLoginURL(colored); got != "https://cursor.com/loginDeepControl?challenge=x&uuid=y" {
		t.Fatalf("colored got %q", got)
	}
	if got := ExtractLoginURL("no url here https://example.com/x"); got != "" {
		t.Fatalf("unexpected %q", got)
	}
}

func TestStripANSIAndTidy(t *testing.T) {
	s := TidyOutput(realSetupTokenSample)
	if !strings.Contains(s, "Use the URL below to sign in") {
		t.Fatalf("spaces not restored: %q", s)
	}
	if strings.Contains(s, "\x1b") || strings.Contains(s, "──────") {
		t.Fatalf("escape or box chars left: %q", s)
	}
}

func TestExtractJSON(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`:                            `{"a":1}`,
		"```json\n{\"a\": 2}\n```":           `{"a": 2}`,
		"Sure! Here it is: {\"a\":3} thanks": `{"a":3}`,
		"[1,2]":                              `[1,2]`,
	}
	for in, want := range cases {
		if got := string(ExtractJSON(in)); got != want {
			t.Errorf("ExtractJSON(%q) = %q, want %q", in, got, want)
		}
	}
	if ExtractJSON("no json") != nil || ExtractJSON("  ") != nil {
		t.Error("expected nil")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		msg    string
		kind   string
		code   int
	}{
		{401, "Failed to authenticate", "auth", 401},
		{0, "Invalid API key · Please run /login", "auth", 401},
		{0, fakecursor.NotLoggedIn, "auth", 401},
		{0, "Unauthorized: this API key has expired", "auth", 401},
		{429, "too many", "rate_limit", 429},
		{0, "You've hit your usage limit", "rate_limit", 429},
		{529, "x", "overloaded", 503},
		{0, "The model is overloaded", "overloaded", 503},
		{400, "bad", "invalid_request", 400},
		{0, "weird", "upstream", 502},
	}
	for _, c := range cases {
		e := Classify("Cursor", c.status, c.msg)
		if e.Kind != c.kind || e.Status != c.code {
			t.Errorf("Classify(%d, %q) = %+v, want %s %d", c.status, c.msg, e, c.kind, c.code)
		}
	}
	if e := Classify("Cursor", 401, "x"); !strings.HasPrefix(e.Message, "Cursor login required") {
		t.Errorf("message %q", e.Message)
	}
	for kind, want := range map[string]bool{"auth": true, "rate_limit": true, "overloaded": true, "upstream": true, "invalid_request": false, "busy": false} {
		if got := (&Error{Kind: kind}).Retryable(); got != want {
			t.Errorf("Retryable(%s) = %v", kind, got)
		}
	}
}

func TestTruncateTailAndEstimate(t *testing.T) {
	if Truncate("hello", 10) != "hello" || Truncate("hello world", 5) != "hello…" {
		t.Fatal("truncate")
	}
	tb := &TailBuffer{Max: 8}
	_, _ = tb.Write([]byte("0123456789"))
	_, _ = tb.Write([]byte("ab"))
	if got := tb.String(); got != "456789ab" {
		t.Fatalf("tail %q", got)
	}
	if EstimateTokens("") != 0 || EstimateTokens("abcdefgh") != 3 {
		t.Fatalf("estimate %d", EstimateTokens("abcdefgh"))
	}
}

func rpcCall(t *testing.T, url string, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestBridgeProtocol(t *testing.T) {
	b, err := NewBridge()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	reg := b.Register([]Tool{{Name: "get_weather", Description: "w", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)}})
	url := reg.URL

	code, out := rpcCall(t, url, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	if code != 200 || out["result"].(map[string]any)["protocolVersion"] != "2025-11-25" {
		t.Fatalf("initialize: %d %v", code, out)
	}
	code, _ = rpcCall(t, url, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if code != http.StatusAccepted {
		t.Fatalf("notification status %d", code)
	}
	_, out = rpcCall(t, url, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := out["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "get_weather" {
		t.Fatalf("tools/list: %v", out)
	}
	_, out = rpcCall(t, url, `{"jsonrpc":"2.0","id":3,"method":"server/discover"}`)
	if out["error"] == nil {
		t.Fatalf("unknown method should error: %v", out)
	}
	resp, _ := http.Get(url)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status %d", resp.StatusCode)
	}

	// tools/call is reported on Calls and then held until Close.
	done := make(chan map[string]any, 1)
	go func() {
		_, o := rpcCall(t, url, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_weather","arguments":{"city":"Oslo"}}}`)
		done <- o
	}()
	select {
	case c := <-reg.Calls:
		if c.Name != "get_weather" || string(c.Arguments) != `{"city":"Oslo"}` {
			t.Fatalf("call %+v", c)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tools/call was not reported")
	}
	select {
	case <-done:
		t.Fatal("tools/call returned before Close")
	case <-time.After(200 * time.Millisecond):
	}
	reg.Close()
	reg.Close() // idempotent
	select {
	case o := <-done:
		if o["result"].(map[string]any)["isError"] != true {
			t.Fatalf("tools/call result %v", o)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tools/call did not return after Close")
	}
	if code, _ := rpcCall(t, url, `{"jsonrpc":"2.0","id":5,"method":"ping"}`); code != 404 {
		t.Fatalf("closed path should 404, got %d", code)
	}
}

func TestSlotsBusy(t *testing.T) {
	s := NewSlots(1)
	rel, err := s.Acquire(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if s.InFlight() != 1 {
		t.Fatalf("in flight %d", s.InFlight())
	}
	_, err = s.Acquire(context.Background(), 50*time.Millisecond)
	var ce *Error
	if !errors.As(err, &ce) || ce.Status != 429 {
		t.Fatalf("expected busy, got %v", err)
	}
	rel()
	rel() // idempotent
	if s.InFlight() != 0 {
		t.Fatalf("in flight %d", s.InFlight())
	}
	rel2, err := s.Acquire(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	rel2()
}

func TestEnvBuilder(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "secret")
	t.Setenv("KEEP_ME", "1")
	t.Setenv("TERM", "dumb")
	t.Setenv("HTTPS_PROXY", "http://proxy:3128")
	t.Setenv("NO_PROXY", "example.com")
	t.Setenv("PRESET", "mine")
	t.Setenv("CURSOR_CONFIG_DIR", "/shared/cursor")
	t.Setenv("XDG_CONFIG_HOME", "/shared/config")
	t.Setenv("CLAUDE_CONFIG_DIR", "/shared/claude")
	e := &EnvBuilder{Drop: []string{"ADMIN_PASSWORD"}, Defaults: map[string]string{"PRESET": "default", "NEW": "x"}}
	env := e.Base("/data/home")
	has := func(kv string) bool { return slices.Contains(env, kv) }
	for _, want := range []string{"HOME=/data/home", "KEEP_ME=1", "PRESET=mine", "NEW=x", "NO_PROXY=example.com,127.0.0.1,localhost"} {
		if !has(want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "ADMIN_PASSWORD=") || strings.HasPrefix(kv, "TERM=") || strings.Contains(kv, "/shared/") {
			t.Errorf("should be dropped: %s", kv)
		}
	}
}

func TestTokenStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tok")
	validate := func(s string) error {
		if !strings.HasPrefix(s, "sk-") {
			return errors.New("bad")
		}
		return nil
	}
	ts := NewTokenStore(path, "sk-env-token-0123456789", validate)
	if tok, src := ts.Token(); src != "env" || tok != "sk-env-token-0123456789" {
		t.Fatalf("got %q %q", tok, src)
	}
	v0 := ts.Version()
	if err := ts.Save("not-a-token"); err == nil {
		t.Fatal("expected validation error")
	}
	if err := ts.Save("sk has spaces"); err == nil {
		t.Fatal("expected space error")
	}
	if err := ts.Save(fakeclaude.ValidToken); err != nil {
		t.Fatal(err)
	}
	if tok, src := ts.Token(); src != "login" || tok != fakeclaude.ValidToken || ts.Version() == v0 || ts.SavedAt().IsZero() {
		t.Fatalf("got %q %q", tok, src)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm %v", fi.Mode().Perm())
	}
	if tok, _ := NewTokenStore(path, "", nil).Token(); tok != fakeclaude.ValidToken {
		t.Fatal("token not persisted")
	}
	v1 := ts.Version()
	ts.Touch()
	if ts.Version() == v1 {
		t.Fatal("touch should bump the version")
	}
	_ = ts.Clear()
	if _, src := ts.Token(); src != "env" {
		t.Fatalf("after clear src %q", src)
	}
	if Hint("short") != "" || Hint(fakeclaude.ValidToken) != fakeclaude.ValidToken[:16]+"…"+fakeclaude.ValidToken[len(fakeclaude.ValidToken)-4:] {
		t.Fatal("hint")
	}
}

// stubBackend answers from a function so Execute can be tested without a CLI.
type stubBackend struct {
	runs atomic.Int32
	err  error
}

func (s *stubBackend) Provider() string                         { return "stub" }
func (s *stubBackend) Label() string                            { return "Stub" }
func (s *stubBackend) Login(*Account) LoginSpec                 { return LoginSpec{} }
func (s *stubBackend) Logout(context.Context, *Account) error   { return nil }
func (s *stubBackend) ValidateCredential(string) error          { return nil }
func (s *stubBackend) CredentialEnv() string                    { return "STUB_KEY" }
func (s *stubBackend) Models() []string                         { return []string{"m"} }
func (s *stubBackend) DefaultModel() string                     { return "m" }
func (s *stubBackend) Accepts(model string) bool                { return model == "m" }
func (s *stubBackend) Version(context.Context, []string) string { return "1" }
func (s *stubBackend) Run(context.Context, *Account, RunOptions, Callbacks) (*Result, error) {
	s.runs.Add(1)
	if s.err != nil {
		return nil, s.err
	}
	return &Result{Text: "ok"}, nil
}

func testAccount(t *testing.T) *Account {
	return &Account{ID: "sub1", Name: "Stub", Home: t.TempDir(),
		Tokens: NewTokenStore(filepath.Join(t.TempDir(), "cred"), "", nil), Status: &Status{},
		BaseEnv: func() []string { return []string{"HOME=/x"} }}
}

func TestAccountEnv(t *testing.T) {
	acc := testAccount(t)
	b := &stubBackend{}
	if env := acc.Env(b); len(env) != 1 {
		t.Fatalf("no credential, env %v", env)
	}
	_ = acc.Tokens.Save("key-123")
	if env := acc.Env(b); !slices.Contains(env, "STUB_KEY=key-123") {
		t.Fatalf("env %v", env)
	}
}

func TestExecuteFailFast(t *testing.T) {
	acc := testAccount(t)
	b := &stubBackend{err: &Error{Kind: "auth", Status: 401, Message: "Stub login required"}}
	opts := RunOptions{Model: "m"}
	_, err := Execute(context.Background(), b, acc, opts, Callbacks{})
	var ce *Error
	if !errors.As(err, &ce) || ce.Kind != "auth" || ce.Subscription != "sub1" {
		t.Fatalf("err %v", err)
	}
	if a := acc.Status.Auth(acc.Tokens.Version()); a.State != "login_required" || a.Source != "request" {
		t.Fatalf("auth %+v", a)
	}

	// Same credential: fail fast without running the backend.
	if _, err := Execute(context.Background(), b, acc, opts, Callbacks{}); !errors.As(err, &ce) || ce.Subscription != "sub1" {
		t.Fatalf("fail-fast err %v", err)
	}
	if b.runs.Load() != 1 {
		t.Fatalf("runs %d", b.runs.Load())
	}

	// A probe always runs.
	probe := opts
	probe.Probe = true
	_, _ = Execute(context.Background(), b, acc, probe, Callbacks{})
	if b.runs.Load() != 2 || acc.Status.Auth(acc.Tokens.Version()).Source != "check" {
		t.Fatalf("runs %d auth %+v", b.runs.Load(), acc.Status.Auth(acc.Tokens.Version()))
	}

	// A new credential resets the state and the backend is tried again.
	_ = acc.Tokens.Save("new-key")
	if a := acc.Status.Auth(acc.Tokens.Version()); a.State != "unknown" {
		t.Fatalf("state after new credential %+v", a)
	}
	b.err = nil
	if _, err := Execute(context.Background(), b, acc, opts, Callbacks{}); err != nil {
		t.Fatal(err)
	}
	if a := acc.Status.Auth(acc.Tokens.Version()); a.State != "valid" || a.CheckedAt == nil {
		t.Fatalf("auth %+v", a)
	}
	if snap := acc.Status.Snapshot(); snap.Completed != 1 || snap.Failed != 2 {
		t.Fatalf("snapshot %+v", snap)
	}
}

func TestExecuteOtherErrorsKeepLogin(t *testing.T) {
	acc := testAccount(t)
	b := &stubBackend{err: &Error{Kind: "rate_limit", Status: 429, Message: "limit"}}
	_, _ = Execute(context.Background(), b, acc, RunOptions{}, Callbacks{})
	if a := acc.Status.Auth(acc.Tokens.Version()); a.State != "unknown" {
		t.Fatalf("auth %+v", a)
	}
	if snap := acc.Status.Snapshot(); snap.LastError != "limit" {
		t.Fatalf("snapshot %+v", snap)
	}
}

// --- login manager ---------------------------------------------------------------

func waitState(t *testing.T, m *LoginManager, sub string, want LoginState) LoginStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st := m.Status(sub)
		if st.State == want {
			return st
		}
		time.Sleep(50 * time.Millisecond)
	}
	st := m.Status(sub)
	t.Fatalf("state %q (want %q), error %q, output:\n%s", st.State, want, st.Error, st.Output)
	return st
}

// codeSpec is a sign-in like Claude's: URL, pasted code, printed token.
func codeSpec(t *testing.T, ts *TokenStore) LoginSpec {
	exe, _ := os.Executable()
	tokenRe := func(raw string) string {
		plain := StripANSI(raw)
		if i := strings.Index(plain, fakeclaude.TokenPrefix); i >= 0 {
			return strings.Fields(plain[i:])[0]
		}
		return ""
	}
	return LoginSpec{
		Bin: exe, Args: []string{"setup-token"}, NeedsCode: true,
		Env:    append(os.Environ(), fakeclaude.EnvScenario+"=auto"),
		Token:  tokenRe,
		Redact: func(s string) string { return strings.ReplaceAll(s, fakeclaude.TokenPrefix, "[redacted]") },
		Save:   ts.Save,
	}
}

// browserSpec is a sign-in like Cursor's: URL, then the CLI exits by itself.
func browserSpec(t *testing.T, mode string, done func()) LoginSpec {
	exe, _ := os.Executable()
	link := filepath.Join(t.TempDir(), "cursor-agent")
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	return LoginSpec{
		Bin: link, Args: []string{"login"},
		Env:  append(os.Environ(), fakecursor.EnvMode+"=auto", fakecursor.EnvLogin+"="+mode, "HOME="+t.TempDir(), "NO_OPEN_BROWSER=1"),
		Done: done,
	}
}

func TestLoginWithCode(t *testing.T) {
	m := NewLoginManager()
	ts := NewTokenStore(filepath.Join(t.TempDir(), "tok"), "", nil)
	st, err := m.Start("sub1", codeSpec(t, ts))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(st.URL, fakeclaude.LoginURLPrefix) || st.State != LoginWaitCode || !st.NeedsCode || st.Subscription != "sub1" {
		t.Fatalf("start status %+v", st)
	}
	if other := m.Status("sub2"); other.State != LoginIdle {
		t.Fatalf("other subscription %+v", other)
	}
	if err := m.SubmitCode("sub2", fakeclaude.CodePrefix); err == nil {
		t.Fatal("a code for another subscription must be refused")
	}
	if err := m.SubmitCode("sub1", "bad\ncode"); err == nil {
		t.Fatal("newlines must be refused")
	}
	if err := m.SubmitCode("sub1", fakeclaude.CodePrefix+"-123#state"); err != nil {
		t.Fatal(err)
	}
	st = waitState(t, m, "sub1", LoginSuccess)
	tok, src := ts.Token()
	if !strings.HasPrefix(tok, fakeclaude.TokenPrefix) || src != "login" {
		t.Fatalf("token %q %q", tok, src)
	}
	if strings.Contains(st.Output, tok) {
		t.Fatal("token must be redacted in output shown to the browser")
	}
}

func TestLoginBadCode(t *testing.T) {
	m := NewLoginManager()
	ts := NewTokenStore(filepath.Join(t.TempDir(), "tok"), "", nil)
	if _, err := m.Start("sub1", codeSpec(t, ts)); err != nil {
		t.Fatal(err)
	}
	if err := m.SubmitCode("sub1", "wrong"); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, m, "sub1", LoginFailed)
	if !strings.Contains(st.Output, "Invalid code") || st.Error == "" {
		t.Fatalf("status %+v", st)
	}
	if tok, _ := ts.Token(); tok != "" {
		t.Fatal("no token expected")
	}
	if err := m.SubmitCode("sub1", "again"); err == nil {
		t.Fatal("submitting to a finished login should fail")
	}
}

func TestLoginInBrowser(t *testing.T) {
	m := NewLoginManager()
	var done atomic.Bool
	st, err := m.Start("cur", browserSpec(t, "", func() { done.Store(true) }))
	if err != nil {
		t.Fatal(err)
	}
	if st.State != LoginWaitBrowse || st.NeedsCode || !strings.HasPrefix(st.URL, fakecursor.LoginURLPrefix) {
		t.Fatalf("start status %+v", st)
	}
	if err := m.SubmitCode("cur", "x"); err == nil {
		t.Fatal("a browser sign-in takes no code")
	}
	waitState(t, m, "cur", LoginSuccess)
	if !done.Load() {
		t.Fatal("Done was not called")
	}
}

func TestLoginInBrowserDenied(t *testing.T) {
	m := NewLoginManager()
	var done atomic.Bool
	if _, err := m.Start("cur", browserSpec(t, "deny", func() { done.Store(true) })); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, m, "cur", LoginFailed)
	if !strings.Contains(st.Output, "denied") || done.Load() {
		t.Fatalf("status %+v done %v", st, done.Load())
	}
}

func TestLoginCancelAndSwitch(t *testing.T) {
	m := NewLoginManager()
	if _, err := m.Start("a", browserSpec(t, "hang", nil)); err != nil {
		t.Fatal(err)
	}
	m.Cancel("other") // not the running one: no effect
	if st := m.Status("a"); st.State != LoginWaitBrowse {
		t.Fatalf("state %q", st.State)
	}
	// Starting another subscription's sign-in replaces the running one.
	if _, err := m.Start("b", browserSpec(t, "hang", nil)); err != nil {
		t.Fatal(err)
	}
	if st := m.Status("a"); st.State != LoginIdle {
		t.Fatalf("a state %q", st.State)
	}
	m.Cancel("b")
	if st := m.Status("b"); st.State != LoginIdle {
		t.Fatalf("b state %q", st.State)
	}
	m.CancelAll()
}

func TestReadLineLimit(t *testing.T) {
	rd := bufio.NewReaderSize(strings.NewReader("short\n"+strings.Repeat("x", 100)+"\nend"), 16)
	if l, err := ReadLine(rd, 50); err != nil || string(l) != "short\n" {
		t.Fatalf("%q %v", l, err)
	}
	if _, err := ReadLine(rd, 50); err == nil {
		t.Fatal("a line over the limit must fail")
	}
	if !ValidModelName("claude-sonnet-4-5[1m]") || !ValidModelName("vendor/model:tag") || ValidModelName("-x") || ValidModelName("") {
		t.Fatal("model name rules")
	}
}

func TestLoginURLHostRestriction(t *testing.T) {
	m := NewLoginManager()
	ts := NewTokenStore(filepath.Join(t.TempDir(), "tok"), "", nil)
	spec := codeSpec(t, ts)
	spec.URLHosts = []string{"example.org"} // the fake prints a claude.ai link
	st, err := m.Start("sub1", spec)
	if err != nil {
		t.Fatal(err)
	}
	defer m.CancelAll()
	deadline := time.Now().Add(5 * time.Second)
	for st.Error == "" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		st = m.Status("sub1")
	}
	if st.URL != "" || !strings.Contains(st.Error, "claude.ai") {
		t.Fatalf("link to another host must not be shown: %+v", st)
	}
	if !hostAllowed("https://login.claude.ai/x", []string{"claude.ai"}) || hostAllowed("https://claude.ai.evil.example/x", []string{"claude.ai"}) {
		t.Fatal("host matching")
	}
}
