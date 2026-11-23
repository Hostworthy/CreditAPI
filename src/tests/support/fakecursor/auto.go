package fakecursor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/atb00ker/CreditAPI/src/tests/support/fakecore"
)

// Reply rules, in order:
//
//  1. Hardening (see checkHardening), then authentication.
//  2. Directives ([fake:crash], [fake:rate-limit], [fake:overloaded],
//     [fake:bad-request]) fail the run like the real CLI would.
//  3. A JSON schema in the prompt: reply with a value generated from it.
//  4. Client tools (MCP bridge): when the conversation ends with tool results,
//     reply "Received N tool result(s): ...". Otherwise call every tool whose
//     name appears in the text (or the first tool) through the bridge and
//     wait to be stopped.
//  5. Text directives ([fake:slow], [fake:echo-system], [fake:echo-input],
//     [fake:long], [fake:whoami]).
//  6. Otherwise "Fake reply to: <last user line>", prefixed with the attached
//     images and their sizes, or a fenced JSON object when the instructions
//     ask for one.

type session struct {
	id      string
	partial bool
}

func emit(v any) {
	b, _ := json.Marshal(v)
	os.Stdout.Write(append(b, '\n'))
}

func (s *session) init(inv *invocation, keySource string) {
	model := inv.model
	if model == "" || model == "auto" {
		model = "Auto"
	}
	emit(map[string]any{"type": "system", "subtype": "init", "apiKeySource": keySource, "cwd": inv.workspace,
		"session_id": s.id, "model": model, "permissionMode": "default"})
	emit(map[string]any{"type": "user", "message": map[string]any{"role": "user",
		"content": []map[string]any{{"type": "text", "text": fakecore.Truncate(inv.prompt, 200)}}}, "session_id": s.id})
}

func message(text string) map[string]any {
	return map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": text}}}
}

// text streams a reply: partial deltas (timestamp_ms, no model_call_id) and
// then the buffered duplicate (with model_call_id), or one complete message
// without --stream-partial-output.
func (s *session) text(reply string, delay time.Duration) {
	if !s.partial {
		emit(map[string]any{"type": "assistant", "message": message(reply), "session_id": s.id})
		return
	}
	parts := fakecore.Chunk(reply)
	if delay > 0 {
		parts = strings.SplitAfter(reply, " ")
	}
	for _, p := range parts {
		emit(map[string]any{"type": "assistant", "message": message(p), "session_id": s.id, "timestamp_ms": time.Now().UnixMilli()})
		if delay > 0 {
			time.Sleep(delay)
		}
	}
	emit(map[string]any{"type": "assistant", "message": message(reply), "session_id": s.id,
		"model_call_id": "mc_" + s.id, "timestamp_ms": time.Now().UnixMilli()})
}

func (s *session) result(reply string, start time.Time) {
	emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": reply,
		"duration_ms": time.Since(start).Milliseconds(), "duration_api_ms": time.Since(start).Milliseconds(),
		"session_id": s.id, "request_id": "req_" + s.id})
}

func (s *session) fail(msg string, start time.Time) {
	emit(map[string]any{"type": "result", "subtype": "error", "is_error": true, "result": msg,
		"duration_ms": time.Since(start).Milliseconds(), "session_id": s.id})
}

func run(inv *invocation) int {
	start := time.Now()
	if err := inv.checkHardening(); err != nil {
		fmt.Fprintln(os.Stderr, "fakecursor: workspace not hardened:", err)
		return 4
	}
	key := os.Getenv("CURSOR_API_KEY")
	keySource := "login"
	switch {
	case key != "" && !strings.HasPrefix(key, KeyPrefix):
		fmt.Fprintln(os.Stderr, "Error: Invalid API key. Please check CURSOR_API_KEY and try again (unauthorized).")
		return 1
	case strings.Contains(key, "expired"):
		fmt.Fprintln(os.Stderr, "Error: Unauthorized: this API key has expired or was revoked.")
		return 1
	case key != "":
		keySource = "env"
	case !loggedIn():
		fmt.Fprintln(os.Stderr, NotLoggedIn)
		return 1
	}

	s := &session{id: fakecore.RandomHex(8), partial: inv.partial}
	d := fakecore.Directives(inv.body)
	switch {
	case d["crash"]:
		fmt.Fprintln(os.Stderr, "fakecursor: simulated crash")
		return 3
	case d["bad-request"]:
		s.init(inv, keySource)
		s.fail("Invalid request: the conversation could not be processed (fake)", start)
		return 1
	case d["rate-limit"]:
		s.init(inv, keySource)
		s.fail("You've hit your usage limit for this billing period (fake)", start)
		return 1
	case d["overloaded"]:
		s.init(inv, keySource)
		s.fail("The model is overloaded right now, please try again (fake)", start)
		return 1
	}

	s.init(inv, keySource)
	if inv.schema != nil {
		raw, _ := json.Marshal(fakecore.SampleFromSchema(inv.schema, "value"))
		s.text(string(raw), 0)
		s.result(string(raw), start)
		return 0
	}

	if len(inv.tools) > 0 {
		if n := fakecore.ToolResults(inv.body); n > 0 && fakecore.EndsWithToolResults(inv.body) {
			reply := fmt.Sprintf("Received %d tool result(s): %s", n, fakecore.FirstToolResult(inv.body))
			s.text(reply, 0)
			s.result(reply, start)
			return 0
		}
		callTools(s, inv)
		return 0
	}

	var reply string
	var delay time.Duration
	switch {
	case d["slow"]:
		reply, delay = fakecore.SlowReply, 150*time.Millisecond
	case d["echo-system"]:
		reply = inv.instructions
	case d["echo-input"]:
		reply = strings.TrimSpace(inv.body)
	case d["long"]:
		reply = fakecore.LongReply()
	case d["whoami"]:
		reply = fakecore.Whoami("cursor")
	default:
		reply = "Fake reply to: " + fakecore.LastUserLine(inv.body)
		if desc := describeImages(inv); desc != "" {
			reply = desc + " " + reply
		}
		if strings.Contains(inv.instructions, "single valid JSON object") && !d["not-json"] {
			obj, _ := json.Marshal(map[string]any{"fake": true, "reply": fakecore.LastUserLine(inv.body)})
			reply = "```json\n" + string(obj) + "\n```"
		}
	}
	s.text(reply, delay)
	s.result(reply, start)
	return 0
}

// callTools imitates the model calling client tools: it announces them,
// calls each one through the MCP bridge (which never answers) and waits to be
// stopped by the server.
func callTools(s *session, inv *invocation) {
	chosen := fakecore.ChooseTools(inv.tools, inv.body)
	names := make([]string, len(chosen))
	for i, t := range chosen {
		names[i] = t.Name
	}
	s.text("Calling "+strings.Join(names, ", ")+".", 0)
	var wg sync.WaitGroup
	for i, t := range chosen {
		args := fakecore.SampleFromSchema(t.InputSchema, t.Name)
		emit(map[string]any{"type": "tool_call", "subtype": "started", "call_id": fmt.Sprintf("tc_%d", i+1), "session_id": s.id,
			"tool_call": map[string]any{"mcpToolCall": map[string]any{"args": map[string]any{"name": t.Name, "args": args}}}})
		wg.Add(1)
		go func() {
			defer wg.Done()
			fakecore.RPC(inv.bridgeURL, "tools/call", map[string]any{"name": t.Name, "arguments": args})
		}()
		time.Sleep(20 * time.Millisecond) // keep the call order stable
	}
	time.Sleep(30 * time.Second)
	wg.Wait()
}

func mediaType(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	}
	return "image/jpeg"
}

func describeImages(inv *invocation) string {
	var items []string
	for _, rel := range inv.images {
		size := "unreadable"
		if raw, err := os.ReadFile(filepath.Join(inv.workspace, rel)); err == nil {
			size = fakecore.ImageSize(raw)
		}
		items = append(items, mediaType(rel)+" "+size)
	}
	return fakecore.DescribeImages(items)
}
