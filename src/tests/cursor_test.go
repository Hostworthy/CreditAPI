package integration_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/atb00ker/CreditAPI/src/tests/support"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakecursor"
)

// cursorOnly returns a server and a key whose only subscription is a Cursor
// one signed in with an API key.
func cursorOnly(t *testing.T) (*support.TestServer, string) {
	t.Helper()
	s := support.NewServer(t, support.Options{})
	id, key := s.CreateKeyFor("cursor-app", []string{})
	cur := s.CreateSubscription("Cursor", "cursor", id)
	if code, _ := s.Admin("PUT", "/admin/api/subscriptions/"+cur+"/credential", map[string]any{"credential": fakecursor.ValidKey}); code != 200 {
		t.Fatal("save key")
	}
	return s, key
}

func TestCursorChat(t *testing.T) {
	s, key := cursorOnly(t)
	resp, out := s.Chat(key, map[string]any{"model": "gpt-4o", "messages": []any{
		map[string]any{"role": "system", "content": "Be brief."},
		map[string]any{"role": "user", "content": "What is the capital of France?"},
	}})
	msg, finish := firstChoice(t, out)
	if resp.StatusCode != 200 || msg["content"] != "Fake reply to: What is the capital of France?" || finish != "stop" {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	// The alias points at a Claude model, so Cursor uses its default model;
	// the response still reports the requested name.
	if out["model"] != "gpt-4o" || out["usage"].(map[string]any)["completion_tokens"].(float64) <= 0 {
		t.Fatalf("envelope %v", out)
	}
	_, out = s.Chat(key, map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "Be brief."},
		map[string]any{"role": "user", "content": "[fake:echo-system]"},
	}})
	if support.Content(out) != "Be brief." {
		t.Fatalf("system prompt: %v", out)
	}
}

func TestCursorStreaming(t *testing.T) {
	s, key := cursorOnly(t)
	resp, events, done := s.Stream(key, map[string]any{"model": "auto", "messages": user("stream this please"),
		"stream_options": map[string]any{"include_usage": true}})
	if resp.StatusCode != 200 || !done {
		t.Fatalf("%d done=%v", resp.StatusCode, done)
	}
	var text strings.Builder
	deltas := 0
	for _, e := range events {
		for _, c := range e["choices"].([]any) {
			if d, ok := c.(map[string]any)["delta"].(map[string]any); ok {
				if s, ok := d["content"].(string); ok && s != "" {
					text.WriteString(s)
					deltas++
				}
			}
		}
	}
	// Partial deltas are streamed once; Cursor's buffered duplicate is skipped.
	if text.String() != "Fake reply to: stream this please" || deltas < 2 {
		t.Fatalf("text %q deltas %d", text.String(), deltas)
	}
}

func TestCursorStopSequence(t *testing.T) {
	s, key := cursorOnly(t)
	start := time.Now()
	_, out := s.Chat(key, map[string]any{"stop": "STOP", "messages": user("[fake:slow]")})
	if got := support.Content(out); got != "one two three " || time.Since(start) > 5*time.Second {
		t.Fatalf("content %q after %v", got, time.Since(start))
	}
}

func TestCursorImages(t *testing.T) {
	s, key := cursorOnly(t)
	_, out := s.Chat(key, map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "text", "text": "What is in this picture?"},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": support.PNGDataURL(t, 40, 30)}},
	}}}})
	if got := support.Content(out); got != "I see 1 image(s): image/png 40x30. Fake reply to: What is in this picture?" {
		t.Fatalf("reply %q", got)
	}
}

func TestCursorToolRoundTrip(t *testing.T) {
	s, key := cursorOnly(t)
	tools := []any{addTask, getAgenda}
	start := time.Now()
	_, out := s.Chat(key, map[string]any{"tools": tools, "messages": user("add a task to renew my passport")})
	if time.Since(start) > 10*time.Second {
		t.Fatal("tool call was not returned promptly")
	}
	msg, finish := firstChoice(t, out)
	calls, _ := msg["tool_calls"].([]any)
	if finish != "tool_calls" || len(calls) != 1 {
		t.Fatalf("finish %q message %v", finish, msg)
	}
	call := calls[0].(map[string]any)
	fn := call["function"].(map[string]any)
	var args map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatal(err)
	}
	if fn["name"] != "add_task" || args["priority"] != "low" || !strings.HasPrefix(call["id"].(string), "call_") {
		t.Fatalf("call %v", call)
	}

	_, out = s.Chat(key, map[string]any{"tools": tools, "messages": []any{
		map[string]any{"role": "user", "content": "add a task to renew my passport"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": calls},
		map[string]any{"role": "tool", "tool_call_id": call["id"], "content": `{"ok":true}`},
	}})
	msg, finish = firstChoice(t, out)
	if finish != "stop" || msg["content"] != `Received 1 tool result(s): {"ok":true}` {
		t.Fatalf("round 2: %v %s", msg, finish)
	}
}

func TestCursorStructuredOutput(t *testing.T) {
	s, key := cursorOnly(t)
	_, out := s.Chat(key, map[string]any{"messages": user("read this receipt"), "response_format": map[string]any{
		"type": "json_schema", "json_schema": map[string]any{"name": "receipt", "schema": map[string]any{
			"type": "object", "properties": map[string]any{"merchant": map[string]any{"type": "string"}, "items": map[string]any{"type": "integer"}},
		}},
	}})
	var v map[string]any
	if err := json.Unmarshal([]byte(support.Content(out)), &v); err != nil || v["merchant"] != "fake merchant" || v["items"] != float64(42) {
		t.Fatalf("content %q", support.Content(out))
	}
	_, out = s.Chat(key, map[string]any{"messages": user("hi"), "response_format": map[string]any{"type": "json_object"}})
	if err := json.Unmarshal([]byte(support.Content(out)), &v); err != nil || v["fake"] != true {
		t.Fatalf("json_object content %q", support.Content(out))
	}
}

func TestCursorUpstreamErrors(t *testing.T) {
	s, key := cursorOnly(t)
	for prompt, want := range map[string]int{"[fake:rate-limit]": 429, "[fake:overloaded]": 503, "[fake:crash]": 502, "[fake:bad-request]": 400} {
		if code, _, _ := s.Ask(key, "", prompt); code != want {
			t.Errorf("%s: %d, want %d", prompt, code, want)
		}
	}
}
