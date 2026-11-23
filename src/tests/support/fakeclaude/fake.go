// Package fakeclaude is a fake Claude Code CLI for tests and the Bruno
// end-to-end stack. It speaks the same `claude -p --input-format stream-json
// --output-format stream-json` protocol as the real CLI (shapes copied from
// real output) and fakes `claude setup-token`, so the whole server can be
// exercised without a Claude account or network access.
//
// Two modes, picked with CREDITAPI_FAKE_CLAUDE:
//
//   - "auto" (the default for the fakeclaude binary): deterministic replies
//     driven by the input. See auto.go for the rules and [fake:...] directives.
//   - a scripted scenario ("text", "slow", "tools", "schema", "auth", "crash")
//     with fixed output, used by unit tests that assert exact values.
//
// Go tests re-execute their own test binary as the fake: call MaybeRun from
// TestMain and point CLAUDE_BIN at os.Executable().
package fakeclaude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/atb00ker/CreditAPI/src/tests/support/fakecore"
)

// Environment variables understood by the fake.
const (
	EnvScenario = "CREDITAPI_FAKE_CLAUDE"
	EnvRecord   = "CREDITAPI_FAKE_RECORD"
)

// Record is what the fake saw; written to CREDITAPI_FAKE_RECORD when set.
type Record struct {
	Args         []string          `json:"args"`
	Stdin        json.RawMessage   `json:"stdin"`
	SystemPrompt string            `json:"system_prompt"`
	MCPConfig    json.RawMessage   `json:"mcp_config"`
	Env          map[string]string `json:"env"`
	BridgeTools  []string          `json:"bridge_tools"`
}

// MaybeRun runs the fake and exits when CREDITAPI_FAKE_CLAUDE is set. Call it
// first thing in TestMain.
func MaybeRun() {
	if os.Getenv(EnvScenario) == "" {
		return
	}
	os.Exit(Main(os.Args[1:]))
}

// Main runs the fake CLI with the given arguments and returns the exit code.
func Main(args []string) int {
	scenario := os.Getenv(EnvScenario)
	if scenario == "" {
		scenario = "auto"
	}
	if len(args) > 0 {
		switch args[0] {
		case "--version", "-v":
			fmt.Println("0.0.0-fake (Claude Code)")
			return 0
		case "setup-token":
			return setupToken(os.Stdin, os.Stdout)
		}
	}
	inv, err := parseInvocation(args, os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakeclaude:", err)
		return 2
	}
	inv.record()
	if scenario == "auto" {
		return runAuto(inv)
	}
	return runScripted(scenario, inv)
}

// --- invocation --------------------------------------------------------------

type imageSource struct {
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type block struct {
	Type   string       `json:"type"`
	Text   string       `json:"text"`
	Source *imageSource `json:"source"`
}

type invocation struct {
	args      []string
	stdin     []byte
	model     string
	system    string
	schema    map[string]any
	mcpConfig []byte
	bridgeURL string
	tools     []fakecore.Tool
	content   []block
	text      string
	maxTokens int
	token     string
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func parseInvocation(args []string, stdin io.Reader) (*invocation, error) {
	inv := &invocation{args: args, model: flagValue(args, "--model"), token: os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")}
	line, _ := bufio.NewReader(stdin).ReadBytes('\n')
	inv.stdin = bytes.TrimSpace(line)
	var msg struct {
		Message struct {
			Content []block `json:"content"`
		} `json:"message"`
	}
	if len(inv.stdin) > 0 {
		if err := json.Unmarshal(inv.stdin, &msg); err != nil {
			return nil, fmt.Errorf("stdin is not a stream-json user message: %w", err)
		}
	}
	inv.content = msg.Message.Content
	var texts []string
	for _, b := range inv.content {
		if b.Type == "text" {
			texts = append(texts, b.Text)
		}
	}
	inv.text = strings.Join(texts, "\n")

	if p := flagValue(args, "--system-prompt-file"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		inv.system = string(b)
	}
	if s := flagValue(args, "--json-schema"); s != "" {
		if err := json.Unmarshal([]byte(s), &inv.schema); err != nil {
			return nil, fmt.Errorf("--json-schema is not JSON: %w", err)
		}
	}
	if p := flagValue(args, "--mcp-config"); p != "" {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		inv.mcpConfig = b
		var cfg struct {
			MCPServers map[string]struct {
				URL string `json:"url"`
			} `json:"mcpServers"`
		}
		_ = json.Unmarshal(b, &cfg)
		inv.bridgeURL = cfg.MCPServers["oai"].URL
	}
	if inv.bridgeURL != "" {
		inv.tools = fakecore.ListTools(inv.bridgeURL)
	}
	inv.maxTokens, _ = strconv.Atoi(os.Getenv("CLAUDE_CODE_MAX_OUTPUT_TOKENS"))
	return inv, nil
}

func (inv *invocation) record() {
	path := os.Getenv(EnvRecord)
	if path == "" {
		return
	}
	rec := Record{Args: inv.args, Stdin: inv.stdin, SystemPrompt: inv.system, MCPConfig: inv.mcpConfig, Env: map[string]string{}}
	for _, k := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_MAX_OUTPUT_TOKENS", "HOME", "ADMIN_PASSWORD"} {
		if v, ok := os.LookupEnv(k); ok {
			rec.Env[k] = v
		}
	}
	for _, t := range inv.tools {
		rec.BridgeTools = append(rec.BridgeTools, t.Name)
	}
	b, _ := json.Marshal(rec)
	_ = os.WriteFile(path, b, 0o600)
}

// --- stream-json output -------------------------------------------------------

func emit(v any) {
	b, _ := json.Marshal(v)
	os.Stdout.Write(append(b, '\n'))
}

func ev(e map[string]any) { emit(map[string]any{"type": "stream_event", "event": e}) }

func usage(in, out int) map[string]any {
	return map[string]any{"input_tokens": in, "output_tokens": out, "cache_read_input_tokens": 100, "cache_creation_input_tokens": 0}
}

func emitInit(inv *invocation) {
	servers := []map[string]string{}
	if inv.bridgeURL != "" {
		servers = append(servers, map[string]string{"name": "oai", "status": "connected"})
	}
	emit(map[string]any{"type": "system", "subtype": "init", "model": inv.model, "tools": []string{}, "mcp_servers": servers})
}

func messageStart(model string, in int) {
	ev(map[string]any{"type": "message_start", "message": map[string]any{"model": "claude-fake-" + model, "id": "msg_fake", "usage": usage(in, 1)}})
}

func textBlock(index int, parts []string, delay time.Duration) {
	ev(map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "text", "text": ""}})
	for _, p := range parts {
		ev(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": p}})
		if delay > 0 {
			time.Sleep(delay)
		}
	}
	ev(map[string]any{"type": "content_block_stop", "index": index})
}

func toolUseBlock(index int, id, name string, args string) {
	ev(map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}})
	half := len(args) / 2
	for _, p := range []string{args[:half], args[half:]} {
		ev(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": p}})
	}
	ev(map[string]any{"type": "content_block_stop", "index": index})
}

func messageEnd(stopReason string, in, out int) {
	ev(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason}, "usage": usage(in, out)})
	ev(map[string]any{"type": "message_stop"})
}

func rateLimitEvent(status string, utilization float64) {
	emit(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{
		"status": status,
		"unifiedWindows": map[string]any{
			"five_hour": map[string]any{"utilization": utilization, "resetsAt": time.Now().Add(3 * time.Hour).Unix()},
			"seven_day": map[string]any{"utilization": utilization / 4, "resetsAt": time.Now().Add(96 * time.Hour).Unix()},
		},
	}})
}

func result(fields map[string]any) {
	out := map[string]any{"type": "result", "subtype": "success", "is_error": false}
	for k, v := range fields {
		out[k] = v
	}
	emit(out)
}

// waitForKill imitates the CLI executing a client tool: it calls the bridge
// (which never answers) and waits to be stopped by the server.
func waitForKill(inv *invocation, tool string) {
	if inv.bridgeURL != "" {
		fakecore.RPC(inv.bridgeURL, "tools/call", map[string]any{"name": tool, "arguments": map[string]any{}})
	}
	time.Sleep(30 * time.Second)
}

// --- scripted scenarios (exact output for unit tests) --------------------------

func runScripted(scenario string, inv *invocation) int {
	emitInit(inv)
	messageStart(inv.model, 10)
	switch scenario {
	case "text", "slow":
		parts := []string{"Hello", " wor", "ld"}
		delay := time.Duration(0)
		if scenario == "slow" {
			parts = []string{"one ", "two ", "ST", "OP three ", "four"}
			delay = 150 * time.Millisecond
		}
		textBlock(0, parts, delay)
		messageEnd("end_turn", 10, 5)
		rateLimitEvent("allowed", 0.25)
		if scenario == "slow" {
			time.Sleep(10 * time.Second) // the server should stop us at the stop sequence
		}
		result(map[string]any{"result": strings.Join(parts, ""), "stop_reason": "end_turn", "usage": usage(10, 5), "total_cost_usd": 0.001})
	case "tools":
		name := "mcp__oai__unknown"
		if len(inv.tools) > 0 {
			name = "mcp__oai__" + inv.tools[0].Name
		}
		textBlock(0, []string{"Checking."}, 0)
		for i, city := range []string{"Paris", "Rome"} {
			toolUseBlock(i+1, fmt.Sprintf("toolu_%d", i+1), name, `{"city":"`+city+`"}`)
		}
		messageEnd("tool_use", 10, 20)
		waitForKill(inv, strings.TrimPrefix(name, "mcp__oai__"))
	case "schema":
		toolUseBlock(0, "toolu_s", "StructuredOutput", `{"food":"banana","calories":105}`)
		messageEnd("tool_use", 10, 9)
		result(map[string]any{"result": `{"food":"banana","calories":105}`, "stop_reason": "tool_use", "usage": usage(20, 9),
			"structured_output": map[string]any{"food": "banana", "calories": 105}})
	case "auth":
		authFailure("Failed to authenticate. API Error: 401 OAuth access token is invalid.", 401)
	case "crash":
		fmt.Fprintln(os.Stderr, "boom: something went wrong")
		return 3
	default:
		fmt.Fprintln(os.Stderr, "fakeclaude: unknown scenario", scenario)
		return 2
	}
	return 0
}

func authFailure(msg string, status int) {
	emit(map[string]any{"type": "assistant", "error": "authentication_failed",
		"message": map[string]any{"content": []map[string]any{{"type": "text", "text": msg}}}})
	fields := map[string]any{"is_error": true, "result": msg}
	if status > 0 {
		fields["api_error_status"] = status
	}
	result(fields)
}
