package openai

// The Anthropic Messages API dialect (POST /v1/messages). Requests are
// translated into the OpenAI shape and answered by the same pipeline
// (routing, fallback, images, tools), then written back the Anthropic way.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/atb00ker/CreditAPI/src/internal/agent"
)

type anthropicRequest struct {
	Model         string               `json:"model"`
	MaxTokens     *int                 `json:"max_tokens,omitempty"`
	System        json.RawMessage      `json:"system,omitempty"`
	Messages      []anthropicMessage   `json:"messages"`
	Stream        bool                 `json:"stream"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Tools         []anthropicTool      `json:"tools,omitempty"`
	ToolChoice    *anthropicToolChoice `json:"tool_choice,omitempty"`
	OutputFormat  *struct {
		Type   string          `json:"type"`
		Schema json.RawMessage `json:"schema"`
	} `json:"output_format,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicBlock struct {
	Type   string `json:"type"`
	Text   string `json:"text,omitempty"`
	Source *struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	} `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type anthropicTool struct {
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

// anthropicBlocks reads content that is either a string or a block list.
func anthropicBlocks(raw json.RawMessage) ([]anthropicBlock, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []anthropicBlock{{Type: "text", Text: s}}, nil
	}
	var blocks []anthropicBlock
	err := json.Unmarshal(raw, &blocks)
	return blocks, err
}

func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// toChatRequest translates an Anthropic request into the OpenAI shape.
func (ar *anthropicRequest) toChatRequest() (*ChatRequest, error) {
	if len(ar.Messages) == 0 {
		return nil, badRequest("messages is required")
	}
	req := &ChatRequest{Model: ar.Model, Stream: ar.Stream, MaxTokens: ar.MaxTokens}

	if sys, err := anthropicBlocks(ar.System); err != nil {
		return nil, badRequest("system must be a string or a list of text blocks")
	} else if len(sys) > 0 {
		var parts []string
		for _, b := range sys {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		if len(parts) > 0 {
			req.Messages = append(req.Messages, Message{Role: "system", Content: jsonString(strings.Join(parts, "\n\n"))})
		}
	}

	for i, m := range ar.Messages {
		blocks, err := anthropicBlocks(m.Content)
		if err != nil {
			return nil, badRequest("messages[%d].content must be a string or a list of content blocks", i)
		}
		switch m.Role {
		case "user":
			var parts []ContentPart
			for _, b := range blocks {
				switch b.Type {
				case "text":
					parts = append(parts, ContentPart{Type: "text", Text: b.Text})
				case "image":
					ref, err := anthropicImage(b)
					if err != nil {
						return nil, badRequest("messages[%d]: %v", i, err)
					}
					parts = append(parts, ContentPart{Type: "image_url", ImageURL: jsonString(ref)})
				case "tool_result":
					// Tool results become OpenAI tool messages, before the
					// rest of the user turn.
					text, err := toolResultText(b)
					if err != nil {
						return nil, badRequest("messages[%d]: %v", i, err)
					}
					req.Messages = append(req.Messages, Message{Role: "tool", ToolCallID: b.ToolUseID, Content: jsonString(text)})
				default:
					return nil, badRequest("messages[%d]: content block type %q is not supported", i, b.Type)
				}
			}
			if len(parts) > 0 {
				raw, _ := json.Marshal(parts)
				req.Messages = append(req.Messages, Message{Role: "user", Content: raw})
			}
		case "assistant":
			msg := Message{Role: "assistant"}
			var text []string
			for _, b := range blocks {
				switch b.Type {
				case "text":
					text = append(text, b.Text)
				case "tool_use":
					args := strings.TrimSpace(string(b.Input))
					if args == "" || args == "null" {
						args = "{}"
					}
					msg.ToolCalls = append(msg.ToolCalls, ToolCallMsg{ID: b.ID, Type: "function", Function: FunctionCall{Name: b.Name, Arguments: args}})
				case "thinking", "redacted_thinking":
					// Not replayed.
				default:
					return nil, badRequest("messages[%d]: content block type %q is not supported", i, b.Type)
				}
			}
			if len(text) > 0 {
				msg.Content = jsonString(strings.Join(text, ""))
			}
			req.Messages = append(req.Messages, msg)
		default:
			return nil, badRequest("messages[%d].role must be user or assistant", i)
		}
	}

	for _, t := range ar.Tools {
		if t.Type != "" && t.Type != "custom" {
			return nil, badRequest("tool %q: server tools (%s) are not supported; define the tool with name, description and input_schema", t.Name, t.Type)
		}
		req.Tools = append(req.Tools, ToolDef{Type: "function", Function: FunctionDef{Name: t.Name, Description: t.Description, Parameters: t.InputSchema}})
	}
	if tc := ar.ToolChoice; tc != nil {
		switch tc.Type {
		case "auto", "":
			req.ToolChoice = jsonString("auto")
		case "any":
			req.ToolChoice = jsonString("required")
		case "none":
			req.ToolChoice = jsonString("none")
		case "tool":
			req.ToolChoice, _ = json.Marshal(map[string]any{"type": "function", "function": map[string]string{"name": tc.Name}})
		default:
			return nil, badRequest("tool_choice.type %q is not supported", tc.Type)
		}
		if tc.DisableParallelToolUse {
			f := false
			req.ParallelToolCalls = &f
		}
	}
	if len(ar.StopSequences) > 0 {
		req.Stop, _ = json.Marshal(ar.StopSequences)
	}
	if of := ar.OutputFormat; of != nil {
		if of.Type != "json_schema" {
			return nil, badRequest("output_format.type must be json_schema")
		}
		raw, _ := json.Marshal(map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "output", "schema": of.Schema}})
		req.ResponseFormat = &ResponseFormat{}
		if err := json.Unmarshal(raw, req.ResponseFormat); err != nil {
			return nil, badRequest("output_format.schema is not valid JSON")
		}
	}
	return req, nil
}

func anthropicImage(b anthropicBlock) (string, error) {
	if b.Source == nil {
		return "", errors.New("image block has no source")
	}
	switch b.Source.Type {
	case "base64":
		return "data:" + b.Source.MediaType + ";base64," + b.Source.Data, nil
	case "url":
		return b.Source.URL, nil
	}
	return "", fmt.Errorf("image source type %q is not supported", b.Source.Type)
}

func toolResultText(b anthropicBlock) (string, error) {
	inner, err := anthropicBlocks(b.Content)
	if err != nil {
		return "", errors.New("tool_result content must be a string or a list of blocks")
	}
	var parts []string
	for _, ib := range inner {
		switch ib.Type {
		case "text":
			parts = append(parts, ib.Text)
		case "image":
			parts = append(parts, "[image omitted]")
		}
	}
	text := strings.Join(parts, "\n")
	if b.IsError {
		text = "Error: " + text
	}
	return text, nil
}

// --- handlers --------------------------------------------------------------------

func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	f := anthropicFormat{}
	release, ok := h.admit(w, f)
	if !ok {
		return
	}
	defer release()
	body, ok := h.readBody(w, r, f)
	if !ok {
		return
	}
	var ar anthropicRequest
	if err := json.Unmarshal(body, &ar); err != nil {
		f.writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "invalid JSON body: "+err.Error())
		return
	}
	req, err := ar.toChatRequest()
	if err != nil {
		f.writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", err.Error())
		return
	}
	h.serveChat(w, r, req, f, start)
}

// countTokens estimates input tokens (about four characters per token); the
// CLIs offer no exact count.
func (h *Handler) countTokens(w http.ResponseWriter, r *http.Request) {
	f := anthropicFormat{}
	body, ok := h.readBody(w, r, f)
	if !ok {
		return
	}
	var ar anthropicRequest
	if err := json.Unmarshal(body, &ar); err != nil {
		f.writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "invalid JSON body: "+err.Error())
		return
	}
	n := agent.EstimateTokens(string(ar.System))
	for _, m := range ar.Messages {
		n += agent.EstimateTokens(string(m.Content))
	}
	for _, t := range ar.Tools {
		n += agent.EstimateTokens(t.Name + t.Description + string(t.InputSchema))
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": n})
}

func anthropicModel(id string) map[string]any {
	return map[string]any{"type": "model", "id": id, "display_name": id, "created_at": "2025-01-01T00:00:00Z"}
}

func (h *Handler) anthropicModels(w http.ResponseWriter, r *http.Request) {
	data := []map[string]any{}
	for _, id := range h.subs.ModelsForKey(keyFrom(r.Context()).ID) {
		data = append(data, anthropicModel(id))
	}
	out := map[string]any{"data": data, "has_more": false, "first_id": nil, "last_id": nil}
	if len(data) > 0 {
		out["first_id"], out["last_id"] = data[0]["id"], data[len(data)-1]["id"]
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) anthropicGetModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := h.subs.Candidates(keyFrom(r.Context()).ID, id); err != nil {
		anthropicFormat{}.writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", "model "+id+" is not available")
		return
	}
	writeJSON(w, http.StatusOK, anthropicModel(id))
}

// isAnthropic reports whether a request comes from an Anthropic client
// (they always send anthropic-version).
func isAnthropic(r *http.Request) bool { return r.Header.Get("Anthropic-Version") != "" }

// --- response format -----------------------------------------------------------------

type anthropicFormat struct{}

// anthropicErrorType maps an HTTP status to Anthropic's error types.
func anthropicErrorType(status int, typ string) (int, string) {
	switch {
	case status == http.StatusUnauthorized:
		return status, "authentication_error"
	case status == http.StatusForbidden:
		return status, "permission_error"
	case status == http.StatusNotFound:
		return status, "not_found_error"
	case status == http.StatusRequestEntityTooLarge:
		return status, "request_too_large"
	case status == http.StatusTooManyRequests:
		return status, "rate_limit_error"
	case typ == "service_unavailable":
		return 529, "overloaded_error" // Anthropic's status for overload
	case status >= 400 && status < 500:
		return status, "invalid_request_error"
	}
	return status, "api_error"
}

func (anthropicFormat) writeError(w http.ResponseWriter, status int, typ, _ string, msg string) {
	status, t := anthropicErrorType(status, typ)
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]string{"type": t, "message": msg}})
}

func anthropicToolID(id string) string {
	if rest, ok := strings.CutPrefix(id, "call_"); ok {
		return "toolu_" + rest
	}
	return id
}

func anthropicUsage(u agent.Usage) map[string]int64 {
	return map[string]int64{"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens,
		"cache_read_input_tokens": u.CacheReadTokens, "cache_creation_input_tokens": u.CacheCreationTokens}
}

// stopReason maps the finish reason to Anthropic's stop_reason and
// stop_sequence.
func stopReason(a *attempt, res *agent.Result) (string, any) {
	switch res.FinishReason {
	case "tool_calls":
		return "tool_use", nil
	case "length":
		return "max_tokens", nil
	}
	if a.stopHit != "" {
		return "stop_sequence", a.stopHit
	}
	return "end_turn", nil
}

// finalText is the reply text: the structured output when there is one.
func finalText(a *attempt, res *agent.Result, text string) string {
	if res.StructuredOutput != nil {
		return string(res.StructuredOutput)
	}
	if a.opts.JSONObject && text == "" {
		return res.Text
	}
	return text
}

func (anthropicFormat) complete(h *Handler, ctx context.Context, w http.ResponseWriter, a *attempt, cr chatRun) (*agent.Result, error) {
	res, text, err := h.runWithStops(ctx, a, agent.Callbacks{}, nil)
	if err != nil {
		h.failAs(anthropicFormat{}, w, err, cr.loginURL)
		return nil, err
	}
	content := []map[string]any{}
	if t := finalText(a, res, text); t != "" {
		content = append(content, map[string]any{"type": "text", "text": t})
	}
	calls := res.ToolCalls
	if cr.singleTool && len(calls) > 1 {
		calls = calls[:1]
	}
	for _, c := range calls {
		input := json.RawMessage(c.Arguments)
		if !json.Valid(input) {
			input = json.RawMessage("{}")
		}
		content = append(content, map[string]any{"type": "tool_use", "id": anthropicToolID(c.ID), "name": c.Name, "input": input})
	}
	reason, seq := stopReason(a, res)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": "msg_" + strings.TrimPrefix(cr.id, "chatcmpl-"), "type": "message", "role": "assistant", "model": cr.model,
		"content": content, "stop_reason": reason, "stop_sequence": seq, "usage": anthropicUsage(res.Usage),
	})
	return res, nil
}

func (anthropicFormat) stream(h *Handler, ctx context.Context, w http.ResponseWriter, a *attempt, cr chatRun) (*agent.Result, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		err := errors.New("streaming not supported by this connection")
		anthropicFormat{}.writeError(w, http.StatusInternalServerError, "api_error", "no_streaming", err.Error())
		return nil, err
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sse := &sseWriter{w: w, f: flusher}
	event := func(name string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		sse.raw("event: " + name + "\ndata: " + string(b) + "\n\n")
	}

	event("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg_" + strings.TrimPrefix(cr.id, "chatcmpl-"), "type": "message", "role": "assistant", "model": cr.model,
		"content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
	}})

	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				event("ping", map[string]string{"type": "ping"})
			case <-done:
				return
			}
		}
	}()

	// Content blocks are opened as text or tool calls arrive and closed when
	// the next one starts.
	var mu sync.Mutex
	index, open := -1, ""
	toolIndex := map[int]int{}
	closeBlock := func() {
		if open != "" {
			event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
			open = ""
		}
	}
	openBlock := func(kind string, block map[string]any) {
		closeBlock()
		index++
		open = kind
		event("content_block_start", map[string]any{"type": "content_block_start", "index": index, "content_block": block})
	}
	onText := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		if open != "text" {
			openBlock("text", map[string]any{"type": "text", "text": ""})
		}
		event("content_block_delta", map[string]any{"type": "content_block_delta", "index": index,
			"delta": map[string]string{"type": "text_delta", "text": s}})
	}
	cb := agent.Callbacks{
		ToolStart: func(i int, id, name string) {
			if cr.singleTool && i > 0 {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			openBlock("tool", map[string]any{"type": "tool_use", "id": anthropicToolID(id), "name": name, "input": map[string]any{}})
			toolIndex[i] = index
		},
		ToolArgs: func(i int, delta string) {
			if cr.singleTool && i > 0 {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			idx, ok := toolIndex[i]
			if !ok {
				return
			}
			event("content_block_delta", map[string]any{"type": "content_block_delta", "index": idx,
				"delta": map[string]string{"type": "input_json_delta", "partial_json": delta}})
		},
	}
	res, text, err := h.runWithStops(ctx, a, cb, onText)
	if err != nil {
		if ctx.Err() == nil || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status, typ, _, msg := describeError(err, cr.loginURL)
			_, t := anthropicErrorType(status, typ)
			event("error", map[string]any{"type": "error", "error": map[string]string{"type": t, "message": h.clean(msg)}})
		}
		return nil, err
	}
	// Structured and JSON-mode replies are held back until complete.
	if t := finalText(a, res, text); t != text && t != "" {
		onText(t)
	}
	mu.Lock()
	closeBlock()
	mu.Unlock()
	reason, seq := stopReason(a, res)
	event("message_delta", map[string]any{"type": "message_delta",
		"delta": map[string]any{"stop_reason": reason, "stop_sequence": seq}, "usage": anthropicUsage(res.Usage)})
	event("message_stop", map[string]string{"type": "message_stop"})
	return res, nil
}
