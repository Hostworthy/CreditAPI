package integration_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/atb00ker/CreditAPI/src/tests/support"
)

// signedIn returns a server with a completed fake login and an API key.
func signedIn(t *testing.T, opts support.Options) (*support.TestServer, string) {
	t.Helper()
	s := support.NewServer(t, opts)
	s.SignIn()
	return s, s.CreateKey("test")
}

func user(text string) []map[string]any {
	return []map[string]any{{"role": "user", "content": text}}
}

func firstChoice(t *testing.T, out map[string]any) (map[string]any, string) {
	t.Helper()
	choices, ok := out["choices"].([]any)
	if !ok || len(choices) != 1 {
		t.Fatalf("expected one choice: %v", out)
	}
	c := choices[0].(map[string]any)
	return c["message"].(map[string]any), c["finish_reason"].(string)
}

func TestChatBasics(t *testing.T) {
	s, key := signedIn(t, support.Options{})

	resp, out := s.Chat(key, map[string]any{"model": "gpt-4o", "messages": user("What is the capital of France?")})
	if resp.StatusCode != 200 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	msg, finish := firstChoice(t, out)
	if msg["role"] != "assistant" || msg["content"] != "Fake reply to: What is the capital of France?" || finish != "stop" {
		t.Fatalf("message %v finish %s", msg, finish)
	}
	if out["object"] != "chat.completion" || out["model"] != "gpt-4o" || !strings.HasPrefix(out["id"].(string), "chatcmpl-") {
		t.Fatalf("envelope %v", out)
	}
	usage := out["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) <= 0 || usage["completion_tokens"].(float64) <= 0 ||
		usage["total_tokens"].(float64) != usage["prompt_tokens"].(float64)+usage["completion_tokens"].(float64) {
		t.Fatalf("usage %v", usage)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Fatal("missing X-Request-Id")
	}
}

func TestSystemPromptAndTranscript(t *testing.T) {
	s, key := signedIn(t, support.Options{})

	_, out := s.Chat(key, map[string]any{"messages": user("[fake:echo-system]")})
	if support.Content(out) != "You are a helpful assistant." {
		t.Fatalf("default system prompt: %q", support.Content(out))
	}

	_, out = s.Chat(key, map[string]any{"messages": []map[string]any{
		{"role": "system", "content": "You are a concise assistant."},
		{"role": "developer", "content": "Be brief."},
		{"role": "user", "content": "[fake:echo-system]"},
	}})
	if support.Content(out) != "You are a concise assistant.\n\nBe brief." {
		t.Fatalf("joined system prompt: %q", support.Content(out))
	}

	_, out = s.Chat(key, map[string]any{"messages": []map[string]any{
		{"role": "user", "content": "I ate oats"},
		{"role": "assistant", "content": "Logged oats."},
		{"role": "user", "content": "[fake:echo-input] and milk"},
	}})
	got := support.Content(out)
	for _, want := range []string{"<user>\nI ate oats\n</user>", "<assistant>\nLogged oats.\n</assistant>", "Write the assistant's reply to the last user message."} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript missing %q:\n%s", want, got)
		}
	}

	_, out = s.Chat(key, map[string]any{"messages": []map[string]any{
		{"role": "user", "content": "first"},
		{"role": "assistant", "content": "ok"},
		{"role": "user", "content": "second question"},
	}})
	if support.Content(out) != "Fake reply to: second question" {
		t.Fatalf("reply should address the last user turn: %q", support.Content(out))
	}
}

func TestStreaming(t *testing.T) {
	s, key := signedIn(t, support.Options{})
	resp, events, done := s.Stream(key, map[string]any{
		"messages": user("stream this please"), "stream_options": map[string]any{"include_usage": true},
	})
	if resp.Header.Get("Content-Type") != "text/event-stream" || !done {
		t.Fatalf("content type %q done %v", resp.Header.Get("Content-Type"), done)
	}
	var text strings.Builder
	var finish string
	var usage map[string]any
	for i, e := range events {
		if e["object"] != "chat.completion.chunk" {
			t.Fatalf("chunk %d: %v", i, e)
		}
		if u, ok := e["usage"].(map[string]any); ok {
			usage = u
		}
		for _, c := range e["choices"].([]any) {
			ch := c.(map[string]any)
			d := ch["delta"].(map[string]any)
			if i == 0 && d["role"] != "assistant" {
				t.Fatalf("first chunk should carry the role: %v", d)
			}
			if s, ok := d["content"].(string); ok {
				text.WriteString(s)
			}
			if f, ok := ch["finish_reason"].(string); ok {
				finish = f
			}
		}
	}
	if text.String() != "Fake reply to: stream this please" || finish != "stop" || usage == nil {
		t.Fatalf("text %q finish %q usage %v", text.String(), finish, usage)
	}
	if len(events) < 4 {
		t.Fatalf("expected several chunks, got %d", len(events))
	}
}

func TestImages(t *testing.T) {
	s, key := signedIn(t, support.Options{})
	big := support.PNGDataURL(t, 3000, 1500)
	small := support.PNGDataURL(t, 8, 8)
	bare := strings.TrimPrefix(small, "data:image/png;base64,")

	_, out := s.Chat(key, map[string]any{"messages": []map[string]any{{"role": "user", "content": []map[string]any{
		{"type": "text", "text": "What did I eat?"},
		{"type": "image_url", "image_url": map[string]any{"url": big}},
		{"type": "image_url", "image_url": small},
		{"type": "image_url", "image_url": map[string]any{"url": bare}},
	}}}})
	want := "I see 3 image(s): image/jpeg 1568x784, image/png 8x8, image/png 8x8. Fake reply to: What did I eat?"
	if support.Content(out) != want {
		t.Fatalf("got %q\nwant %q", support.Content(out), want)
	}

	resp, out := s.Chat(key, map[string]any{"messages": []map[string]any{{"role": "user", "content": []map[string]any{
		{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,bm90IGFuIGltYWdl"}},
	}}}})
	if resp.StatusCode != 400 || !strings.Contains(errorOf(t, out)["message"].(string), "image 1") {
		t.Fatalf("bad image: %d %v", resp.StatusCode, out)
	}

	resp, _ = s.Chat(key, map[string]any{"messages": []map[string]any{{"role": "user", "content": []map[string]any{
		{"type": "image_url", "image_url": map[string]any{"url": "http://127.0.0.1:1/photo.png"}},
	}}}})
	if resp.StatusCode != 400 {
		t.Fatalf("private image URL should be refused: %d", resp.StatusCode)
	}
}

func TestMaxImages(t *testing.T) {
	s, key := signedIn(t, support.Options{MaxImages: 1})
	img := support.PNGDataURL(t, 4, 4)
	_, out := s.Chat(key, map[string]any{"messages": []map[string]any{
		{"role": "user", "content": []map[string]any{{"type": "image_url", "image_url": img}, {"type": "text", "text": "old"}}},
		{"role": "assistant", "content": "ok"},
		{"role": "user", "content": []map[string]any{{"type": "image_url", "image_url": img}, {"type": "text", "text": "[fake:echo-input]"}}},
	}})
	if got := support.Content(out); !strings.Contains(got, "[earlier image omitted]") {
		t.Fatalf("older image should be dropped: %q", got)
	}
}

var addTask = map[string]any{"type": "function", "function": map[string]any{
	"name": "add_task", "description": "Add a task to the to-do list",
	"parameters": map[string]any{"type": "object", "required": []string{"title"}, "properties": map[string]any{
		"title":    map[string]any{"type": "string"},
		"hours":    map[string]any{"type": "number"},
		"priority": map[string]any{"type": "string", "enum": []string{"low", "normal", "high"}},
	}},
}}

var getAgenda = map[string]any{"type": "function", "function": map[string]any{
	"name": "get_agenda",
	"parameters": map[string]any{"type": "object", "properties": map[string]any{
		"date": map[string]any{"type": "string", "format": "date"},
	}},
}}

func TestToolRoundTrip(t *testing.T) {
	s, key := signedIn(t, support.Options{})
	tools := []any{addTask, getAgenda}

	start := time.Now()
	_, out := s.Chat(key, map[string]any{"tools": tools, "messages": user("add a task to renew my passport")})
	if time.Since(start) > 10*time.Second {
		t.Fatal("tool call was not returned promptly")
	}
	msg, finish := firstChoice(t, out)
	calls, _ := msg["tool_calls"].([]any)
	if finish != "tool_calls" || len(calls) != 1 {
		t.Fatalf("finish %q calls %v", finish, msg)
	}
	call := calls[0].(map[string]any)
	fn := call["function"].(map[string]any)
	var args map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatal(err)
	}
	if fn["name"] != "add_task" || call["type"] != "function" || !strings.HasPrefix(call["id"].(string), "call_") {
		t.Fatalf("call %v", call)
	}
	if args["title"] != "fake title" || args["priority"] != "low" || args["hours"].(float64) != 42 {
		t.Fatalf("arguments should follow the schema: %v", args)
	}

	// Round 2: the client ran the tool and sends the result back.
	assistant := map[string]any{"role": "assistant", "content": nil, "tool_calls": calls}
	_, out = s.Chat(key, map[string]any{"tools": tools, "messages": []any{
		map[string]any{"role": "user", "content": "add a task to renew my passport"},
		assistant,
		map[string]any{"role": "tool", "tool_call_id": call["id"], "content": `{"ok":true,"entry_id":7}`},
	}})
	msg, finish = firstChoice(t, out)
	if finish != "stop" || msg["content"] != `Received 1 tool result(s): {"ok":true,"entry_id":7}` {
		t.Fatalf("round 2: %v %s", msg, finish)
	}
}

func TestToolChoiceAndParallelCalls(t *testing.T) {
	s, key := signedIn(t, support.Options{})
	tools := []any{addTask, getAgenda}

	// Mentioning both tools makes the fake call both (parallel tool calls).
	_, out := s.Chat(key, map[string]any{"tools": tools, "messages": user("use add_task and get_agenda")})
	msg, _ := firstChoice(t, out)
	if calls := msg["tool_calls"].([]any); len(calls) != 2 {
		t.Fatalf("expected two calls: %v", msg)
	}
	_, out = s.Chat(key, map[string]any{"tools": tools, "parallel_tool_calls": false, "messages": user("use add_task and get_agenda")})
	msg, _ = firstChoice(t, out)
	if calls := msg["tool_calls"].([]any); len(calls) != 1 {
		t.Fatalf("parallel_tool_calls=false should keep one call: %v", msg)
	}

	// A named tool_choice exposes only that tool.
	_, out = s.Chat(key, map[string]any{"tools": tools, "tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "get_agenda"}}, "messages": user("anything")})
	msg, _ = firstChoice(t, out)
	fn := msg["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_agenda" || fn["arguments"] != `{"date":"2026-01-01"}` {
		t.Fatalf("named tool: %v", fn)
	}

	// tool_choice none answers in text.
	_, out = s.Chat(key, map[string]any{"tools": tools, "tool_choice": "none", "messages": user("just talk")})
	if msg, finish := firstChoice(t, out); finish != "stop" || msg["content"] != "Fake reply to: just talk" {
		t.Fatalf("tool_choice none: %v %s", msg, finish)
	}

	// Legacy functions are treated as tools.
	_, out = s.Chat(key, map[string]any{"functions": []any{addTask["function"]}, "messages": user("add it")})
	if _, finish := firstChoice(t, out); finish != "tool_calls" {
		t.Fatalf("legacy functions: %v", out)
	}

	// Unknown named tool is a client error.
	resp, _ := s.Chat(key, map[string]any{"tools": tools, "tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "nope"}}, "messages": user("x")})
	if resp.StatusCode != 400 {
		t.Fatalf("unknown tool_choice: %d", resp.StatusCode)
	}
}

func TestStreamingToolCalls(t *testing.T) {
	s, key := signedIn(t, support.Options{})
	_, events, done := s.Stream(key, map[string]any{"tools": []any{addTask}, "messages": user("remind me to water the plants")})
	if !done {
		t.Fatal("missing [DONE]")
	}
	var name, args, finish, id string
	for _, e := range events {
		for _, c := range e["choices"].([]any) {
			ch := c.(map[string]any)
			if f, ok := ch["finish_reason"].(string); ok {
				finish = f
			}
			d, _ := ch["delta"].(map[string]any)
			tcs, _ := d["tool_calls"].([]any)
			for _, tc := range tcs {
				m := tc.(map[string]any)
				if v, ok := m["id"].(string); ok {
					id = v
				}
				fn := m["function"].(map[string]any)
				if v, ok := fn["name"].(string); ok && v != "" {
					name = v
				}
				args += fn["arguments"].(string)
			}
		}
	}
	if name != "add_task" || finish != "tool_calls" || id == "" || !json.Valid([]byte(args)) {
		t.Fatalf("name %q id %q finish %q args %q", name, id, finish, args)
	}
}

func TestStructuredOutput(t *testing.T) {
	s, key := signedIn(t, support.Options{})
	schema := map[string]any{"type": "object", "properties": map[string]any{
		"total": map[string]any{"type": "number"}, "merchant": map[string]any{"type": "string"},
		"items": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
	}}
	_, out := s.Chat(key, map[string]any{"messages": user("read this receipt"),
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "receipt", "schema": schema}}})
	var v map[string]any
	if err := json.Unmarshal([]byte(support.Content(out)), &v); err != nil {
		t.Fatalf("not JSON: %q", support.Content(out))
	}
	if v["total"].(float64) != 42 || v["merchant"] != "fake merchant" || len(v["items"].([]any)) != 1 {
		t.Fatalf("value %v", v)
	}

	// JSON mode strips the code fence the model adds.
	_, out = s.Chat(key, map[string]any{"messages": user("hello"), "response_format": map[string]any{"type": "json_object"}})
	if support.Content(out) != `{"fake":true,"reply":"hello"}` {
		t.Fatalf("json_object: %q", support.Content(out))
	}
	_, events, _ := s.Stream(key, map[string]any{"messages": user("world"), "response_format": map[string]any{"type": "json_object"}})
	var text strings.Builder
	for _, e := range events {
		for _, c := range e["choices"].([]any) {
			if s, ok := c.(map[string]any)["delta"].(map[string]any)["content"].(string); ok {
				text.WriteString(s)
			}
		}
	}
	if text.String() != `{"fake":true,"reply":"world"}` {
		t.Fatalf("streamed json_object: %q", text.String())
	}

	resp, _ := s.Chat(key, map[string]any{"messages": user("x"), "response_format": map[string]any{"type": "json_schema"}})
	if resp.StatusCode != 400 {
		t.Fatalf("schema missing: %d", resp.StatusCode)
	}
}

func TestStopSequencesAndLength(t *testing.T) {
	s, key := signedIn(t, support.Options{})
	start := time.Now()
	_, out := s.Chat(key, map[string]any{"messages": user("[fake:slow]"), "stop": []string{"STOP"}})
	msg, finish := firstChoice(t, out)
	if msg["content"] != "one two three " || finish != "stop" {
		t.Fatalf("stop: %v %s", msg, finish)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("generation should stop at the stop sequence")
	}

	_, out = s.Chat(key, map[string]any{"messages": user("[fake:long]"), "max_tokens": 300})
	msg, finish = firstChoice(t, out)
	if finish != "length" || len(msg["content"].(string)) != 1200 {
		t.Fatalf("max_tokens: finish %s, %d chars", finish, len(msg["content"].(string)))
	}
}

func TestUpstreamErrors(t *testing.T) {
	s, key := signedIn(t, support.Options{})
	cases := []struct {
		directive string
		status    int
		code      string
	}{
		{"[fake:rate-limit]", 429, "rate_limit_exceeded"},
		{"[fake:overloaded]", 503, "overloaded"},
		{"[fake:bad-request]", 400, "invalid_request"},
		{"[fake:crash]", 502, "upstream_error"},
	}
	for _, c := range cases {
		resp, out := s.Chat(key, map[string]any{"messages": user(c.directive)})
		if resp.StatusCode != c.status || errorOf(t, out)["code"] != c.code {
			t.Errorf("%s: %d %v", c.directive, resp.StatusCode, out)
		}
	}
	// Errors during a stream arrive as an error event before [DONE].
	_, events, done := s.Stream(key, map[string]any{"messages": user("[fake:rate-limit]")})
	last := events[len(events)-1]
	if !done || errorOf(t, last)["code"] != "rate_limit_exceeded" {
		t.Fatalf("stream error: %v", events)
	}
	// A rate limit is not a login problem: the next request still runs.
	if resp, _ := s.Chat(key, hello); resp.StatusCode != 200 {
		t.Fatalf("after rate limit: %d", resp.StatusCode)
	}
}

func TestRequestValidation(t *testing.T) {
	s, key := signedIn(t, support.Options{StrictModels: true, MaxBodyBytes: 64 << 10})
	cases := []struct {
		name   string
		body   any
		status int
	}{
		{"no messages", map[string]any{"messages": []any{}}, 400},
		{"only system", map[string]any{"messages": []map[string]any{{"role": "system", "content": "x"}}}, 400},
		{"bad role", map[string]any{"messages": []map[string]any{{"role": "wizard", "content": "x"}}}, 400},
		{"n > 1", map[string]any{"n": 2, "messages": user("x")}, 400},
		{"audio", map[string]any{"messages": []map[string]any{{"role": "user", "content": []map[string]any{{"type": "input_audio"}}}}}, 400},
		{"unknown model (strict)", map[string]any{"model": "gpt-5-mega", "messages": user("x")}, 404},
		{"too large", map[string]any{"messages": user(strings.Repeat("x", 70<<10))}, 413},
		{"alias allowed (strict)", map[string]any{"model": "gpt-4o", "messages": user("x")}, 200},
	}
	for _, c := range cases {
		resp, out := s.Do("POST", "/v1/chat/completions", c.body, map[string]string{"Authorization": "Bearer " + key})
		if resp.StatusCode != c.status {
			t.Errorf("%s: %d %v", c.name, resp.StatusCode, out)
		}
	}
	resp, out := s.Do("POST", "/v1/chat/completions", nil, map[string]string{"Authorization": "Bearer " + key})
	if resp.StatusCode != 400 || errorOf(t, out)["code"] != "invalid_json" {
		t.Fatalf("empty body: %d %v", resp.StatusCode, out)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	s, key := signedIn(t, support.Options{MaxConcurrent: 1, QueueTimeout: 200 * time.Millisecond})
	done := make(chan int, 1)
	go func() {
		resp, _ := s.Chat(key, map[string]any{"messages": user("[fake:slow]")})
		done <- resp.StatusCode
	}()
	time.Sleep(300 * time.Millisecond)
	resp, out := s.Chat(key, hello)
	if resp.StatusCode != 429 || errorOf(t, out)["code"] != "server_busy" {
		t.Fatalf("busy: %d %v", resp.StatusCode, out)
	}
	if code := <-done; code != 200 {
		t.Fatalf("slow request: %d", code)
	}
}
