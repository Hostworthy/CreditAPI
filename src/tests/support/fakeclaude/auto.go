package fakeclaude

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif" // register decoders for image size reports
	_ "image/jpeg"
	_ "image/png"
	"os"
	"regexp"
	"strings"
	"time"

	_ "golang.org/x/image/webp"
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
//     from the schema (see SampleFromSchema).
//  4. Client tools (MCP bridge): when the conversation ends with tool results,
//     reply "Received N tool result(s): ...". Otherwise call every tool whose
//     name appears in the text (or the first tool) with generated arguments.
//  5. Text directives:
//     [fake:slow]         "one two three STOP four five", slowly
//     [fake:echo-system]  reply with the system prompt
//     [fake:echo-input]   reply with the full user message text
//     [fake:long]         a reply of about 4000 characters
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

var directiveRe = regexp.MustCompile(`\[fake:([a-z-]+)\]`)

func directives(text string) map[string]bool {
	out := map[string]bool{}
	for _, m := range directiveRe.FindAllStringSubmatch(text, -1) {
		out[m[1]] = true
	}
	return out
}

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

	d := directives(inv.text)
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
		value := SampleFromSchema(inv.schema, "value")
		raw, _ := json.Marshal(value)
		toolUseBlock(0, "toolu_fake_schema", "StructuredOutput", string(raw))
		messageEnd("tool_use", in, len(raw)/4+1)
		rateLimitEvent("allowed", 0.1)
		result(map[string]any{"result": string(raw), "structured_output": value, "stop_reason": "tool_use",
			"usage": usage(in, len(raw)/4+1), "total_cost_usd": 0.0001})
		return 0
	}

	if len(inv.tools) > 0 {
		if n := strings.Count(inv.text, "</tool_result>"); n > 0 && endsWithToolResults(inv.text) {
			reply := fmt.Sprintf("Received %d tool result(s): %s", n, firstToolResult(inv.text))
			return finishText(inv, in, reply, 0)
		}
		callTools(inv, in)
		return 0
	}

	var reply string
	var delay time.Duration
	switch {
	case d["slow"]:
		reply, delay = "one two three STOP four five", 150*time.Millisecond
	case d["echo-system"]:
		reply = inv.system
	case d["echo-input"]:
		reply = inv.text
	case d["long"]:
		reply = strings.Repeat("All work and no play makes a long fake reply. ", 90)
	default:
		reply = "Fake reply to: " + lastUserLine(inv.text)
		if desc := describeImages(inv.content); desc != "" {
			reply = desc + " " + reply
		}
		if strings.Contains(inv.system, "single valid JSON object") {
			obj, _ := json.Marshal(map[string]any{"fake": true, "reply": lastUserLine(inv.text)})
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
	parts := chunk(reply)
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
	var chosen []bridgeTool
	for _, t := range inv.tools {
		if strings.Contains(inv.text, t.Name) {
			chosen = append(chosen, t)
		}
	}
	if len(chosen) == 0 {
		chosen = inv.tools[:1]
	}
	names := make([]string, len(chosen))
	for i, t := range chosen {
		names[i] = t.Name
	}
	textBlock(0, []string{"Calling ", strings.Join(names, ", "), "."}, 0)
	for i, t := range chosen {
		args, _ := json.Marshal(SampleFromSchema(t.InputSchema, t.Name))
		toolUseBlock(i+1, fmt.Sprintf("toolu_fake_%d", i+1), "mcp__oai__"+t.Name, string(args))
	}
	messageEnd("tool_use", in, 20*len(chosen))
	rateLimitEvent("allowed", 0.1)
	waitForKill(inv, chosen[0].Name)
}

// chunk splits a reply into a few streaming deltas.
func chunk(s string) []string {
	if s == "" {
		return []string{""}
	}
	words := strings.SplitAfter(s, " ")
	var parts []string
	for i := 0; i < len(words); i += 4 {
		end := min(i+4, len(words))
		parts = append(parts, strings.Join(words[i:end], ""))
	}
	return parts
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
	var parts []string
	for _, b := range content {
		if b.Type != "image" || b.Source == nil {
			continue
		}
		size := "unreadable"
		if raw, err := base64.StdEncoding.DecodeString(b.Source.Data); err == nil {
			if cfg, _, err := image.DecodeConfig(bytes.NewReader(raw)); err == nil {
				size = fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)
			}
		}
		parts = append(parts, b.Source.MediaType+" "+size)
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("I see %d image(s): %s.", len(parts), strings.Join(parts, ", "))
}

// lastUserLine returns the first line of the last user turn, whether the
// input is a single message or a rendered transcript.
func lastUserLine(text string) string {
	if i := strings.LastIndex(text, "<user"); i >= 0 {
		rest := text[i:]
		if j := strings.Index(rest, ">"); j >= 0 {
			rest = rest[j+1:]
		}
		if k := strings.Index(rest, "</user>"); k >= 0 {
			rest = rest[:k]
		}
		text = rest
	}
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if len(l) > 120 {
				l = l[:120]
			}
			return l
		}
	}
	return "(empty message)"
}

func endsWithToolResults(text string) bool {
	last := strings.LastIndex(text, "</tool_result>")
	return last >= 0 && !strings.Contains(text[last:], "<user")
}

// firstToolResult returns the body of the first complete <tool_result> block
// (the transcript's introduction mentions the tag name, so anchor on the
// closing tag and search backwards for its opening tag).
func firstToolResult(text string) string {
	end := strings.Index(text, "</tool_result>")
	if end < 0 {
		return ""
	}
	start := strings.LastIndex(text[:end], "<tool_result")
	if start < 0 {
		return ""
	}
	rest := text[start:end]
	if j := strings.Index(rest, ">"); j >= 0 {
		rest = rest[j+1:]
	}
	rest = strings.TrimSpace(rest)
	if len(rest) > 200 {
		rest = rest[:200]
	}
	return rest
}
