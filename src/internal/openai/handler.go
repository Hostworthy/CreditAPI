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
	"time"

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
	cr := chatRun{id: id, created: created, model: respModel, singleTool: singleTool, loginURL: loginURL,
		includeUsage: req.StreamOptions != nil && req.StreamOptions.IncludeUsage}
	var res *agent.Result
	if req.Stream {
		res, err = h.stream(ctx, w, run, cr)
	} else {
		res, err = h.complete(ctx, w, run, cr)
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

// chatRun is what the response writers need to know about a request.
type chatRun struct {
	id           string
	created      int64
	model        string // as the client asked for it
	singleTool   bool   // parallel_tool_calls: false
	includeUsage bool   // stream_options.include_usage
	loginURL     string
}

func (h *Handler) complete(ctx context.Context, w http.ResponseWriter, a *attempt, out chatRun) (*agent.Result, error) {
	id, created, model, singleTool, loginURL := out.id, out.created, out.model, out.singleTool, out.loginURL
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
