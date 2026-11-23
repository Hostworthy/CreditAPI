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
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/atb00ker/CreditAPI/src/internal/agent"
	"github.com/atb00ker/CreditAPI/src/internal/config"
	"github.com/atb00ker/CreditAPI/src/internal/keys"
	"github.com/atb00ker/CreditAPI/src/internal/logging"
	"github.com/atb00ker/CreditAPI/src/internal/subscriptions"
)

// Handler serves the OpenAI-compatible endpoints.
type Handler struct {
	pending chan struct{} // MAX_PENDING admission
	cfg     *config.Config
	keys    *keys.Store
	subs    *subscriptions.Manager
	slots   *agent.Slots
	images  *ImageProcessor
}

// NewHandler wires the handler.
func NewHandler(cfg *config.Config, ks *keys.Store, subs *subscriptions.Manager, slots *agent.Slots) *Handler {
	images := NewImageProcessor(cfg.ImageMaxDimension, cfg.AllowImageURLs, cfg.AllowPrivateImageURLs)
	if cfg.ImageMaxPixels > 0 {
		images.MaxPixels = cfg.ImageMaxPixels
	}
	pending := cfg.MaxPending
	if pending < 1 {
		pending = max(cfg.MaxConcurrent, 1) * 4
	}
	return &Handler{
		pending: make(chan struct{}, pending),
		cfg:     cfg,
		keys:    ks,
		subs:    subs,
		slots:   slots,
		images:  images,
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

func modelObject(id string) map[string]any {
	return map[string]any{"id": id, "object": "model", "created": 1700000000, "owned_by": "creditapi"}
}

// listModels lists the models the calling key can use, through all of its
// subscriptions.
func (h *Handler) listModels(w http.ResponseWriter, r *http.Request) {
	data := []map[string]any{}
	for _, id := range h.subs.ModelsForKey(keyFrom(r.Context()).ID) {
		data = append(data, modelObject(id))
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (h *Handler) getModel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := h.subs.Candidates(keyFrom(r.Context()).ID, id); err != nil {
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
	select {
	case h.pending <- struct{}{}:
		defer func() { <-h.pending }()
	default:
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "server_busy",
			"server is busy: too many requests are waiting, try again shortly (MAX_PENDING)")
		return
	}
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
	loginURL := h.loginURL(r)
	cands, err := h.subs.Candidates(key.ID, req.Model)
	if err != nil {
		h.fail(w, err, loginURL)
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
			logging.From(r.Context()).Error(logging.Event("chat", "convert", "failed"), logging.Err(err))
			writeError(w, http.StatusInternalServerError, "api_error", "internal_error", "internal server error; the server log has the details")
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

	opts := agent.RunOptions{
		Effort:          effortFor(req.ReasoningEffort, h.cfg.DefaultEffort),
		SystemPrompt:    h.systemPrompt(conv.system, tools, tc, jsonObject),
		Content:         conv.content,
		Transcript:      conv.transcript,
		Tools:           tools,
		ToolNames:       toolNames,
		JSONSchema:      schema,
		JSONObject:      jsonObject,
		MaxOutputTokens: maxTokens,
	}

	release, err := h.slots.Acquire(ctx, h.cfg.QueueTimeout)
	if err != nil {
		h.fail(w, err, loginURL)
		return
	}
	defer release()

	respModel := req.Model
	if respModel == "" {
		respModel = cands[0].Model
	}
	id := newID()
	created := time.Now().Unix()
	stops := stopSequences(req.Stop)
	singleTool := req.ParallelToolCalls != nil && !*req.ParallelToolCalls

	run := &attempt{h: h, cands: cands, opts: opts, stops: stops}
	var res *agent.Result
	if req.Stream {
		res, err = h.stream(ctx, w, run, id, created, respModel, singleTool, req.StreamOptions != nil && req.StreamOptions.IncludeUsage, loginURL)
	} else {
		res, err = h.complete(ctx, w, run, id, created, respModel, singleTool, loginURL)
	}

	log := logging.From(r.Context()).With(slog.String("key", key.Name), slog.String("model", run.served.Model),
		slog.String("subscription", run.served.Sub.ID), slog.Bool("stream", req.Stream), slog.Int("images", conv.images),
		slog.Int("tools", len(tools)), slog.Int("attempts", run.tries), slog.Int64("duration_ms", time.Since(start).Milliseconds()))
	if err != nil {
		log.Warn(logging.Event("chat", "completion", "failed"), logging.Err(err))
		return
	}
	in, out := res.Usage.PromptTokens(), res.Usage.OutputTokens
	h.keys.RecordUsage(key.ID, in, out)
	log.Info(logging.Event("chat", "completion", "succeeded"), slog.String("finish", res.FinishReason),
		slog.Int("tool_calls", len(res.ToolCalls)), slog.Int64("prompt_tokens", in), slog.Int64("completion_tokens", out))
}

// attempt runs a request on its candidate subscriptions in order. It moves
// to the next candidate when one fails in a way another subscription might
// not (signed out, rate limited, overloaded, CLI failure), as long as nothing
// has been sent to the client yet.
type attempt struct {
	h      *Handler
	cands  []subscriptions.Candidate
	opts   agent.RunOptions
	stops  []string
	served subscriptions.Candidate
	tries  int
}

func (a *attempt) run(ctx context.Context, cb agent.Callbacks) (*agent.Result, error) {
	emitted := false
	wrapped := agent.Callbacks{
		Text: func(s string) {
			emitted = true
			if cb.Text != nil {
				cb.Text(s)
			}
		},
		ToolStart: func(i int, id, name string) {
			emitted = true
			if cb.ToolStart != nil {
				cb.ToolStart(i, id, name)
			}
		},
		ToolArgs: cb.ToolArgs,
	}
	var signedOut []subscriptions.Candidate
	var firstErr error
	// Subscriptions resting after a usage limit or overload go last; they are
	// still tried when nothing else answers.
	cands := make([]subscriptions.Candidate, 0, len(a.cands))
	var resting []subscriptions.Candidate
	for _, c := range a.cands {
		if c.Account.Status.CoolingDown() {
			resting = append(resting, c)
		} else {
			cands = append(cands, c)
		}
	}
	cands = append(cands, resting...)
	for _, c := range cands {
		a.served = c
		a.tries++
		opts := a.opts
		opts.Model = c.Model
		res, err := agent.Execute(ctx, c.Backend, c.Account, opts, wrapped)
		if err == nil {
			return res, nil
		}
		var ce *agent.Error
		if ctx.Err() != nil || emitted || !errors.As(err, &ce) || !ce.Retryable() {
			return nil, err
		}
		if ce.Kind == "auth" {
			signedOut = append(signedOut, c)
		} else if firstErr == nil {
			firstErr = err
		}
		logging.From(ctx).Info(logging.Event("chat", "subscription", "skipped"), slog.String("subscription", c.Sub.ID),
			slog.String("kind", ce.Kind))
	}
	if len(signedOut) > 0 {
		names := make([]string, len(signedOut))
		for i, c := range signedOut {
			names[i] = c.Sub.Name
		}
		msg := names[0] + " is signed out or its credential expired"
		if len(names) > 1 {
			msg = strings.Join(names, ", ") + " are signed out or their credentials expired"
		}
		return nil, &agent.Error{Kind: "auth", Status: 401, Subscription: signedOut[0].Sub.ID, Message: msg}
	}
	return nil, firstErr
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

func (h *Handler) runWithStops(ctx context.Context, a *attempt, cb agent.Callbacks, onText func(string)) (*agent.Result, string, error) {
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	filter := newStopFilter(a.stops)
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
	res, err := a.run(runCtx, cb)
	if filter.hit && ctx.Err() == nil {
		// Stopped on purpose at a stop sequence.
		if res == nil {
			res = &agent.Result{}
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

func (h *Handler) complete(ctx context.Context, w http.ResponseWriter, a *attempt, id string, created int64, model string, singleTool bool, loginURL string) (*agent.Result, error) {
	res, text, err := h.runWithStops(ctx, a, agent.Callbacks{}, nil)
	if err != nil {
		h.fail(w, err, loginURL)
		return nil, err
	}
	msg := &ResponseMsg{Role: "assistant"}
	finish := res.FinishReason
	switch {
	case res.StructuredOutput != nil:
		s := string(res.StructuredOutput)
		msg.Content = &s
	case a.opts.JSONObject && res.Text != "":
		// No JSON found: send what the model said rather than nothing.
		s := res.Text
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

func toUsage(u agent.Usage) *Usage {
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

func (h *Handler) stream(ctx context.Context, w http.ResponseWriter, a *attempt, id string, created int64, model string, singleTool, includeUsage bool, loginURL string) (*agent.Result, error) {
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

	// Keep proxies from closing an idle connection while the model thinks.
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

	cb := agent.Callbacks{
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
	res, _, err := h.runWithStops(ctx, a, cb, onText)
	if err != nil {
		// Stay silent only when the client went away; a REQUEST_TIMEOUT
		// still gets an error event so the app knows what happened.
		if ctx.Err() == nil || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			_, typ, code, msg := describeError(err, loginURL)
			msg = h.clean(msg)
			sse.data(ErrorBody{Error: ErrorDetail{Message: msg, Type: typ, Code: &code}})
			sse.raw("data: [DONE]\n\n")
		}
		return nil, err
	}
	if res.StructuredOutput != nil {
		s := string(res.StructuredOutput)
		sse.data(chunk(&ResponseMsg{Content: &s}, nil))
	} else if a.opts.JSONObject && res.Text != "" {
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

// loginURL is the address of the /login page shown in "login required"
// errors: PUBLIC_URL when set, otherwise the address the client used.
func (h *Handler) loginURL(r *http.Request) string {
	if h.cfg.PublicURL != "" {
		return h.cfg.PublicURL + "/login"
	}
	scheme := "http"
	if h.cfg.TrustedProxies.HTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + h.cfg.TrustedProxies.Host(r) + "/login"
}

func describeError(err error, loginURL string) (status int, typ, code, msg string) {
	var ce *agent.Error
	if errors.As(err, &ce) {
		switch ce.Kind {
		case "auth":
			// 401 so OpenAI SDKs do not retry; the message carries the link.
			link := loginURL
			if ce.Subscription != "" {
				link += "?subscription=" + ce.Subscription
			}
			return http.StatusUnauthorized, "authentication_error", "subscription_login_required",
				"Sign-in required: " + ce.Message + ". Sign in at " + link + " and then try again."
		case "forbidden":
			return http.StatusForbidden, "permission_error", "no_subscription", ce.Message
		case "not_found":
			return http.StatusNotFound, "invalid_request_error", "model_not_found", ce.Message
		case "rate_limit":
			return ce.Status, "rate_limit_error", "rate_limit_exceeded", ce.Message
		case "busy":
			return ce.Status, "rate_limit_error", "server_busy", ce.Message
		case "overloaded":
			return ce.Status, "service_unavailable", "overloaded", ce.Message
		case "invalid_request":
			return ce.Status, "invalid_request_error", "invalid_request", ce.Message
		case "invalid_model":
			return http.StatusBadRequest, "invalid_request_error", "invalid_model", ce.Message
		}
		return ce.Status, "api_error", "upstream_error", ce.Message
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "api_error", "timeout", "request timed out (REQUEST_TIMEOUT)"
	}
	return http.StatusInternalServerError, "api_error", "internal_error", "internal server error; the server log has the details"
}

// clean removes secrets and local paths from a message sent to a client.
func (h *Handler) clean(msg string) string {
	msg = logging.RedactString(msg)
	for path, label := range map[string]string{h.cfg.DataDir: "<data>", os.TempDir(): "<tmp>"} {
		if path != "" && path != "/" {
			msg = strings.ReplaceAll(msg, path, label)
		}
	}
	return msg
}

func (h *Handler) fail(w http.ResponseWriter, err error, loginURL string) {
	if errors.Is(err, context.Canceled) {
		return // client went away
	}
	status, typ, code, msg := describeError(err, loginURL)
	writeError(w, status, typ, code, h.clean(msg))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	writeJSON(w, status, ErrorBody{Error: ErrorDetail{Message: msg, Type: typ, Code: &code}})
}
