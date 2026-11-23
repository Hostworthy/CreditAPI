// Package fakeclaude imitates the claude CLI for tests. A test binary calls
// MaybeRun from TestMain; when CMTA_FAKE_CLAUDE is set the process behaves
// like `claude` for the named scenario and exits. The stream-json shapes
// mirror real `claude -p --output-format stream-json` output.
package fakeclaude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// EnvScenario selects the behaviour; EnvRecord names a file to record the
// invocation into.
const (
	EnvScenario = "CMTA_FAKE_CLAUDE"
	EnvRecord   = "CMTA_FAKE_RECORD"
)

// Record is what the fake saw.
type Record struct {
	Args         []string          `json:"args"`
	Stdin        json.RawMessage   `json:"stdin"`
	SystemPrompt string            `json:"system_prompt"`
	MCPConfig    json.RawMessage   `json:"mcp_config"`
	Env          map[string]string `json:"env"`
	BridgeTools  []string          `json:"bridge_tools"`
}

// MaybeRun runs the fake if the scenario env var is set, then exits.
func MaybeRun() {
	scenario := os.Getenv(EnvScenario)
	if scenario == "" {
		return
	}
	os.Exit(run(scenario, os.Args[1:]))
}

func flag(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func emit(v any) {
	b, _ := json.Marshal(v)
	os.Stdout.Write(append(b, '\n'))
}

func ev(e map[string]any) { emit(map[string]any{"type": "stream_event", "event": e}) }

func usage(in, out int) map[string]any {
	return map[string]any{"input_tokens": in, "output_tokens": out, "cache_read_input_tokens": 100, "cache_creation_input_tokens": 0}
}

func run(scenario string, args []string) int {
	if scenario == "setup-token" || (len(args) > 0 && args[0] == "setup-token") {
		return setupToken()
	}
	if len(args) > 0 && args[0] == "--version" {
		fmt.Println("9.9.9 (Claude Code)")
		return 0
	}

	rec := Record{Args: args, Env: map[string]string{}}
	line, _ := bufio.NewReader(os.Stdin).ReadBytes('\n')
	rec.Stdin = json.RawMessage(bytes.TrimSpace(line))
	if p := flag(args, "--system-prompt-file"); p != "" {
		b, _ := os.ReadFile(p)
		rec.SystemPrompt = string(b)
	}
	for _, k := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_MAX_OUTPUT_TOKENS", "HOME", "ADMIN_PASSWORD"} {
		if v, ok := os.LookupEnv(k); ok {
			rec.Env[k] = v
		}
	}
	var bridgeURL string
	if p := flag(args, "--mcp-config"); p != "" {
		b, _ := os.ReadFile(p)
		rec.MCPConfig = b
		var cfg struct {
			MCPServers map[string]struct {
				URL string `json:"url"`
			} `json:"mcpServers"`
		}
		_ = json.Unmarshal(b, &cfg)
		bridgeURL = cfg.MCPServers["oai"].URL
	}
	if bridgeURL != "" {
		rec.BridgeTools = listBridgeTools(bridgeURL)
	}
	if p := os.Getenv(EnvRecord); p != "" {
		b, _ := json.Marshal(rec)
		_ = os.WriteFile(p, b, 0o600)
	}

	model := flag(args, "--model")
	emit(map[string]any{"type": "system", "subtype": "init", "model": model, "tools": []string{},
		"mcp_servers": []map[string]string{{"name": "oai", "status": "connected"}}})
	ev(map[string]any{"type": "message_start", "message": map[string]any{"model": "claude-test-" + model, "id": "msg_1", "usage": usage(10, 1)}})

	switch scenario {
	case "text", "slow":
		ev(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		parts := []string{"Hello", " wor", "ld"}
		if scenario == "slow" {
			parts = []string{"one ", "two ", "ST", "OP three ", "four"}
		}
		for _, p := range parts {
			ev(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": p}})
			if scenario == "slow" {
				time.Sleep(150 * time.Millisecond)
			}
		}
		ev(map[string]any{"type": "content_block_stop", "index": 0})
		ev(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": usage(10, 5)})
		ev(map[string]any{"type": "message_stop"})
		emit(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"status": "allowed",
			"unifiedWindows": map[string]any{"five_hour": map[string]any{"utilization": 0.25, "resetsAt": 1795437000}}}})
		if scenario == "slow" {
			time.Sleep(10 * time.Second) // should be killed at the stop sequence
		}
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": strings.Join(parts, ""),
			"stop_reason": "end_turn", "usage": usage(10, 5), "total_cost_usd": 0.001})
	case "tools":
		name := "mcp__oai__unknown"
		if len(rec.BridgeTools) > 0 {
			name = "mcp__oai__" + rec.BridgeTools[0]
		}
		ev(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		ev(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "Checking."}})
		ev(map[string]any{"type": "content_block_stop", "index": 0})
		for i, city := range []string{"Paris", "Rome"} {
			idx := i + 1
			ev(map[string]any{"type": "content_block_start", "index": idx, "content_block": map[string]any{"type": "tool_use", "id": fmt.Sprintf("toolu_%d", idx), "name": name, "input": map[string]any{}}})
			ev(map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"city":`}})
			ev(map[string]any{"type": "content_block_delta", "index": idx, "delta": map[string]any{"type": "input_json_delta", "partial_json": `"` + city + `"}`}})
			ev(map[string]any{"type": "content_block_stop", "index": idx})
		}
		ev(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": usage(10, 20)})
		ev(map[string]any{"type": "message_stop"})
		// The real CLI would now call the tool and wait; the runner must kill us.
		if bridgeURL != "" {
			rpc(bridgeURL, "tools/call", map[string]any{"name": strings.TrimPrefix(name, "mcp__oai__"), "arguments": map[string]any{"city": "Paris"}})
		}
		time.Sleep(30 * time.Second)
	case "schema":
		ev(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "toolu_s", "name": "StructuredOutput", "input": map[string]any{}}})
		ev(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"food":"banana","calories":105}`}})
		ev(map[string]any{"type": "content_block_stop", "index": 0})
		ev(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": usage(10, 9)})
		ev(map[string]any{"type": "message_stop"})
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": `{"food":"banana","calories":105}`,
			"structured_output": map[string]any{"food": "banana", "calories": 105}, "stop_reason": "tool_use", "usage": usage(20, 9)})
	case "auth":
		ev(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		emit(map[string]any{"type": "assistant", "error": "authentication_failed", "message": map[string]any{"content": []map[string]any{{"type": "text", "text": "Failed to authenticate. API Error: 401 OAuth access token is invalid."}}}})
		emit(map[string]any{"type": "result", "subtype": "success", "is_error": true, "api_error_status": 401,
			"result": "Failed to authenticate. API Error: 401 OAuth access token is invalid."})
	case "crash":
		fmt.Fprintln(os.Stderr, "boom: something went wrong")
		return 3
	}
	return 0
}

func rpc(url, method string, params any) json.RawMessage {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out struct {
		Result json.RawMessage `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Result
}

func listBridgeTools(url string) []string {
	rpc(url, "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "fake"}})
	res := rpc(url, "tools/list", map[string]any{})
	var tl struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	_ = json.Unmarshal(res, &tl)
	var names []string
	for _, t := range tl.Tools {
		names = append(names, t.Name)
	}
	return names
}

// FakeToken is the token the fake setup-token flow prints.
const FakeToken = "sk-ant-oat01-FAKEtokenFAKEtokenFAKEtokenFAKEtokenFAKEtokenFAKEtoken_-123"

// FakeLoginURL is the sign-in URL the fake setup-token flow prints.
const FakeLoginURL = "https://claude.ai/oauth/authorize?code=true&client_id=test&state=abc123"

func setupToken() int {
	out := os.Stdout
	fmt.Fprint(out, "Welcome to Claude Code v9.9.9\r\r\n\r\r\nThis\x1b[6Gwill\x1b[11Gguide\x1b[17Gyou\r\r\n")
	fmt.Fprint(out, "\x1b[>0q\r✢\r\r\n\r*\r\r\n")
	fmt.Fprint(out, "Browser didn't open?\x1b[23GUse the URL\x1b[35Gbelow\r\r\n\r\r\n")
	fmt.Fprintf(out, "\x1b]8;id=1;%s\x07\x1b[38;5;246m%s\x1b[39m\x1b]8;;\x07\r\r\n\r\r\n", FakeLoginURL, FakeLoginURL)
	fmt.Fprint(out, "\x1b[2GPaste\x1b[8Gcode\x1b[13Ghere\x1b[18Gif\x1b[21Gprompted\r\r\n\x1b[2G╭──────────────╮\r\r\n")
	var sb strings.Builder
	rd := bufio.NewReader(os.Stdin)
	for {
		c, err := rd.ReadByte()
		if err != nil || c == '\r' || c == '\n' {
			break
		}
		sb.WriteByte(c)
	}
	code := strings.TrimSpace(sb.String())
	if code != "good-code#state" {
		fmt.Fprint(out, "\r\r\nOAuth error: Invalid code\r\r\n")
		return 1
	}
	fmt.Fprint(out, "\r\r\n\x1b[32m✓\x1b[39m Long-lived authentication token created successfully!\r\r\n\r\r\n")
	fmt.Fprintf(out, "Your OAuth token (valid for 1 year):\r\r\n\r\r\n\x1b[33m%s\x1b[39m\r\r\n\r\r\n", FakeToken)
	fmt.Fprint(out, "Store this token securely.\r\r\n")
	return 0
}
