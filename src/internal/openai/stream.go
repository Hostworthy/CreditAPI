package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/atb00ker/CreditAPI/src/internal/agent"
)

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

func (h *Handler) stream(ctx context.Context, w http.ResponseWriter, a *attempt, out chatRun) (*agent.Result, error) {
	id, created, model, singleTool, includeUsage, loginURL := out.id, out.created, out.model, out.singleTool, out.includeUsage, out.loginURL
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
