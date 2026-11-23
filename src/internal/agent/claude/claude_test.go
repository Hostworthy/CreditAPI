package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atb00ker/CreditAPI/src/internal/agent"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakeclaude"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakes"
)

func TestMain(m *testing.M) {
	fakes.MaybeRun()
	os.Exit(m.Run())
}

func TestExtractToken(t *testing.T) {
	out := "Your OAuth token (valid for 1 year):\r\r\n\r\r\n\x1b[33m" + fakeclaude.ValidToken + "\x1b[39m\r\r\n\r\r\nStore this token securely."
	if got := ExtractToken(out); got != fakeclaude.ValidToken {
		t.Fatalf("got %q", got)
	}
	if got := ExtractToken("nothing"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestAcceptsAndCredential(t *testing.T) {
	b := &Backend{Default: "sonnet", List: []string{"opus", "sonnet", "my-model"}}
	for _, m := range []string{"opus", "haiku", "fable", "claude-sonnet-4-6", "sonnet[1m]", "my-model"} {
		if !b.Accepts(m) {
			t.Errorf("should accept %q", m)
		}
	}
	for _, m := range []string{"gpt-4o", "auto", ""} {
		if b.Accepts(m) {
			t.Errorf("should not accept %q", m)
		}
	}
	if b.ValidateCredential(fakeclaude.ValidToken) != nil || b.ValidateCredential("sk-other") == nil {
		t.Fatal("credential validation")
	}
	if b.Provider() != Provider || b.Label() != "Claude" || b.CredentialEnv() != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Fatal("identity")
	}
}

// newTestBackend returns a backend running the test binary as the fake CLI in
// the given scenario, an account signed in with token, and the record path.
func newTestBackend(t *testing.T, scenario, token string) (*Backend, *agent.Account, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "record.json")
	bridge, err := agent.NewBridge()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	b := &Backend{Bin: exe, WorkDir: dir, Bridge: bridge, Default: "sonnet", List: []string{"opus", "sonnet", "haiku"}}
	acc := &agent.Account{ID: "sub1", Name: "Claude", Provider: Provider, Home: filepath.Join(dir, "home"),
		Tokens: agent.NewTokenStore(filepath.Join(dir, "credential"), token, nil), Status: &agent.Status{},
		BaseEnv: func() []string {
			return append(os.Environ(), fakeclaude.EnvScenario+"="+scenario, fakeclaude.EnvRecord+"="+record, "HOME="+filepath.Join(dir, "home"))
		}}
	return b, acc, record
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

func text(s string) []agent.ContentBlock { return []agent.ContentBlock{{Type: "text", Text: s}} }

func TestRunText(t *testing.T) {
	b, acc, record := newTestBackend(t, "text", "tok")
	var deltas []string
	res, err := b.Run(context.Background(), acc, agent.RunOptions{
		Model: "sonnet", SystemPrompt: "be nice", MaxOutputTokens: 1234, Effort: "low", Content: text("hi"),
	}, agent.Callbacks{Text: func(s string) { deltas = append(deltas, s) }})
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
	if rec.SystemPrompt != "be nice" || rec.Env["CLAUDE_CODE_MAX_OUTPUT_TOKENS"] != "1234" || rec.Env["CLAUDE_CODE_OAUTH_TOKEN"] != "tok" {
		t.Fatalf("record %+v", rec)
	}
	if rec.Env["HOME"] != acc.Home {
		t.Fatalf("HOME %q, want the account home %q", rec.Env["HOME"], acc.Home)
	}
	args := strings.Join(rec.Args, " ")
	for _, want := range []string{"--tools  ", "--model sonnet", "--max-turns 1", "--no-session-persistence", "--strict-mcp-config", "--effort low", "--setting-sources  "} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
	if strings.Contains(args, "--mcp-config") {
		t.Errorf("no tools, so no MCP config expected: %s", args)
	}
	if snap := acc.Status.Snapshot(); snap.RateLimit == nil {
		t.Errorf("rate limit not recorded: %+v", snap)
	}
}

func TestRunToolCalls(t *testing.T) {
	b, acc, record := newTestBackend(t, "tools", "tok")
	var started []string
	var args bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	res, err := b.Run(ctx, acc, agent.RunOptions{
		Model:     "sonnet",
		Content:   text("weather?"),
		Tools:     []agent.Tool{{Name: "get_weather_v2", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)}},
		ToolNames: map[string]string{"get_weather_v2": "get.weather.v2"},
	}, agent.Callbacks{
		ToolStart: func(i int, id, name string) { started = append(started, name) },
		ToolArgs:  func(i int, d string) { args.WriteString(d) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("claude was not stopped after tool_use")
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

func TestRunStructuredOutput(t *testing.T) {
	b, acc, record := newTestBackend(t, "schema", "tok")
	var deltas []string
	res, err := b.Run(context.Background(), acc, agent.RunOptions{
		Model: "haiku", JSONSchema: json.RawMessage(`{"type":"object"}`), Content: text("weather in Paris"),
	}, agent.Callbacks{Text: func(s string) { deltas = append(deltas, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.StructuredOutput) != `{"city":"Paris","temp_c":21}` {
		t.Fatalf("structured %s", res.StructuredOutput)
	}
	if len(deltas) != 0 {
		t.Fatalf("text should be suppressed in JSON mode: %v", deltas)
	}
	args := strings.Join(readRecord(t, record).Args, " ")
	if !strings.Contains(args, `--json-schema {"type":"object"}`) || !strings.Contains(args, "--max-turns 3") {
		t.Fatalf("args %s", args)
	}
}

func TestRunAuthErrorThroughExecute(t *testing.T) {
	b, acc, record := newTestBackend(t, "auth", "tok")
	opts := agent.RunOptions{Model: "sonnet", Content: text("x")}
	_, err := agent.Execute(context.Background(), b, acc, opts, agent.Callbacks{})
	var ce *agent.Error
	if !errors.As(err, &ce) || ce.Kind != "auth" || ce.Status != 401 || ce.Subscription != "sub1" {
		t.Fatalf("err %v", err)
	}
	if !strings.HasPrefix(ce.Message, "Claude login required") {
		t.Fatalf("message %q", ce.Message)
	}
	// Same token: fail fast without starting claude.
	_ = os.Remove(record)
	if _, err := agent.Execute(context.Background(), b, acc, opts, agent.Callbacks{}); !errors.As(err, &ce) || ce.Kind != "auth" {
		t.Fatalf("fail-fast err %v", err)
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("claude was started although the login is known to be broken")
	}
}

func TestRunNotLoggedIn(t *testing.T) {
	b, acc, _ := newTestBackend(t, "auto", "")
	_, err := b.Run(context.Background(), acc, agent.RunOptions{Model: "sonnet", Content: text("x")}, agent.Callbacks{})
	var ce *agent.Error
	if !errors.As(err, &ce) || ce.Kind != "auth" {
		t.Fatalf("err %v", err)
	}
}

func TestRunAutoReplies(t *testing.T) {
	b, acc, _ := newTestBackend(t, "auto", fakeclaude.ValidToken)
	res, err := b.Run(context.Background(), acc, agent.RunOptions{Model: "sonnet", Content: text("[fake:whoami]")}, agent.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	// The fake reports the directory holding HOME, which is the test dir here.
	if !strings.HasPrefix(res.Text, "Served by claude subscription ") {
		t.Fatalf("reply %q", res.Text)
	}
	_, err = b.Run(context.Background(), acc, agent.RunOptions{Model: "sonnet", Content: text("[fake:rate-limit]")}, agent.Callbacks{})
	var ce *agent.Error
	if !errors.As(err, &ce) || ce.Kind != "rate_limit" || !ce.Retryable() {
		t.Fatalf("err %v", err)
	}
}

func TestRunCrash(t *testing.T) {
	b, acc, _ := newTestBackend(t, "crash", "tok")
	_, err := b.Run(context.Background(), acc, agent.RunOptions{Model: "sonnet", Content: text("x")}, agent.Callbacks{})
	var ce *agent.Error
	if !errors.As(err, &ce) || !strings.Contains(ce.Message, "boom") {
		t.Fatalf("err %v", err)
	}
}

func TestRunCancel(t *testing.T) {
	b, acc, _ := newTestBackend(t, "slow", "tok")
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		_, err := b.Run(ctx, acc, agent.RunOptions{Model: "sonnet", Content: text("x")}, agent.Callbacks{
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

func TestLoginSpec(t *testing.T) {
	b, acc, _ := newTestBackend(t, "auto", "")
	m := agent.NewLoginManager()
	st, err := m.Start(acc.ID, b.Login(acc))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(st.URL, fakeclaude.LoginURLPrefix) || st.State != agent.LoginWaitCode {
		t.Fatalf("start %+v", st)
	}
	if err := m.SubmitCode(acc.ID, fakeclaude.CodePrefix+"-1#s"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for m.Status(acc.ID).State != agent.LoginSuccess && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	st = m.Status(acc.ID)
	tok, src := acc.Tokens.Token()
	if st.State != agent.LoginSuccess || !strings.HasPrefix(tok, fakeclaude.TokenPrefix) || src != "login" {
		t.Fatalf("status %+v token %q %q", st, tok, src)
	}
	if strings.Contains(st.Output, tok) {
		t.Fatal("token shown to the browser")
	}
	if err := b.Logout(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	if v := b.Version(context.Background(), acc.BaseEnv()); !strings.Contains(v, "fake") {
		t.Fatalf("version %q", v)
	}
}
