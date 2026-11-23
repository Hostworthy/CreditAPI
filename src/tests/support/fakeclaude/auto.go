package fakeclaude

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/atb00ker/CreditAPI/src/tests/support/fakecore"
)

// Auto mode rules, in order:
//
//  1. Auth: CLAUDE_CODE_OAUTH_TOKEN must start with TokenPrefix (tokens issued
//     by the fake setup-token do). No token: "Not logged in". Another token, or
//     one containing "expired": a 401 like the real API.
//  2. Directives anywhere in the user text:
//     [fake:crash]        exit with an error and no result
//     [fake:rate-limit]   API error 429
//     [fake:overloaded]   API error 529
//     [fake:bad-request]  API error 400
//  3. --json-schema: answer through StructuredOutput with a value generated
//     from the schema (see fakecore.SampleFromSchema).
//  4. Client tools (MCP bridge): when the conversation ends with tool results,
//     reply "Received N tool result(s): ...". Otherwise call every tool whose
//     name appears in the text (or the first tool) with generated arguments.
//  5. Text directives:
//     [fake:slow]         "one two three STOP four five", slowly
//     [fake:echo-system]  reply with the system prompt
//     [fake:echo-input]   reply with the full user message text
//     [fake:long]         a reply of about 4000 characters
//     [fake:whoami]       "Served by claude subscription <id>"
//  6. Otherwise "Fake reply to: <last user line>", prefixed with a list of
//     images and their sizes ("I see 1 image(s): image/png 8x8."). When the
//     system prompt asks for a JSON object the reply is a fenced JSON object.
//
// A reply longer than CLAUDE_CODE_MAX_OUTPUT_TOKENS*4 characters is cut with
// stop_reason max_tokens.

const (
	// TokenPrefix marks tokens the fake accepts.
	TokenPrefix = "sk-ant-oat01-fake-"
	// ValidToken is a token the fake accepts.
	ValidToken = TokenPrefix + "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJ"
	// ExpiredToken looks like a fake token but is rejected as expired.
	ExpiredToken = TokenPrefix + "expired-0123456789abcdefghijklmnopqrstuvwxyz"
)

func runAuto(inv *invocation) int {
	emitInit(inv)
	switch {
	case inv.token == "":
		result(map[string]any{"is_error": true, "result": "Not logged in · Please run /login"})
		return 0
	case !strings.HasPrefix(inv.token, TokenPrefix):
		authFailure("Failed to authenticate. API Error: 401 OAuth access token is invalid.", 401)
		return 0
	case strings.Contains(inv.token, "expired"):
		authFailure("Failed to authenticate. API Error: 401 OAuth token has expired. Please obtain a new token or refresh your existing token.", 401)
		return 0
	}

	d := fakecore.Directives(inv.text)
	in := 10 + len(inv.text)/4 + 85*countImages(inv.content)
	switch {
	case d["crash"]:
		fmt.Fprintln(os.Stderr, "fakeclaude: simulated crash")
		return 3
	case d["rate-limit"]:
		rateLimitEvent("rejected", 1)
		apiError(429, `API Error: 429 {"type":"error","error":{"type":"rate_limit_error","message":"This request would exceed your usage limit (fake)"}}`)
		return 0
	case d["overloaded"]:
		apiError(529, `API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded (fake)"}}`)
		return 0
	case d["bad-request"]:
		apiError(400, `API Error: 400 {"type":"error","error":{"type":"invalid_request_error","message":"messages: invalid (fake)"}}`)
		return 0
	}

	messageStart(inv.model, in)
	if inv.schema != nil {
		value := fakecore.SampleFromSchema(inv.schema, "value")
		raw, _ := json.Marshal(value)
		toolUseBlock(0, "toolu_fake_schema", "StructuredOutput", string(raw))
		messageEnd("tool_use", in, len(raw)/4+1)
		rateLimitEvent("allowed", 0.1)
		result(map[string]any{"result": string(raw), "structured_output": value, "stop_reason": "tool_use",
			"usage": usage(in, len(raw)/4+1), "total_cost_usd": 0.0001})
		return 0
	}

	if len(inv.tools) > 0 {
		if n := fakecore.ToolResults(inv.text); n > 0 && fakecore.EndsWithToolResults(inv.text) {
			reply := fmt.Sprintf("Received %d tool result(s): %s", n, fakecore.FirstToolResult(inv.text))
			return finishText(inv, in, reply, 0)
		}
		callTools(inv, in)
		return 0
	}

	var reply string
	var delay time.Duration
	switch {
	case d["slow"]:
		reply, delay = fakecore.SlowReply, 150*time.Millisecond
	case d["echo-system"]:
		reply = inv.system
	case d["echo-input"]:
		reply = inv.text
	case d["long"]:
		reply = fakecore.LongReply()
	case d["whoami"]:
		reply = fakecore.Whoami("claude")
	default:
		reply = "Fake reply to: " + fakecore.LastUserLine(inv.text)
		if desc := describeImages(inv.content); desc != "" {
			reply = desc + " " + reply
		}
		if strings.Contains(inv.system, "single valid JSON object") && !d["not-json"] {
			obj, _ := json.Marshal(map[string]any{"fake": true, "reply": fakecore.LastUserLine(inv.text)})
			reply = "```json\n" + string(obj) + "\n```"
		}
	}
	return finishText(inv, in, reply, delay)
}

func apiError(status int, msg string) {
	emit(map[string]any{"type": "assistant", "error": "api_error",
		"message": map[string]any{"content": []map[string]any{{"type": "text", "text": msg}}}})
	result(map[string]any{"is_error": true, "result": msg, "api_error_status": status})
}

func finishText(inv *invocation, in int, reply string, delay time.Duration) int {
	stop := "end_turn"
	if inv.maxTokens > 0 && len(reply) > inv.maxTokens*4 {
		reply = reply[:inv.maxTokens*4]
		stop = "max_tokens"
	}
	out := len(reply)/4 + 1
	parts := fakecore.Chunk(reply)
	if delay > 0 {
		parts = strings.SplitAfter(reply, " ") // one word per delta, so it really is slow
	}
	textBlock(0, parts, delay)
	messageEnd(stop, in, out)
	rateLimitEvent("allowed", 0.1)
	result(map[string]any{"result": reply, "stop_reason": stop, "usage": usage(in, out), "total_cost_usd": 0.0001})
	return 0
}

func callTools(inv *invocation, in int) {
	chosen := fakecore.ChooseTools(inv.tools, inv.text)
	names := make([]string, len(chosen))
	for i, t := range chosen {
		names[i] = t.Name
	}
	textBlock(0, []string{"Calling ", strings.Join(names, ", "), "."}, 0)
	for i, t := range chosen {
		args, _ := json.Marshal(fakecore.SampleFromSchema(t.InputSchema, t.Name))
		toolUseBlock(i+1, fmt.Sprintf("toolu_fake_%d", i+1), "mcp__oai__"+t.Name, string(args))
	}
	messageEnd("tool_use", in, 20*len(chosen))
	rateLimitEvent("allowed", 0.1)
	waitForKill(inv, chosen[0].Name)
}

func countImages(content []block) int {
	n := 0
	for _, b := range content {
		if b.Type == "image" {
			n++
		}
	}
	return n
}

func describeImages(content []block) string {
	var items []string
	for _, b := range content {
		if b.Type != "image" || b.Source == nil {
			continue
		}
		size := "unreadable"
		if raw, err := base64.StdEncoding.DecodeString(b.Source.Data); err == nil {
			size = fakecore.ImageSize(raw)
		}
		items = append(items, b.Source.MediaType+" "+size)
	}
	return fakecore.DescribeImages(items)
}
