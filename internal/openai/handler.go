package openai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/atb00ker/claude-max-to-api/internal/claude"
	"github.com/atb00ker/claude-max-to-api/internal/config"
	"github.com/atb00ker/claude-max-to-api/internal/keys"
)

// Handler serves the OpenAI-compatible endpoints.
type Handler struct {
	cfg    *config.Config
	keys   *keys.Store
	runner *claude.Runner
	images *ImageProcessor
}

// NewHandler wires the handler.
func NewHandler(cfg *config.Config, ks *keys.Store, runner *claude.Runner) *Handler {
	return &Handler{
		cfg:    cfg,
		keys:   ks,
		runner: runner,
		images: NewImageProcessor(cfg.ImageMaxDimension, cfg.AllowImageURLs, cfg.AllowPrivateImageURLs),
	}
}

// Register adds the routes to mux. Paths are served with and without /v1 so
// clients configured with either base URL work.
func (h *Handler) Register(mux *http.ServeMux) {
	for _, prefix := range []string{"/v1", ""} {
		mux.HandleFunc("POST "+prefix+"/chat/completions", h.auth(h.chatCompletions))
		mux.HandleFunc("GET "+prefix+"/models", h.auth(h.listModels))
		mux.HandleFunc("GET "+prefix+"/models/{id...}", h.auth(h.getModel))
	}
}

type ctxKey struct{}

func keyFrom(ctx context.Context) *keys.Key {
	k, _ := ctx.Value(ctxKey{}).(*keys.Key)
	return k
}

func (h *Handler) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		secret := ""
		if a := r.Header.Get("Authorization"); a != "" {
			if v, ok := strings.CutPrefix(a, "Bearer "); ok {
				secret = strings.TrimSpace(v)
			} else if v, ok := strings.CutPrefix(a, "bearer "); ok {
				secret = strings.TrimSpace(v)
			}
		}
		if secret == "" {
			secret = strings.TrimSpace(r.Header.Get("X-Api-Key"))
		}
		if secret == "" {
			secret = strings.TrimSpace(r.Header.Get("Api-Key"))
		}
		k := h.keys.Authenticate(secret)
		if k == nil {
			writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key",
				"Invalid or missing API key. Create a virtual key at /admin and send it as 'Authorization: Bearer <key>'.")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, k)))
	}
}

func (h *Handler) modelIDs() []string {
	seen := map[string]bool{}
	var ids []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			ids = append(ids, s)
		}
	}
	add(h.cfg.DefaultModel)
	for _, m := range h.cfg.Models {
		add(m)
	}
	for a := range h.cfg.ModelAliases {
		add(a)
	}
	return ids
}

func modelObject(id string) map[string]any {
	return map[string]any{"id": id, "object": "model", "created": 1700000000, "owned_by": "anthropic"}
}

func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	data := []map[string]any{}
	for _, id := range h.modelIDs() {
		data = append(data, modelObject(id))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (h *Handler) getModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := h.cfg.ResolveModel(id); !ok {
		writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", "model "+id+" is not available")
		return
	}
	writeJSON(w, http.StatusOK, modelObject(id))
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "chatcmpl-" + hex.EncodeToString(b)
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	key := keyFrom(r.Context())
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.cfg.MaxBodyBytes))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "request body is too large (MAX_BODY_MB)")
		return
	}
	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "invalid JSON body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "missing_messages", "messages is required")
		return
	}
	if req.N != nil && *req.N > 1 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "unsupported_n", "n > 1 is not supported")
		return
	}
	model, ok := h.cfg.ResolveModel(req.Model)
	if !ok {
		writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", "model "+req.Model+" is not available; see /v1/models")
		return
	}

	tc := parseToolChoice(req.ToolChoice, req.FunctionCall)
	tools, toolNames, err := buildTools(&req, tc)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_tools", err.Error())
		return
	}
	jsonObject := req.ResponseFormat != nil && req.ResponseFormat.Type == "json_object"
	schema, err := jsonSchemaFor(req.ResponseFormat)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_response_format", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.RequestTimeout)
	defer cancel()

	conv, err := h.convertMessages(ctx, req.Messages, len(tools) > 0)
	if err != nil {
		var re *requestError
		if errors.As(err, &re) {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_messages", err.Error())
		} else {
			writeError(w, http.StatusInternalServerError, "api_error", "internal_error", err.Error())
		}
		return
	}

	maxTokens := 0
	if req.MaxCompletionTokens != nil {
		maxTokens = *req.MaxCompletionTokens
	} else if req.MaxTokens != nil {
		maxTokens = *req.MaxTokens
	}
	if maxTokens > 0 && maxTokens < minOutputTokens {
		maxTokens = minOutputTokens
	}

	opts := claude.RunOptions{
		Model:           model,
		Effort:          effortFor(req.ReasoningEffort, h.cfg.DefaultEffort),
		SystemPrompt:    h.systemPrompt(conv.system, tools, tc, jsonObject),
		Content:         conv.content,
		Tools:           tools,
		ToolNames:       toolNames,
		JSONSchema:      schema,
		JSONObject:      jsonObject,
		MaxOutputTokens: maxTokens,
	}

	release, err := h.runner.Acquire(ctx, h.cfg.QueueTimeout)
	if err != nil {
		h.fail(w, err)
		return
	}
	defer release()

	respModel := req.Model
	if respModel == "" {
		respModel = model
	}
	id := newID()
	created := time.Now().Unix()
	stops := stopSequences(req.Stop)
	singleTool := req.ParallelToolCalls != nil && !*req.ParallelToolCalls

	var res *claude.Result
	if req.Stream {
		res, err = h.stream(ctx, w, opts, id, created, respModel, stops, singleTool, req.StreamOptions != nil && req.StreamOptions.IncludeUsage)
	} else {
		res, err = h.complete(ctx, w, opts, id, created, respModel, stops, singleTool)
	}

	attrs := []any{"key", key.Name, "model", model, "stream", req.Stream, "images", conv.images,
		"tools", len(tools), "duration", time.Since(start).Round(time.Millisecond)}
	if err != nil {
		slog.Warn("chat completion failed", append(attrs, "err", err)...)
		return
	}
	in, out := res.Usage.PromptTokens(), res.Usage.OutputTokens
	h.keys.RecordUsage(key.ID, in, out)
	slog.Info("chat completion", append(attrs, "finish", res.FinishReason, "tool_calls", len(res.ToolCalls),
		"prompt_tokens", in, "completion_tokens", out)...)
}

// minOutputTokens keeps tiny max_tokens values from starving tool calls and
// structured output.
const minOutputTokens = 256

// stopFilter truncates streamed text at the first stop sequence.
type stopFilter struct {
	stops  []string
	maxLen int
	held   string
	hit    bool
}

func newStopFilter(stops []string) *stopFilter {
	f := &stopFilter{stops: stops}
	for _, s := range stops {
		f.maxLen = max(f.maxLen, len(s))
	}
	return f
}

// push returns text that is safe to emit and whether a stop sequence was hit.
func (f *stopFilter) push(s string) (string, bool) {
	if f.hit {
		return "", true
	}
	if len(f.stops) == 0 {
		return s, false
	}
	buf := f.held + s
	cut := -1
	for _, st := range f.stops {
		if i := strings.Index(buf, st); i >= 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if cut >= 0 {
		f.hit = true
		f.held = ""
		return buf[:cut], true
	}
	keep := f.maxLen - 1
	if keep <= 0 || len(buf) <= keep {
		if keep <= 0 {
			f.held = ""
			return buf, false
		}
		f.held = buf
		return "", false
	}
	split := len(buf) - keep
	for split > 0 && !utf8.RuneStart(buf[split]) {
		split--
	}
	f.held = buf[split:]
	return buf[:split], false
}

func (f *stopFilter) flush() string {
	s := f.held
	f.held = ""
	return s
}

func (h *Handler) runWithStops(ctx context.Context, opts claude.RunOptions, stops []string, cb claude.Callbacks, onText func(string)) (*claude.Result, string, error) {
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	filter := newStopFilter(stops)
	var text strings.Builder
	cb.Text = func(delta string) {
		out, hit := filter.push(delta)
		if out != "" {
			text.WriteString(out)
			if onText != nil {
				onText(out)
			}
		}
		if hit {
			cancelRun()
		}
	}
	res, err := h.runner.Run(runCtx, opts, cb)
	if filter.hit && ctx.Err() == nil {
		// Stopped on purpose at a stop sequence.
		if res == nil {
			res = &claude.Result{}
		}
		res.Text = text.String()
		res.FinishReason = "stop"
		res.ToolCalls = nil
		return res, text.String(), nil
	}
	if err != nil {
		return nil, "", err
	}
	if rest := filter.flush(); rest != "" {
		text.WriteString(rest)
		if onText != nil {
			onText(rest)
		}
	}
	return res, text.String(), nil
}

func (h *Handler) complete(ctx context.Context, w http.ResponseWriter, opts claude.RunOptions, id string, created int64, model string, stops []string, singleTool bool) (*claude.Result, error) {
	res, text, err := h.runWithStops(ctx, opts, stops, claude.Callbacks{}, nil)
	if err != nil {
		h.fail(w, err)
		return nil, err
	}
	msg := &ResponseMsg{Role: "assistant"}
	finish := res.FinishReason
	switch {
	case res.StructuredOutput != nil:
		s := string(res.StructuredOutput)
		msg.Content = &s
	case text != "" || len(res.ToolCalls) == 0:
		msg.Content = &text
	}
	calls := res.ToolCalls
	if singleTool && len(calls) > 1 {
		calls = calls[:1]
	}
	for _, c := range calls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCallMsg{ID: c.ID, Type: "function", Function: FunctionCall{Name: c.Name, Arguments: c.Arguments}})
	}
	writeJSON(w, http.StatusOK, ChatCompletion{
		ID: id, Object: "chat.completion", Created: created, Model: model,
		Choices: []Choice{{Index: 0, Message: msg, FinishReason: &finish, Logprobs: json.RawMessage("null")}},
		Usage:   toUsage(res.Usage),
	})
	return res, nil
}

func toUsage(u claude.Usage) *Usage {
	p := u.PromptTokens()
	return &Usage{
		PromptTokens:        p,
		CompletionTokens:    u.OutputTokens,
		TotalTokens:         p + u.OutputTokens,
		PromptTokensDetails: &PromptTokensDetails{CachedTokens: u.CacheReadTokens},
	}
}

type sseWriter struct {
	mu sync.Mutex
	w  http.ResponseWriter
	f  http.Flusher
}

func (s *sseWriter) data(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.w.Write([]byte("data: "))
	_, _ = s.w.Write(b)
	_, _ = s.w.Write([]byte("\n\n"))
	s.f.Flush()
}

func (s *sseWriter) raw(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.w.Write([]byte(line))
	s.f.Flush()
}

func (h *Handler) stream(ctx context.Context, w http.ResponseWriter, opts claude.RunOptions, id string, created int64, model string, stops []string, singleTool, includeUsage bool) (*claude.Result, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		err := errors.New("streaming not supported by this connection")
		writeError(w, http.StatusInternalServerError, "api_error", "no_streaming", err.Error())
		return nil, err
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sse := &sseWriter{w: w, f: flusher}

	chunk := func(delta *ResponseMsg, finish *string) map[string]any {
		return map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []Choice{{Index: 0, Delta: delta, FinishReason: finish, Logprobs: json.RawMessage("null")}},
		}
	}
	empty := ""
	sse.data(chunk(&ResponseMsg{Role: "assistant", Content: &empty}, nil))

	// Keep proxies from closing an idle connection while Claude thinks.
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				sse.raw(": keep-alive\n\n")
			case <-done:
				return
			}
		}
	}()

	cb := claude.Callbacks{
		ToolStart: func(index int, callID, name string) {
			if singleTool && index > 0 {
				return
			}
			i := index
			sse.data(chunk(&ResponseMsg{ToolCalls: []ToolCallMsg{{Index: &i, ID: callID, Type: "function", Function: FunctionCall{Name: name, Arguments: ""}}}}, nil))
		},
		ToolArgs: func(index int, delta string) {
			if singleTool && index > 0 {
				return
			}
			i := index
			sse.data(chunk(&ResponseMsg{ToolCalls: []ToolCallMsg{{Index: &i, Function: FunctionCall{Arguments: delta}}}}, nil))
		},
	}
	onText := func(s string) {
		sse.data(chunk(&ResponseMsg{Content: &s}, nil))
	}
	res, _, err := h.runWithStops(ctx, opts, stops, cb, onText)
	if err != nil {
		if ctx.Err() == nil {
			_, typ, code, msg := describeError(err)
			sse.data(ErrorBody{Error: ErrorDetail{Message: msg, Type: typ, Code: &code}})
			sse.raw("data: [DONE]\n\n")
		}
		return nil, err
	}
	if res.StructuredOutput != nil {
		s := string(res.StructuredOutput)
		sse.data(chunk(&ResponseMsg{Content: &s}, nil))
	} else if opts.JSONObject && res.Text != "" {
		// JSON mode buffers the reply; send it even if no JSON was found.
		s := res.Text
		sse.data(chunk(&ResponseMsg{Content: &s}, nil))
	}
	finish := res.FinishReason
	sse.data(chunk(&ResponseMsg{}, &finish))
	if includeUsage {
		sse.data(map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []Choice{}, "usage": toUsage(res.Usage),
		})
	}
	sse.raw("data: [DONE]\n\n")
	return res, nil
}

func describeError(err error) (status int, typ, code, msg string) {
	var ce *claude.Error
	if errors.As(err, &ce) {
		switch ce.Kind {
		case "auth":
			return ce.Status, "service_unavailable", "claude_not_authenticated", ce.Message
		case "rate_limit":
			return ce.Status, "rate_limit_error", "rate_limit_exceeded", ce.Message
		case "busy":
			return ce.Status, "rate_limit_error", "server_busy", ce.Message
		case "overloaded":
			return ce.Status, "service_unavailable", "overloaded", ce.Message
		case "invalid_request":
			return ce.Status, "invalid_request_error", "invalid_request", ce.Message
		}
		return ce.Status, "api_error", "upstream_error", ce.Message
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "api_error", "timeout", "request timed out (REQUEST_TIMEOUT)"
	}
	return http.StatusInternalServerError, "api_error", "internal_error", err.Error()
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	if errors.Is(err, context.Canceled) {
		return // client went away
	}
	status, typ, code, msg := describeError(err)
	writeError(w, status, typ, code, msg)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	writeJSON(w, status, ErrorBody{Error: ErrorDetail{Message: msg, Type: typ, Code: &code}})
}
