package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/atb00ker/CreditAPI/src/tests/support"
)

type sseEvent struct {
	name string
	data map[string]any
}

// anthropic posts to path the way Anthropic SDKs do (x-api-key and
// anthropic-version) and returns the status, the JSON body (non-streaming)
// and the events (streaming).
func anthropic(t *testing.T, s *support.TestServer, key, path string, body map[string]any) (int, map[string]any, []sseEvent) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", s.URL+path, bytes.NewReader(b))
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		var out map[string]any
		_ = json.Unmarshal(raw, &out)
		return resp.StatusCode, out, nil
	}
	var events []sseEvent
	for _, chunk := range strings.Split(string(raw), "\n\n") {
		var ev sseEvent
		for _, line := range strings.Split(chunk, "\n") {
			if v, ok := strings.CutPrefix(line, "event: "); ok {
				ev.name = v
			} else if v, ok := strings.CutPrefix(line, "data: "); ok {
				if err := json.Unmarshal([]byte(v), &ev.data); err != nil {
					t.Fatalf("bad event data %q", v)
				}
			}
		}
		if ev.name != "" {
			events = append(events, ev)
		}
	}
	return resp.StatusCode, nil, events
}

func say(text string) []map[string]any {
	return []map[string]any{{"role": "user", "content": text}}
}

func blocksOf(out map[string]any) []map[string]any {
	var list []map[string]any
	for _, b := range out["content"].([]any) {
		list = append(list, b.(map[string]any))
	}
	return list
}

func TestAnthropicMessage(t *testing.T) {
	s, key := signedIn(t, support.Options{})
	code, out, _ := anthropic(t, s, key, "/v1/messages", map[string]any{
		"model": "claude-haiku-4-5", "max_tokens": 512, "messages": say("What is the capital of France?"),
	})
	if code != 200 || out["type"] != "message" || out["role"] != "assistant" || !strings.HasPrefix(out["id"].(string), "msg_") {
		t.Fatalf("%d %v", code, out)
	}
	blocks := blocksOf(out)
	if len(blocks) != 1 || blocks[0]["type"] != "text" || blocks[0]["text"] != "Fake reply to: What is the capital of France?" {
		t.Fatalf("content %v", blocks)
	}
	usage := out["usage"].(map[string]any)
	if out["stop_reason"] != "end_turn" || out["model"] != "claude-haiku-4-5" || usage["output_tokens"].(float64) <= 0 {
		t.Fatalf("envelope %v", out)
	}

	// The same endpoint under /anthropic, a system prompt as blocks, and the
	// Authorization header instead of x-api-key all work.
	code, out, _ = anthropic(t, s, key, "/anthropic/v1/messages", map[string]any{
		"model": "haiku", "max_tokens": 100,
		"system":   []map[string]any{{"type": "text", "text": "Be brief."}, {"type": "text", "text": "Be kind."}},
		"messages": say("[fake:echo-system]"),
	})
	if code != 200 || blocksOf(out)[0]["text"] != "Be brief.\n\nBe kind." {
		t.Fatalf("system blocks: %d %v", code, out)
	}
}

func TestAnthropicConversationAndImages(t *testing.T) {
	s, key := signedIn(t, support.Options{})
	png := strings.TrimPrefix(support.PNGDataURL(t, 30, 20), "data:image/png;base64,")
	_, out, _ := anthropic(t, s, key, "/v1/messages", map[string]any{"model": "sonnet", "max_tokens": 100,
		"messages": []map[string]any{{"role": "user", "content": []map[string]any{
			{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": png}},
			{"type": "text", "text": "What is in this picture?"},
		}}}})
	if got := blocksOf(out)[0]["text"]; got != "I see 1 image(s): image/png 30x20. Fake reply to: What is in this picture?" {
		t.Fatalf("image reply %q", got)
	}
	_, out, _ = anthropic(t, s, key, "/v1/messages", map[string]any{"model": "sonnet", "max_tokens": 100,
		"messages": []map[string]any{
			{"role": "user", "content": "My favourite colour is green"},
			{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "Noted."}}},
			{"role": "user", "content": "[fake:echo-input] and my number is 7"},
		}})
	text := blocksOf(out)[0]["text"].(string)
	if !strings.Contains(text, "<user>\nMy favourite colour is green\n</user>") || !strings.Contains(text, "<assistant>\nNoted.\n</assistant>") {
		t.Fatalf("history %q", text)
	}
}

var anthropicTools = []map[string]any{{"name": "add_task", "description": "Add a task",
	"input_schema": map[string]any{"type": "object", "required": []string{"title"},
		"properties": map[string]any{"title": map[string]any{"type": "string"}}}}}

func TestAnthropicToolRoundTrip(t *testing.T) {
	s, key := signedIn(t, support.Options{})
	_, out, _ := anthropic(t, s, key, "/v1/messages", map[string]any{"model": "sonnet", "max_tokens": 512,
		"tools": anthropicTools, "messages": say("add a task to renew my passport")})
	if out["stop_reason"] != "tool_use" {
		t.Fatalf("%v", out)
	}
	var use map[string]any
	for _, b := range blocksOf(out) {
		if b["type"] == "tool_use" {
			use = b
		}
	}
	if use == nil || use["name"] != "add_task" || !strings.HasPrefix(use["id"].(string), "toolu_") ||
		use["input"].(map[string]any)["title"] != "fake title" {
		t.Fatalf("tool_use %v", out["content"])
	}

	_, out, _ = anthropic(t, s, key, "/v1/messages", map[string]any{"model": "sonnet", "max_tokens": 512,
		"tools": anthropicTools, "messages": []map[string]any{
			{"role": "user", "content": "add a task to renew my passport"},
			{"role": "assistant", "content": out["content"]},
			{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": use["id"],
				"content": []map[string]any{{"type": "text", "text": `{"ok":true}`}}}}},
		}})
	if out["stop_reason"] != "end_turn" || blocksOf(out)[0]["text"] != `Received 1 tool result(s): {"ok":true}` {
		t.Fatalf("round 2: %v", out)
	}

	// tool_choice "tool" names the tool; server tools are refused.
	_, out, _ = anthropic(t, s, key, "/v1/messages", map[string]any{"model": "sonnet", "max_tokens": 512,
		"tools": anthropicTools, "tool_choice": map[string]any{"type": "tool", "name": "add_task"}, "messages": say("anything")})
	if out["stop_reason"] != "tool_use" {
		t.Fatalf("named tool: %v", out)
	}
	code, out, _ := anthropic(t, s, key, "/v1/messages", map[string]any{"model": "sonnet", "max_tokens": 512,
		"tools": []map[string]any{{"type": "web_search_20250305", "name": "web_search"}}, "messages": say("x")})
	if code != 400 || out["error"].(map[string]any)["type"] != "invalid_request_error" {
		t.Fatalf("server tool: %d %v", code, out)
	}
}

func TestAnthropicStreaming(t *testing.T) {
	s, key := signedIn(t, support.Options{})
	code, _, events := anthropic(t, s, key, "/v1/messages", map[string]any{"model": "sonnet", "max_tokens": 100,
		"stream": true, "messages": say("Stream a short answer")})
	if code != 200 || len(events) < 5 {
		t.Fatalf("%d %v", code, events)
	}
	var names []string
	var text strings.Builder
	for _, e := range events {
		names = append(names, e.name)
		if e.name == "content_block_delta" {
			text.WriteString(e.data["delta"].(map[string]any)["text"].(string))
		}
	}
	got := strings.Join(names, ",")
	if !strings.HasPrefix(got, "message_start,content_block_start,content_block_delta") ||
		!strings.HasSuffix(got, "content_block_stop,message_delta,message_stop") {
		t.Fatalf("event order %s", got)
	}
	last := events[len(events)-2].data
	if text.String() != "Fake reply to: Stream a short answer" || last["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Fatalf("text %q delta %v", text.String(), last)
	}

	// A streamed tool call arrives as a tool_use block with input_json_delta.
	_, _, events = anthropic(t, s, key, "/v1/messages", map[string]any{"model": "sonnet", "max_tokens": 512, "stream": true,
		"tools": anthropicTools, "messages": say("remind me to water the plants")})
	var args strings.Builder
	sawTool := false
	for _, e := range events {
		if e.name == "content_block_start" && e.data["content_block"].(map[string]any)["type"] == "tool_use" {
			sawTool = true
		}
		if e.name == "content_block_delta" {
			if d := e.data["delta"].(map[string]any); d["type"] == "input_json_delta" {
				args.WriteString(d["partial_json"].(string))
			}
		}
	}
	var input map[string]any
	if !sawTool || json.Unmarshal([]byte(args.String()), &input) != nil || input["title"] != "fake title" {
		t.Fatalf("streamed tool: %v %q", sawTool, args.String())
	}
	if events[len(events)-2].data["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Fatalf("stop reason %v", events[len(events)-2].data)
	}
}

func TestAnthropicStopSequenceAndJSON(t *testing.T) {
	s, key := signedIn(t, support.Options{})
	_, out, _ := anthropic(t, s, key, "/v1/messages", map[string]any{"model": "sonnet", "max_tokens": 100,
		"stop_sequences": []string{"STOP"}, "messages": say("[fake:slow]")})
	if out["stop_reason"] != "stop_sequence" || out["stop_sequence"] != "STOP" || blocksOf(out)[0]["text"] != "one two three " {
		t.Fatalf("stop sequence: %v", out)
	}
	_, out, _ = anthropic(t, s, key, "/v1/messages", map[string]any{"model": "sonnet", "max_tokens": 100,
		"output_format": map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object",
			"properties": map[string]any{"total": map[string]any{"type": "number"}}}},
		"messages": say("read this receipt")})
	var v map[string]any
	if err := json.Unmarshal([]byte(blocksOf(out)[0]["text"].(string)), &v); err != nil || v["total"] != float64(42) {
		t.Fatalf("structured: %v", out)
	}
}

func TestAnthropicErrors(t *testing.T) {
	s := support.NewServer(t, support.Options{})
	key := s.CreateKey("app")
	code, out, _ := anthropic(t, s, "sk-credit-wrong", "/v1/messages", map[string]any{"model": "haiku", "max_tokens": 10, "messages": say("hi")})
	if code != 401 || out["type"] != "error" || out["error"].(map[string]any)["type"] != "authentication_error" {
		t.Fatalf("bad key: %d %v", code, out)
	}
	code, out, _ = anthropic(t, s, key, "/v1/messages", map[string]any{"model": "haiku", "max_tokens": 10, "messages": say("hi")})
	e := out["error"].(map[string]any)
	if code != 401 || e["type"] != "authentication_error" || !strings.Contains(e["message"].(string), "/login?subscription=") {
		t.Fatalf("login required: %d %v", code, out)
	}
	s.SignIn()
	for prompt, want := range map[string][2]any{"[fake:overloaded]": {529, "overloaded_error"}, "[fake:rate-limit]": {429, "rate_limit_error"}} {
		code, out, _ = anthropic(t, s, key, "/v1/messages", map[string]any{"model": "haiku", "max_tokens": 10, "messages": say(prompt)})
		if code != want[0] || out["error"].(map[string]any)["type"] != want[1] {
			t.Errorf("%s: %d %v", prompt, code, out)
		}
	}
	code, out, _ = anthropic(t, s, key, "/v1/messages", map[string]any{"model": "haiku", "max_tokens": 10,
		"messages": []map[string]any{{"role": "system", "content": "x"}}})
	if code != 400 || out["error"].(map[string]any)["type"] != "invalid_request_error" {
		t.Fatalf("bad role: %d %v", code, out)
	}
}

func TestAnthropicModelsAndTokenCount(t *testing.T) {
	s, key := signedIn(t, support.Options{})
	anthropicHeaders := map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}
	for _, path := range []string{"/v1/models", "/anthropic/v1/models"} {
		resp, out := s.Do("GET", path, nil, anthropicHeaders)
		data, _ := out["data"].([]any)
		if resp.StatusCode != 200 || len(data) == 0 || data[0].(map[string]any)["type"] != "model" || out["has_more"] != false {
			t.Fatalf("%s: %d %v", path, resp.StatusCode, out)
		}
	}
	// Without anthropic-version, /v1/models stays OpenAI-shaped.
	_, out := s.Do("GET", "/v1/models", nil, map[string]string{"Authorization": "Bearer " + key})
	if out["object"] != "list" {
		t.Fatalf("openai models %v", out)
	}
	resp, out := s.Do("GET", "/anthropic/v1/models/haiku", nil, anthropicHeaders)
	if resp.StatusCode != 200 || out["id"] != "haiku" {
		t.Fatalf("get model %d %v", resp.StatusCode, out)
	}
	code, out, _ := anthropic(t, s, key, "/v1/messages/count_tokens", map[string]any{"model": "haiku", "messages": say("hello there")})
	if code != 200 || out["input_tokens"].(float64) <= 0 {
		t.Fatalf("count_tokens %d %v", code, out)
	}
}
