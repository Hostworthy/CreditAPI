package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atb00ker/claude-max-to-api/internal/fakeclaude"
)

func TestMain(m *testing.M) {
	fakeclaude.MaybeRun()
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

func TestExtractToken(t *testing.T) {
	out := "Your OAuth token (valid for 1 year):\r\r\n\r\r\n\x1b[33m" + fakeclaude.FakeToken + "\x1b[39m\r\r\n\r\r\nStore this token securely."
	if got := ExtractToken(out); got != fakeclaude.FakeToken {
		t.Fatalf("got %q", got)
	}
	if got := ExtractToken("nothing"); got != "" {
		t.Fatalf("got %q", got)
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
	if ExtractJSON("no json") != nil {
		t.Error("expected nil")
	}
}

func TestClassify(t *testing.T) {
	if e := classify(401, "Failed to authenticate"); e.Kind != "auth" || e.Status != 503 {
		t.Fatalf("auth: %+v", e)
	}
	if e := classify(0, "Invalid API key · Please run /login"); e.Kind != "auth" {
		t.Fatalf("login: %+v", e)
	}
	if e := classify(429, "too many"); e.Kind != "rate_limit" || e.Status != 429 {
		t.Fatalf("rate: %+v", e)
	}
	if e := classify(0, "weird"); e.Kind != "upstream" || e.Status != 502 {
		t.Fatalf("other: %+v", e)
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
	url, unregister := b.Register([]Tool{{Name: "get_weather", Description: "w", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)}})

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

	// tools/call blocks until the run is unregistered.
	done := make(chan map[string]any, 1)
	go func() {
		_, o := rpcCall(t, url, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_weather","arguments":{}}}`)
		done <- o
	}()
	select {
	case <-done:
		t.Fatal("tools/call returned before unregister")
	case <-time.After(200 * time.Millisecond):
	}
	unregister()
	select {
	case o := <-done:
		if o["result"].(map[string]any)["isError"] != true {
			t.Fatalf("tools/call result %v", o)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tools/call did not return after unregister")
	}
	if code, _ := rpcCall(t, url, `{"jsonrpc":"2.0","id":5,"method":"ping"}`); code != 404 {
		t.Fatalf("unregistered path should 404, got %d", code)
	}
}

func newTestRunner(t *testing.T, scenario string) (*Runner, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "record.json")
	b, err := NewBridge()
	if err != nil {
		t.Fatal(err)
	}
	r := NewRunner(2)
	r.Bin = exe
	r.WorkDir = dir
	r.MaxTurns = 5
	r.Bridge = b
	r.Status = &Status{}
	r.Env = func() []string {
		return append(os.Environ(), fakeclaude.EnvScenario+"="+scenario, fakeclaude.EnvRecord+"="+record, "CLAUDE_CODE_OAUTH_TOKEN=tok")
	}
	return r, record
}

func readRecord(t *testing.T, path string) fakeclaude.Record {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec fakeclaude.Record
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestRunnerText(t *testing.T) {
	r, record := newTestRunner(t, "text")
	var deltas []string
	res, err := r.Run(context.Background(), RunOptions{
		Model: "sonnet", SystemPrompt: "be nice", MaxOutputTokens: 1234,
		Content: []ContentBlock{{Type: "text", Text: "hi"}},
	}, Callbacks{Text: func(s string) { deltas = append(deltas, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Hello world" || strings.Join(deltas, "") != "Hello world" || res.FinishReason != "stop" {
		t.Fatalf("result %+v deltas %v", res, deltas)
	}
	if res.Usage.OutputTokens != 5 || res.Usage.PromptTokens() != 110 {
		t.Fatalf("usage %+v", res.Usage)
	}
	rec := readRecord(t, record)
	if rec.SystemPrompt != "be nice" || rec.Env["CLAUDE_CODE_MAX_OUTPUT_TOKENS"] != "1234" {
		t.Fatalf("record %+v", rec)
	}
	args := strings.Join(rec.Args, " ")
	for _, want := range []string{"--tools  ", "--model sonnet", "--max-turns 1", "--no-session-persistence", "--strict-mcp-config"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
	if strings.Contains(args, "--mcp-config") {
		t.Errorf("no tools, so no MCP config expected: %s", args)
	}
	if snap := r.Status.Snapshot(); snap.RateLimit == nil || snap.Completed != 1 {
		t.Errorf("status %+v", snap)
	}
}

func TestRunnerToolCalls(t *testing.T) {
	r, record := newTestRunner(t, "tools")
	var started []string
	var args bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	res, err := r.Run(ctx, RunOptions{
		Model:     "sonnet",
		Content:   []ContentBlock{{Type: "text", Text: "weather?"}},
		Tools:     []Tool{{Name: "get_weather_v2", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)}},
		ToolNames: map[string]string{"get_weather_v2": "get.weather.v2"},
	}, Callbacks{
		ToolStart: func(i int, id, name string) { started = append(started, name) },
		ToolArgs:  func(i int, d string) { args.WriteString(d) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("runner did not stop claude after tool_use")
	}
	if res.FinishReason != "tool_calls" || len(res.ToolCalls) != 2 {
		t.Fatalf("result %+v", res)
	}
	c := res.ToolCalls[0]
	if c.Name != "get.weather.v2" || c.Arguments != `{"city":"Paris"}` || c.ID != "call_1" {
		t.Fatalf("call %+v", c)
	}
	if res.ToolCalls[1].Arguments != `{"city":"Rome"}` || res.Text != "Checking." {
		t.Fatalf("result %+v", res)
	}
	if len(started) != 2 || args.String() != `{"city":"Paris"}{"city":"Rome"}` {
		t.Fatalf("callbacks %v %q", started, args.String())
	}
	rec := readRecord(t, record)
	if len(rec.BridgeTools) != 1 || rec.BridgeTools[0] != "get_weather_v2" {
		t.Fatalf("bridge tools %v", rec.BridgeTools)
	}
	if !strings.Contains(strings.Join(rec.Args, " "), "--allowedTools mcp__oai") {
		t.Fatalf("args %v", rec.Args)
	}
}

func TestRunnerStructuredOutput(t *testing.T) {
	r, record := newTestRunner(t, "schema")
	var deltas []string
	res, err := r.Run(context.Background(), RunOptions{
		Model: "haiku", JSONSchema: json.RawMessage(`{"type":"object"}`),
		Content: []ContentBlock{{Type: "text", Text: "banana"}},
	}, Callbacks{Text: func(s string) { deltas = append(deltas, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.StructuredOutput) != `{"calories":105,"food":"banana"}` {
		t.Fatalf("structured %s", res.StructuredOutput)
	}
	if len(deltas) != 0 {
		t.Fatalf("text should be suppressed in JSON mode: %v", deltas)
	}
	args := strings.Join(readRecord(t, record).Args, " ")
	if !strings.Contains(args, `--json-schema {"type":"object"}`) || !strings.Contains(args, "--max-turns 5") {
		t.Fatalf("args %s", args)
	}
}

func TestRunnerAuthError(t *testing.T) {
	r, _ := newTestRunner(t, "auth")
	_, err := r.Run(context.Background(), RunOptions{Model: "sonnet", Content: []ContentBlock{{Type: "text", Text: "x"}}}, Callbacks{})
	var ce *Error
	if !errors.As(err, &ce) || ce.Kind != "auth" || ce.Status != 503 {
		t.Fatalf("err %v", err)
	}
	if !strings.Contains(ce.Message, "/login") {
		t.Fatalf("message should point to /login: %s", ce.Message)
	}
}

func TestRunnerCrash(t *testing.T) {
	r, _ := newTestRunner(t, "crash")
	_, err := r.Run(context.Background(), RunOptions{Model: "sonnet", Content: []ContentBlock{{Type: "text", Text: "x"}}}, Callbacks{})
	var ce *Error
	if !errors.As(err, &ce) || !strings.Contains(ce.Message, "boom") {
		t.Fatalf("err %v", err)
	}
}

func TestRunnerCancel(t *testing.T) {
	r, _ := newTestRunner(t, "slow")
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, RunOptions{Model: "sonnet", Content: []ContentBlock{{Type: "text", Text: "x"}}}, Callbacks{
			Text: func(s string) {
				if strings.Contains(s, "two") {
					cancel()
				}
			},
		})
		got <- err
	}()
	select {
	case err := <-got:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop the run")
	}
}

func TestAcquireBusy(t *testing.T) {
	r := NewRunner(1)
	r.Status = &Status{}
	rel, err := r.Acquire(context.Background(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Acquire(context.Background(), 50*time.Millisecond)
	var ce *Error
	if !errors.As(err, &ce) || ce.Status != 429 {
		t.Fatalf("expected busy, got %v", err)
	}
	rel()
	rel() // idempotent
	if rel2, err := r.Acquire(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	} else {
		rel2()
	}
}

func TestTokenStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tok")
	ts := NewTokenStore(path, "sk-ant-oat01-envenvenvenvenvenvenvenvenvenvenvenvenv")
	if _, src := ts.Token(); src != "env" {
		t.Fatalf("src %q", src)
	}
	if err := ts.Save("not-a-token"); err == nil {
		t.Fatal("expected validation error")
	}
	if err := ts.Save(fakeclaude.FakeToken); err != nil {
		t.Fatal(err)
	}
	if tok, src := ts.Token(); src != "login" || tok != fakeclaude.FakeToken {
		t.Fatalf("got %q %q", tok, src)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm %v", fi.Mode().Perm())
	}
	if tok, _ := NewTokenStore(path, "").Token(); tok != fakeclaude.FakeToken {
		t.Fatal("token not persisted")
	}
	_ = ts.Clear()
	if _, src := ts.Token(); src != "env" {
		t.Fatalf("after clear src %q", src)
	}
}

func waitState(t *testing.T, m *LoginManager, want LoginState) LoginStatus {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st := m.Status()
		if st.State == want {
			return st
		}
		time.Sleep(50 * time.Millisecond)
	}
	st := m.Status()
	t.Fatalf("state %q (want %q), error %q, output:\n%s", st.State, want, st.Error, st.Output)
	return st
}

func newTestLogin(t *testing.T) (*LoginManager, *TokenStore) {
	exe, _ := os.Executable()
	ts := NewTokenStore(filepath.Join(t.TempDir(), "tok"), "")
	m := NewLoginManager(exe, func() []string {
		return append(os.Environ(), fakeclaude.EnvScenario+"=setup-token")
	}, ts)
	return m, ts
}

func TestLoginFlow(t *testing.T) {
	m, ts := newTestLogin(t)
	st, err := m.Start()
	if err != nil {
		t.Fatal(err)
	}
	if st.URL != fakeclaude.FakeLoginURL || st.State != LoginWaitCode {
		t.Fatalf("start status %+v", st)
	}
	if err := m.SubmitCode("good-code#state"); err != nil {
		t.Fatal(err)
	}
	st = waitState(t, m, LoginSuccess)
	if tok, src := ts.Token(); tok != fakeclaude.FakeToken || src != "login" {
		t.Fatalf("token %q %q", tok, src)
	}
	if strings.Contains(st.Output, fakeclaude.FakeToken) {
		t.Fatal("token must be redacted in output shown to the browser")
	}
}

func TestLoginBadCode(t *testing.T) {
	m, ts := newTestLogin(t)
	if _, err := m.Start(); err != nil {
		t.Fatal(err)
	}
	if err := m.SubmitCode("wrong"); err != nil {
		t.Fatal(err)
	}
	st := waitState(t, m, LoginFailed)
	if !strings.Contains(st.Output, "Invalid code") {
		t.Fatalf("output %q", st.Output)
	}
	if tok, _ := ts.Token(); tok != "" {
		t.Fatal("no token expected")
	}
	if err := m.SubmitCode("again"); err == nil {
		t.Fatal("submitting to a finished login should fail")
	}
}

func TestLoginCancel(t *testing.T) {
	m, _ := newTestLogin(t)
	if _, err := m.Start(); err != nil {
		t.Fatal(err)
	}
	m.Cancel()
	if st := m.Status(); st.State != LoginIdle {
		t.Fatalf("state %q", st.State)
	}
}
