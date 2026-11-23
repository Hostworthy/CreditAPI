package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atb00ker/CreditAPI/src/internal/agent"
	"github.com/atb00ker/CreditAPI/src/internal/agent/claude"
	"github.com/atb00ker/CreditAPI/src/internal/agent/cursor"
	"github.com/atb00ker/CreditAPI/src/internal/config"
	"github.com/atb00ker/CreditAPI/src/internal/keys"
	"github.com/atb00ker/CreditAPI/src/internal/subscriptions"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakeclaude"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakes"
)

func TestMain(m *testing.M) {
	fakes.MaybeRun()
	os.Exit(m.Run())
}

func testConfig() *config.Config {
	return &config.Config{
		DefaultModel: "sonnet", Models: []string{"opus", "sonnet", "haiku"},
		ModelAliases: map[string]string{"gpt-4o": "sonnet"}, DefaultSystemPrompt: "You are a helpful assistant.",
		MaxConcurrent: 2, QueueTimeout: time.Second, RequestTimeout: 30 * time.Second,
		ImageMaxDimension: 64, MaxImages: 20, MaxBodyBytes: 10 << 20,
	}
}

// testHandler returns a handler whose only subscription ("Claude") runs the
// fake CLI in the given scenario, a key that may use it and the record path.
func testHandler(t *testing.T, scenario string) (*Handler, string, string) {
	t.Helper()
	dir := t.TempDir()
	ks, err := keys.Open(filepath.Join(dir, "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	k, secret, err := ks.Create("test")
	if err != nil {
		t.Fatal(err)
	}
	store, _, err := subscriptions.OpenStore(filepath.Join(dir, "subscriptions.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("Claude", "claude", []string{k.ID}, "OPENAI_TEST_TOKEN"); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	record := filepath.Join(dir, "record.json")
	bridge, err := agent.NewBridge()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	t.Setenv(fakeclaude.EnvScenario, scenario)
	t.Setenv(fakeclaude.EnvRecord, record)
	t.Setenv("OPENAI_TEST_TOKEN", "tok")
	cfg := testConfig()
	subs := subscriptions.NewManager(dir, store, &agent.EnvBuilder{},
		subscriptions.Routing{Aliases: cfg.ModelAliases, Strict: cfg.StrictModels},
		&claude.Backend{Bin: exe, WorkDir: dir, Bridge: bridge, Default: cfg.DefaultModel, List: cfg.Models},
		&cursor.Backend{Bin: "/bin/false", Default: "auto", List: []string{"auto"}})
	return NewHandler(cfg, ks, subs, agent.NewSlots(cfg.MaxConcurrent)), secret, record
}

func pngDataURL(t *testing.T, w, h int) string {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{200, 30, 30, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestConvertSingleUserMessage(t *testing.T) {
	h, _, _ := testHandler(t, "text")
	msgs := []Message{
		{Role: "system", Content: json.RawMessage(`"Be brief."`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"What is this?"},{"type":"image_url","image_url":{"url":"` + pngDataURL(t, 8, 8) + `"}}]`)},
	}
	conv, err := h.convertMessages(context.Background(), msgs, false)
	if err != nil {
		t.Fatal(err)
	}
	if conv.system != "Be brief." || conv.images != 1 || len(conv.content) != 2 {
		t.Fatalf("conv %+v", conv)
	}
	if conv.content[0].Text != "What is this?" || conv.content[1].Source.MediaType != "image/png" {
		t.Fatalf("content %+v", conv.content)
	}
}

func TestConvertTranscriptWithTools(t *testing.T) {
	h, _, _ := testHandler(t, "text")
	msgs := []Message{
		{Role: "user", Content: json.RawMessage(`"Log 2 eggs"`)},
		{Role: "assistant", Content: json.RawMessage(`null`), ToolCalls: []ToolCallMsg{{ID: "call_1", Type: "function", Function: FunctionCall{Name: "log_food", Arguments: `{"food":"egg","qty":2}`}}}},
		{Role: "tool", ToolCallID: "call_1", Content: json.RawMessage(`"{\"ok\":true}"`)},
	}
	conv, err := h.convertMessages(context.Background(), msgs, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(conv.content) != 1 {
		t.Fatalf("expected one text block, got %+v", conv.content)
	}
	text := conv.content[0].Text
	for _, want := range []string{
		"<user>\nLog 2 eggs\n</user>",
		`<tool_call id="call_1" name="log_food">{"food":"egg","qty":2}</tool_call>`,
		`<tool_result tool_call_id="call_1" name="log_food">` + "\n{\"ok\":true}\n</tool_result>",
		"call more tools if needed",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("transcript missing %q:\n%s", want, text)
		}
	}
}

func TestConvertErrors(t *testing.T) {
	h, _, _ := testHandler(t, "text")
	cases := [][]Message{
		{{Role: "system", Content: json.RawMessage(`"only system"`)}},
		{{Role: "user", Content: json.RawMessage(`[{"type":"input_audio"}]`)}},
		{{Role: "wizard", Content: json.RawMessage(`"x"`)}},
		{{Role: "user", Content: json.RawMessage(`[{"type":"image_url","image_url":{"url":"data:image/png;base64,bm90IGFuIGltYWdl"}}]`)}},
	}
	for i, msgs := range cases {
		if _, err := h.convertMessages(context.Background(), msgs, false); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
}

func TestBuildToolsAndChoice(t *testing.T) {
	req := &ChatRequest{Tools: []ToolDef{
		{Type: "function", Function: FunctionDef{Name: "log.food", Parameters: json.RawMessage(`{"properties":{"a":{"type":"string"}}}`)}},
		{Type: "function", Function: FunctionDef{Name: "get_goals"}},
	}}
	tools, names, err := buildTools(req, parseToolChoice(nil, nil))
	if err != nil || len(tools) != 2 {
		t.Fatalf("tools %v err %v", tools, err)
	}
	if tools[0].Name != "log_food" || names["log_food"] != "log.food" {
		t.Fatalf("name mapping %v %v", tools[0].Name, names)
	}
	if !strings.Contains(string(tools[0].InputSchema), `"type":"object"`) || string(tools[1].InputSchema) != `{"type":"object","properties":{}}` {
		t.Fatalf("schemas %s / %s", tools[0].InputSchema, tools[1].InputSchema)
	}
	tc := parseToolChoice(json.RawMessage(`{"type":"function","function":{"name":"get_goals"}}`), nil)
	tools, _, _ = buildTools(req, tc)
	if len(tools) != 1 || tools[0].Name != "get_goals" {
		t.Fatalf("forced tool %v", tools)
	}
	if tools, _, _ := buildTools(req, parseToolChoice(json.RawMessage(`"none"`), nil)); tools != nil {
		t.Fatal("none should drop tools")
	}
	long := strings.Repeat("x", 80)
	if n := claudeToolName(long); len(n)+len(agent.BridgeToolPrefix) > 64 {
		t.Fatalf("name too long: %d", len(n))
	}
}

func TestStopFilter(t *testing.T) {
	f := newStopFilter([]string{"STOP"})
	var out strings.Builder
	hit := false
	for _, p := range []string{"one ", "two ", "ST", "OP three"} {
		s, h := f.push(p)
		out.WriteString(s)
		if h {
			hit = true
			break
		}
	}
	if !hit || out.String() != "one two " {
		t.Fatalf("got %q hit=%v", out.String(), hit)
	}
	f = newStopFilter(nil)
	if s, h := f.push("abc"); s != "abc" || h {
		t.Fatal("no stops should pass through")
	}
	f = newStopFilter([]string{"ZZ"})
	s1, _ := f.push("héllo")
	if s1+f.flush() != "héllo" {
		t.Fatal("flush lost text")
	}
}

func TestEffortAndSchema(t *testing.T) {
	if effortFor("minimal", "") != "low" || effortFor("", "high") != "high" || effortFor("weird", "") != "" {
		t.Fatal("effort mapping")
	}
	s, err := jsonSchemaFor(&ResponseFormat{Type: "json_object"})
	if err != nil || s != nil {
		t.Fatal("json_object uses instructions, not a schema")
	}
	if _, err := jsonSchemaFor(&ResponseFormat{Type: "json_schema"}); err == nil {
		t.Fatal("missing schema should fail")
	}
}

// jpegWithOrientation builds a w×h JPEG (left half red, right half blue)
// carrying an EXIF orientation tag.
func jpegWithOrientation(t *testing.T, w, h, orientation int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{255, 0, 0, 255}
			if x >= w/2 {
				c = color.RGBA{0, 0, 255, 255}
			}
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95})
	raw := buf.Bytes()
	// TIFF (big endian) with one IFD entry: Orientation.
	tiff := []byte("MM\x00\x2a\x00\x00\x00\x08")
	entry := make([]byte, 2+12+4)
	binary.BigEndian.PutUint16(entry[0:], 1)
	binary.BigEndian.PutUint16(entry[2:], 0x0112)
	binary.BigEndian.PutUint16(entry[4:], 3)
	binary.BigEndian.PutUint32(entry[6:], 1)
	binary.BigEndian.PutUint16(entry[10:], uint16(orientation))
	payload := append([]byte("Exif\x00\x00"), append(tiff, entry...)...)
	seg := []byte{0xFF, 0xE1, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}
	out := append([]byte{}, raw[:2]...)
	out = append(out, seg...)
	out = append(out, payload...)
	return append(out, raw[2:]...)
}

func TestImageOrientationAndResize(t *testing.T) {
	data := jpegWithOrientation(t, 40, 20, 6) // rotate 90° clockwise
	if o := jpegOrientation(data); o != 6 {
		t.Fatalf("orientation %d", o)
	}
	p := NewImageProcessor(1568, false, false)
	out, mt, err := p.normalize(data)
	if err != nil {
		t.Fatal(err)
	}
	img, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if mt != "image/jpeg" || img.Bounds().Dx() != 20 || img.Bounds().Dy() != 40 {
		t.Fatalf("expected rotated 20x40, got %v", img.Bounds())
	}
	// After a clockwise rotation the red (left) half ends up on top.
	r, _, b, _ := img.At(10, 5).RGBA()
	if r < b {
		t.Fatal("top should be red after rotation")
	}

	p = NewImageProcessor(16, false, false)
	big := jpegWithOrientation(t, 64, 32, 1)
	out, _, err = p.normalize(big)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, _ := image.DecodeConfig(bytes.NewReader(out))
	if cfg.Width != 16 || cfg.Height != 8 {
		t.Fatalf("resized to %dx%d", cfg.Width, cfg.Height)
	}
}

func TestImageInputs(t *testing.T) {
	p := NewImageProcessor(0, false, false)
	url := pngDataURL(t, 4, 4)
	if _, err := p.Load(context.Background(), url); err != nil {
		t.Fatalf("data url: %v", err)
	}
	bare := strings.TrimPrefix(url, "data:image/png;base64,")
	if blk, err := p.Load(context.Background(), bare); err != nil || blk.Source.MediaType != "image/png" {
		t.Fatalf("bare base64: %v", err)
	}
	// Wrong declared type is corrected by sniffing.
	if blk, err := p.Load(context.Background(), "data:image/jpeg;base64,"+bare); err != nil || blk.Source.MediaType != "image/png" {
		t.Fatalf("sniff: %v", err)
	}
	if _, err := p.Load(context.Background(), "https://example.com/a.png"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("urls disabled: %v", err)
	}
	p = NewImageProcessor(0, true, false)
	if _, err := p.Load(context.Background(), "http://127.0.0.1:1/a.png"); err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("ssrf guard: %v", err)
	}
	heic := append([]byte{0, 0, 0, 24}, []byte("ftypheic0000")...)
	if _, _, err := p.normalize(heic); err == nil || !strings.Contains(err.Error(), "heic") {
		t.Fatalf("heic: %v", err)
	}
}

func doJSON(t *testing.T, srv *httptest.Server, key, path string, body any) (*http.Response, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func newServer(t *testing.T, scenario string) (*httptest.Server, string, string) {
	h, key, record := testHandler(t, scenario)
	mux := http.NewServeMux()
	h.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, key, record
}

func TestHTTPAuthAndModels(t *testing.T) {
	srv, key, _ := newServer(t, "text")
	resp, _ := doJSON(t, srv, "", "/v1/chat/completions", map[string]any{})
	if resp.StatusCode != 401 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	resp, _ = doJSON(t, srv, "sk-credit-wrong", "/v1/chat/completions", map[string]any{})
	if resp.StatusCode != 401 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var models struct {
		Data []struct{ ID string } `json:"data"`
	}
	_ = json.NewDecoder(res.Body).Decode(&models)
	res.Body.Close()
	if len(models.Data) != 7 || models.Data[0].ID != "sonnet" || models.Data[6].ID != "claude/haiku" {
		t.Fatalf("models %+v", models)
	}
}

func TestHTTPChatCompletion(t *testing.T) {
	srv, key, record := newServer(t, "text")
	resp, out := doJSON(t, srv, key, "/v1/chat/completions", map[string]any{
		"model": "gpt-4o", "max_tokens": 50,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %v", resp.StatusCode, out)
	}
	choice := out["choices"].([]any)[0].(map[string]any)
	if choice["message"].(map[string]any)["content"] != "Hello world" || choice["finish_reason"] != "stop" {
		t.Fatalf("out %v", out)
	}
	if out["model"] != "gpt-4o" || out["usage"].(map[string]any)["completion_tokens"].(float64) != 5 {
		t.Fatalf("out %v", out)
	}
	b, _ := os.ReadFile(record)
	var rec fakeclaude.Record
	_ = json.Unmarshal(b, &rec)
	if !strings.Contains(strings.Join(rec.Args, " "), "--model sonnet") || rec.Env["CLAUDE_CODE_MAX_OUTPUT_TOKENS"] != "256" {
		t.Fatalf("record %+v", rec)
	}
	if rec.SystemPrompt != "You are a helpful assistant." {
		t.Fatalf("system %q", rec.SystemPrompt)
	}
}

func readSSE(t *testing.T, resp *http.Response) ([]map[string]any, bool) {
	t.Helper()
	var events []map[string]any
	done := false
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			done = true
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("bad chunk %q", data)
		}
		events = append(events, m)
	}
	return events, done
}

func streamRequest(t *testing.T, srv *httptest.Server, key string, body map[string]any) *http.Response {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestHTTPStreamText(t *testing.T) {
	srv, key, _ := newServer(t, "text")
	resp := streamRequest(t, srv, key, map[string]any{
		"model": "sonnet", "stream": true, "stream_options": map[string]any{"include_usage": true},
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	events, done := readSSE(t, resp)
	if !done {
		t.Fatal("missing [DONE]")
	}
	var text strings.Builder
	var finish any
	var usage any
	for _, e := range events {
		if u, ok := e["usage"]; ok {
			usage = u
		}
		for _, c := range e["choices"].([]any) {
			ch := c.(map[string]any)
			if d, ok := ch["delta"].(map[string]any); ok {
				if s, ok := d["content"].(string); ok {
					text.WriteString(s)
				}
			}
			if ch["finish_reason"] != nil {
				finish = ch["finish_reason"]
			}
		}
	}
	if text.String() != "Hello world" || finish != "stop" || usage == nil {
		t.Fatalf("text %q finish %v usage %v", text.String(), finish, usage)
	}
}

func TestHTTPToolCalls(t *testing.T) {
	srv, key, _ := newServer(t, "tools")
	tools := []map[string]any{{"type": "function", "function": map[string]any{
		"name": "get_weather", "parameters": map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
	}}}
	resp, out := doJSON(t, srv, key, "/v1/chat/completions", map[string]any{
		"model": "sonnet", "tools": tools, "messages": []map[string]any{{"role": "user", "content": "weather?"}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %v", resp.StatusCode, out)
	}
	choice := out["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	calls := msg["tool_calls"].([]any)
	if choice["finish_reason"] != "tool_calls" || len(calls) != 2 {
		t.Fatalf("out %v", out)
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"Paris"}` {
		t.Fatalf("call %v", fn)
	}

	// Streaming variant with parallel_tool_calls=false keeps only the first call.
	sresp := streamRequest(t, srv, key, map[string]any{
		"model": "sonnet", "stream": true, "tools": tools, "parallel_tool_calls": false,
		"messages": []map[string]any{{"role": "user", "content": "weather?"}},
	})
	events, done := readSSE(t, sresp)
	if !done {
		t.Fatal("missing [DONE]")
	}
	var args strings.Builder
	indexes := map[float64]bool{}
	var finish any
	for _, e := range events {
		for _, c := range e["choices"].([]any) {
			ch := c.(map[string]any)
			if ch["finish_reason"] != nil {
				finish = ch["finish_reason"]
			}
			d, _ := ch["delta"].(map[string]any)
			tcs, _ := d["tool_calls"].([]any)
			for _, tc := range tcs {
				m := tc.(map[string]any)
				indexes[m["index"].(float64)] = true
				args.WriteString(m["function"].(map[string]any)["arguments"].(string))
			}
		}
	}
	if finish != "tool_calls" || args.String() != `{"city":"Paris"}` || len(indexes) != 1 {
		t.Fatalf("stream finish %v args %q indexes %v", finish, args.String(), indexes)
	}
}

func TestHTTPStructuredOutput(t *testing.T) {
	srv, key, _ := newServer(t, "schema")
	resp, out := doJSON(t, srv, key, "/v1/chat/completions", map[string]any{
		"model": "sonnet", "messages": []map[string]any{{"role": "user", "content": "banana nutrition"}},
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "food", "schema": map[string]any{"type": "object"}}},
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %v", resp.StatusCode, out)
	}
	content := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
	var v map[string]any
	if err := json.Unmarshal([]byte(content), &v); err != nil || v["food"] != "banana" {
		t.Fatalf("content %q", content)
	}
}

func TestHTTPAuthErrorAndStop(t *testing.T) {
	srv, key, _ := newServer(t, "auth")
	resp, out := doJSON(t, srv, key, "/v1/chat/completions", map[string]any{
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	e := out["error"].(map[string]any)
	if resp.StatusCode != 401 || e["code"] != "subscription_login_required" || e["type"] != "authentication_error" {
		t.Fatalf("status %d %v", resp.StatusCode, out)
	}
	if want := srv.URL + "/login?subscription="; !strings.Contains(e["message"].(string), want) ||
		!strings.HasPrefix(e["message"].(string), "Sign-in required: Claude is signed out") {
		t.Fatalf("message should link to %s: %v", want, e["message"])
	}

	srv, key, _ = newServer(t, "slow")
	start := time.Now()
	resp, out = doJSON(t, srv, key, "/v1/chat/completions", map[string]any{
		"stop": "STOP", "messages": []map[string]any{{"role": "user", "content": "count"}},
	})
	if resp.StatusCode != 200 || time.Since(start) > 5*time.Second {
		t.Fatalf("status %d after %v", resp.StatusCode, time.Since(start))
	}
	content := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"]
	if content != "one two " {
		t.Fatalf("content %q", content)
	}
}

func TestLoginURL(t *testing.T) {
	h, _, _ := testHandler(t, "text")
	r := httptest.NewRequest("POST", "http://claude-api:8080/v1/chat/completions", nil)
	if got := h.loginURL(r); got != "http://claude-api:8080/login" {
		t.Fatalf("derived %q", got)
	}
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "claude.example.com")
	if got := h.loginURL(r); got != "https://claude.example.com/login" {
		t.Fatalf("forwarded %q", got)
	}
	h.cfg.PublicURL = "https://ai.example.org"
	if got := h.loginURL(r); got != "https://ai.example.org/login" {
		t.Fatalf("public %q", got)
	}
}

func TestStreamLoginRequired(t *testing.T) {
	srv, key, _ := newServer(t, "auth")
	resp := streamRequest(t, srv, key, map[string]any{
		"stream": true, "messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	events, done := readSSE(t, resp)
	if !done {
		t.Fatal("missing [DONE]")
	}
	last := events[len(events)-1]
	e, ok := last["error"].(map[string]any)
	if !ok || e["code"] != "subscription_login_required" || !strings.Contains(e["message"].(string), "/login?subscription=") {
		t.Fatalf("last event %v", last)
	}
}

func TestImageURLFetch(t *testing.T) {
	png8 := pngDataURL(t, 8, 8)
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(png8, "data:image/png;base64,"))
	mux := http.NewServeMux()
	mux.HandleFunc("/food.png", func(w http.ResponseWriter, r *http.Request) { w.Write(raw) })
	mux.HandleFunc("/missing.png", http.NotFound)
	mux.HandleFunc("/huge.png", func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, maxImageDownload+10)) })
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// The SSRF guard blocks the loopback test server unless private URLs are allowed.
	if _, err := NewImageProcessor(0, true, false).Load(context.Background(), srv.URL+"/food.png"); err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("guard: %v", err)
	}
	p := NewImageProcessor(0, true, true)
	blk, err := p.Load(context.Background(), srv.URL+"/food.png")
	if err != nil || blk.Source.MediaType != "image/png" {
		t.Fatalf("fetch: %v %+v", err, blk)
	}
	for path, want := range map[string]string{"/missing.png": "HTTP 404", "/huge.png": "larger than 20 MB", "/loop": "redirects"} {
		if _, err := p.Load(context.Background(), srv.URL+path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v (want %q)", path, err, want)
		}
	}
	for _, bad := range []string{"data:image/png;base64", "ftp://example.com/a.png", "short"} {
		if _, err := p.Load(context.Background(), bad); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
	// URL-encoded (non-base64) data URLs are accepted too.
	if _, err := p.Load(context.Background(), "data:image/png,"+url.PathEscape(string(raw))); err != nil {
		t.Errorf("percent-encoded data URL: %v", err)
	}
}

func TestJPEGOrientationEdgeCases(t *testing.T) {
	for name, b := range map[string][]byte{
		"empty":     nil,
		"not jpeg":  []byte("\x89PNG...."),
		"truncated": {0xFF, 0xD8, 0xFF, 0xE1, 0x00},
		"no exif":   {0xFF, 0xD8, 0xFF, 0xDA, 0x00, 0x02},
		"bad tiff":  append([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x10}, []byte("Exif\x00\x00XX\x00\x2a\x00\x00")...),
	} {
		if o := jpegOrientation(b); o != 0 {
			t.Errorf("%s: orientation %d", name, o)
		}
	}
	for o := 2; o <= 8; o++ {
		data := jpegWithOrientation(t, 6, 4, o)
		out, _, err := NewImageProcessor(0, false, false).normalize(data)
		if err != nil {
			t.Fatalf("orientation %d: %v", o, err)
		}
		cfg, _, _ := image.DecodeConfig(bytes.NewReader(out))
		wantW, wantH := 6, 4
		if o >= 5 {
			wantW, wantH = 4, 6
		}
		if cfg.Width != wantW || cfg.Height != wantH {
			t.Errorf("orientation %d: %dx%d", o, cfg.Width, cfg.Height)
		}
	}
}

func TestEffortMapping(t *testing.T) {
	cases := map[string]string{"minimal": "low", "none": "low", "low": "low", "medium": "medium", "HIGH": "high", "xhigh": "xhigh", "max": "max", "": "dflt", "turbo": "dflt"}
	for in, want := range cases {
		if got := effortFor(in, "dflt"); got != want {
			t.Errorf("effortFor(%q) = %q, want %q", in, got, want)
		}
	}
}
