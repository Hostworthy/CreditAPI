package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/atb00ker/CreditAPI/src/internal/agent"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakecursor"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakes"
)

func TestMain(m *testing.M) {
	fakes.MaybeRun()
	os.Exit(m.Run())
}

const pngPixel = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// newTestBackend returns a backend running the test binary as the fake
// Cursor CLI, an account with the given API key, and the record path.
func newTestBackend(t *testing.T, key string) (*Backend, *agent.Account, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "cursor-agent")
	if err := os.Symlink(exe, bin); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(dir, "record.json")
	bridge, err := agent.NewBridge()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	b := &Backend{Bin: bin, Bridge: bridge, Default: "auto", List: []string{"auto", "gpt-5"}, Protected: []string{filepath.Join(dir, "data")}}
	acc := &agent.Account{ID: "cur1", Name: "Cursor", Provider: Provider, Home: home,
		Tokens: agent.NewTokenStore(filepath.Join(dir, "credential"), key, b.ValidateCredential), Status: &agent.Status{},
		BaseEnv: func() []string {
			return append(os.Environ(), fakecursor.EnvMode+"=auto", fakecursor.EnvRecord+"="+record, "HOME="+home)
		}}
	return b, acc, record
}

func readRecord(t *testing.T, path string) fakecursor.Record {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec fakecursor.Record
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func text(s string) []agent.ContentBlock { return []agent.ContentBlock{{Type: "text", Text: s}} }

func TestIdentityAndModels(t *testing.T) {
	b := &Backend{Default: "auto", List: []string{"auto", "GPT-5"}}
	if !b.Accepts("auto") || !b.Accepts("gpt-5") || b.Accepts("sonnet") {
		t.Fatal("accepts")
	}
	if b.Provider() != Provider || b.Label() != "Cursor" || b.CredentialEnv() != "CURSOR_API_KEY" {
		t.Fatal("identity")
	}
	if b.ValidateCredential("short") == nil || b.ValidateCredential(fakecursor.ValidKey) != nil {
		t.Fatal("credential validation")
	}
}

func TestRunStreamsWithoutDuplicates(t *testing.T) {
	b, acc, record := newTestBackend(t, fakecursor.ValidKey)
	var deltas []string
	res, err := b.Run(context.Background(), acc, agent.RunOptions{
		Model: "gpt-5", SystemPrompt: "You are terse.", Content: text("Hello there\nsecond line"),
	}, agent.Callbacks{Text: func(s string) { deltas = append(deltas, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Fake reply to: Hello there" || strings.Join(deltas, "") != res.Text || len(deltas) < 2 {
		t.Fatalf("result %+v deltas %q", res, deltas)
	}
	if res.FinishReason != "stop" || res.Usage.InputTokens == 0 || res.Usage.OutputTokens == 0 {
		t.Fatalf("result %+v", res)
	}
	rec := readRecord(t, record)
	if rec.Instructions != "You are terse." || rec.Env["CURSOR_API_KEY"] != fakecursor.ValidKey || rec.Env["HOME"] != acc.Home {
		t.Fatalf("record %+v", rec)
	}
	args := strings.Join(rec.Args, " ")
	for _, want := range []string{"-p", "--output-format stream-json", "--stream-partial-output", "--model gpt-5", "--trust"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q: %s", want, args)
		}
	}
	if slices.Contains(rec.Args, "--force") || slices.Contains(rec.Args, "--approve-mcps") {
		t.Errorf("unexpected flags: %s", args)
	}
}

func TestHardening(t *testing.T) {
	b, acc, record := newTestBackend(t, fakecursor.ValidKey)
	// Fields the CLI keeps in its global config survive.
	cfgPath := filepath.Join(acc.Home, ".cursor", "cli-config.json")
	_ = os.MkdirAll(filepath.Dir(cfgPath), 0o700)
	_ = os.WriteFile(cfgPath, []byte(`{"version":1,"approvalMode":"unrestricted","editor":{"vimMode":true}}`), 0o600)

	if _, err := b.Run(context.Background(), acc, agent.RunOptions{Model: "auto", Content: text("hi")}, agent.Callbacks{}); err != nil {
		t.Fatal(err)
	}
	rec := readRecord(t, record)
	var project struct {
		Permissions struct {
			Allow []string `json:"allow"`
			Deny  []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(rec.ProjectConfig, &project); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Shell(*)", "Write(**)", "WebFetch(*)", "Read(" + acc.Home + "/**)", "Read(" + b.Protected[0] + "/**)", "Read(/etc/**)"} {
		if !slices.Contains(project.Permissions.Deny, want) {
			t.Errorf("deny missing %q: %v", want, project.Permissions.Deny)
		}
	}
	if !slices.Contains(project.Permissions.Allow, "Mcp(oai:*)") {
		t.Errorf("allow %v", project.Permissions.Allow)
	}
	var global map[string]any
	if err := json.Unmarshal(rec.GlobalConfig, &global); err != nil {
		t.Fatal(err)
	}
	if global["approvalMode"] != "allowlist" || global["version"] != float64(1) || global["editor"] == nil || global["permissions"] == nil {
		t.Fatalf("global config %v", global)
	}
}

func TestRunImagesAsAttachments(t *testing.T) {
	b, acc, record := newTestBackend(t, fakecursor.ValidKey)
	res, err := b.Run(context.Background(), acc, agent.RunOptions{Model: "auto", Content: []agent.ContentBlock{
		{Type: "text", Text: "what is this"},
		{Type: "image", Source: &agent.ImageSource{Type: "base64", MediaType: "image/png", Data: pngPixel}},
	}}, agent.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "I see 1 image(s): image/png 1x1. Fake reply to: what is this" {
		t.Fatalf("reply %q", res.Text)
	}
	rec := readRecord(t, record)
	if len(rec.Attachments) != 1 || rec.Attachments[0] != "image-1.png" || !strings.Contains(rec.Prompt, "./attachments/image-1.png") {
		t.Fatalf("record %+v", rec)
	}
}

func TestRunLongPromptUsesFile(t *testing.T) {
	b, acc, record := newTestBackend(t, fakecursor.ValidKey)
	long := "first line\n" + strings.Repeat("lorem ipsum ", 12000)
	res, err := b.Run(context.Background(), acc, agent.RunOptions{Model: "auto", Content: text(long)}, agent.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "Fake reply to: first line" {
		t.Fatalf("reply %q", res.Text)
	}
	if rec := readRecord(t, record); !strings.Contains(rec.Prompt, "./conversation.md") || len(rec.Prompt) > 1000 {
		t.Fatalf("prompt should point at the file: %q", agent.Truncate(rec.Prompt, 200))
	}
}

func TestRunStructuredOutput(t *testing.T) {
	b, acc, _ := newTestBackend(t, fakecursor.ValidKey)
	var deltas []string
	res, err := b.Run(context.Background(), acc, agent.RunOptions{
		Model: "auto", Content: text("weather in Paris"),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"},"temp_c":{"type":"number"}}}`),
	}, agent.Callbacks{Text: func(s string) { deltas = append(deltas, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.StructuredOutput) != `{"city":"fake city","temp_c":42}` || len(deltas) != 0 {
		t.Fatalf("structured %s deltas %v", res.StructuredOutput, deltas)
	}
}

func TestRunJSONObjectMode(t *testing.T) {
	b, acc, _ := newTestBackend(t, fakecursor.ValidKey)
	res, err := b.Run(context.Background(), acc, agent.RunOptions{
		Model: "auto", Content: text("hi"), JSONObject: true,
		SystemPrompt: "Respond with a single valid JSON object.",
	}, agent.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if string(res.StructuredOutput) != `{"fake":true,"reply":"hi"}` {
		t.Fatalf("structured %s", res.StructuredOutput)
	}
}

func TestRunToolCalls(t *testing.T) {
	b, acc, record := newTestBackend(t, fakecursor.ValidKey)
	var started []string
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	res, err := b.Run(ctx, acc, agent.RunOptions{
		Model:   "auto",
		Content: text("use get_weather_v2 and add_task"),
		Tools: []agent.Tool{
			{Name: "get_weather_v2", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)},
			{Name: "add_task", InputSchema: json.RawMessage(`{"type":"object","properties":{"hours":{"type":"number"}}}`)},
		},
		ToolNames: map[string]string{"get_weather_v2": "get.weather.v2"},
	}, agent.Callbacks{ToolStart: func(i int, id, name string) { started = append(started, name) }})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 8*time.Second {
		t.Fatal("cursor-agent was not stopped after the tool call")
	}
	if res.FinishReason != "tool_calls" || len(res.ToolCalls) != 2 {
		t.Fatalf("result %+v", res)
	}
	if c := res.ToolCalls[0]; c.Name != "get.weather.v2" || c.Arguments != `{"city":"fake city"}` || !strings.HasPrefix(c.ID, "call_") {
		t.Fatalf("call %+v", c)
	}
	if c := res.ToolCalls[1]; c.Name != "add_task" || c.Arguments != `{"hours":42}` {
		t.Fatalf("call %+v", c)
	}
	if len(started) != 2 || res.Text != "Calling get_weather_v2, add_task." {
		t.Fatalf("started %v text %q", started, res.Text)
	}
	rec := readRecord(t, record)
	if !slices.Contains(rec.Args, "--approve-mcps") || len(rec.BridgeTools) != 2 {
		t.Fatalf("record %+v", rec)
	}
}

func TestRunToolResults(t *testing.T) {
	b, acc, _ := newTestBackend(t, fakecursor.ValidKey)
	transcript := "<user>\nweather?\n</user>\n\n<tool_result tool_call_id=\"c1\" name=\"get_weather\">\n{\"temp\":20}\n</tool_result>"
	res, err := b.Run(context.Background(), acc, agent.RunOptions{
		Model: "auto", Content: text(transcript), Transcript: true,
		Tools: []agent.Tool{{Name: "get_weather", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}, agent.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != `Received 1 tool result(s): {"temp":20}` || res.FinishReason != "stop" {
		t.Fatalf("result %+v", res)
	}
}

func TestRunErrors(t *testing.T) {
	cases := []struct {
		key, prompt, kind string
	}{
		{"", "hi", "auth"},
		{fakecursor.ExpiredKey, "hi", "auth"},
		{"not-a-fake-key-0123456789", "hi", "auth"},
		{fakecursor.ValidKey, "[fake:rate-limit]", "rate_limit"},
		{fakecursor.ValidKey, "[fake:overloaded]", "overloaded"},
		{fakecursor.ValidKey, "[fake:bad-request]", "invalid_request"},
		{fakecursor.ValidKey, "[fake:crash]", "upstream"},
	}
	for _, c := range cases {
		b, acc, _ := newTestBackend(t, c.key)
		_, err := b.Run(context.Background(), acc, agent.RunOptions{Model: "auto", Content: text(c.prompt)}, agent.Callbacks{})
		var ce *agent.Error
		if !errors.As(err, &ce) || ce.Kind != c.kind {
			t.Errorf("key %q prompt %q: err %v, want %s", c.key, c.prompt, err, c.kind)
		}
	}
}

func TestRunCancel(t *testing.T) {
	b, acc, _ := newTestBackend(t, fakecursor.ValidKey)
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() {
		_, err := b.Run(ctx, acc, agent.RunOptions{Model: "auto", Content: text("[fake:slow]")}, agent.Callbacks{
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

func TestLoginRunLogout(t *testing.T) {
	b, acc, _ := newTestBackend(t, "")
	m := agent.NewLoginManager()
	v0 := acc.Tokens.Version()
	st, err := m.Start(acc.ID, b.Login(acc))
	if err != nil {
		t.Fatal(err)
	}
	if st.State != agent.LoginWaitBrowse || st.NeedsCode || !strings.HasPrefix(st.URL, fakecursor.LoginURLPrefix) {
		t.Fatalf("start %+v", st)
	}
	deadline := time.Now().Add(10 * time.Second)
	for m.Status(acc.ID).State != agent.LoginSuccess && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if st := m.Status(acc.ID); st.State != agent.LoginSuccess {
		t.Fatalf("status %+v", st)
	}
	if acc.Tokens.Version() == v0 {
		t.Fatal("a sign-in must bump the credential version so a stale login failure is forgotten")
	}
	// Signed in through the CLI: runs work without an API key.
	res, err := b.Run(context.Background(), acc, agent.RunOptions{Model: "auto", Content: text("hi")}, agent.Callbacks{})
	if err != nil || res.Text != "Fake reply to: hi" {
		t.Fatalf("res %+v err %v", res, err)
	}
	if err := b.Logout(context.Background(), acc); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Run(context.Background(), acc, agent.RunOptions{Model: "auto", Content: text("hi")}, agent.Callbacks{}); err == nil {
		t.Fatal("run after logout should fail")
	}
	if v := b.Version(context.Background(), acc.BaseEnv()); v != fakecursor.Version {
		t.Fatalf("version %q", v)
	}
}
