// Package agent holds what every AI backend shares: request and result
// types, the MCP bridge for client tools, the sign-in driver, credential
// storage, runtime status and process helpers. Backends for each provider
// (Claude Code, Cursor) live in sub-packages.
package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// ContentBlock is a message content block (text or image), shaped like the
// Anthropic Messages API.
type ContentBlock struct {
	Type   string       `json:"type"`
	Text   string       `json:"text,omitempty"`
	Source *ImageSource `json:"source,omitempty"`
}

// ImageSource is a base64 image payload.
type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// RunOptions describes one completion.
type RunOptions struct {
	Model        string
	Effort       string
	SystemPrompt string
	Content      []ContentBlock
	// Transcript is true when Content is a rendered multi-turn conversation
	// rather than a single user message.
	Transcript bool
	// Tools are client-side functions exposed through the MCP bridge.
	Tools []Tool
	// ToolNames maps the name the model sees back to the client's name.
	ToolNames  map[string]string
	JSONSchema json.RawMessage
	// JSONObject asks for a JSON object without a schema (OpenAI json_object
	// mode). The reply is buffered and cleaned up (fences removed).
	JSONObject      bool
	MaxOutputTokens int
	// Probe marks a login check: it always runs the CLI, even when the login
	// is already known to be broken.
	Probe bool
}

// ToolCall is a function call requested by the model for the client to run.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// Usage is token accounting for one completion.
type Usage struct {
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
}

// PromptTokens is the OpenAI-style prompt token count (all input incl. cache).
func (u Usage) PromptTokens() int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
}

// EstimateTokens approximates a token count for providers that do not report
// usage (about four characters per token).
func EstimateTokens(s string) int64 {
	if s == "" {
		return 0
	}
	return int64(len(s)/4 + 1)
}

// Result is the outcome of a completion.
type Result struct {
	Text             string
	ToolCalls        []ToolCall
	StructuredOutput json.RawMessage
	FinishReason     string // stop, length, tool_calls
	Usage            Usage
	Model            string
	CostUSD          float64
}

// Callbacks receive streaming output. Any of them may be nil.
type Callbacks struct {
	Text      func(delta string)
	ToolStart func(index int, id, name string)
	ToolArgs  func(index int, delta string)
}

// Error is a classified failure with a suggested HTTP status.
type Error struct {
	// Kind is auth, rate_limit, overloaded, invalid_request, busy, forbidden,
	// not_found or upstream.
	Kind    string
	Status  int
	Message string
	// Subscription identifies the subscription that failed, when known.
	Subscription string
}

func (e *Error) Error() string { return e.Message }

// Retryable reports whether another subscription might succeed where this
// one failed.
func (e *Error) Retryable() bool {
	switch e.Kind {
	case "auth", "rate_limit", "overloaded", "upstream":
		return true
	}
	return false
}

// Classify turns a CLI or API failure message into an Error. label names the
// provider in messages ("Claude", "Cursor").
func Classify(label string, status int, msg string) *Error {
	lower := strings.ToLower(msg)
	switch {
	case status == 401 || status == 403 || strings.Contains(lower, "/login") ||
		strings.Contains(lower, "failed to authenticate") || strings.Contains(lower, "not logged in") ||
		strings.Contains(lower, "not authenticated") || strings.Contains(lower, "authentication required") ||
		strings.Contains(lower, "oauth token") ||
		strings.Contains(lower, "invalid api key") || strings.Contains(lower, "unauthorized"):
		return &Error{Kind: "auth", Status: 401, Message: label + " login required (" + Truncate(strings.TrimSpace(msg), 300) + ")"}
	case status == 429 || strings.Contains(lower, "rate limit") || strings.Contains(lower, "usage limit") ||
		strings.Contains(lower, "quota"):
		return &Error{Kind: "rate_limit", Status: 429, Message: Truncate(msg, 500)}
	case status == 529 || strings.Contains(lower, "overloaded"):
		return &Error{Kind: "overloaded", Status: 503, Message: Truncate(msg, 500)}
	case status == 400 || status == 413 || strings.Contains(lower, "invalid request") || strings.Contains(lower, "invalid_request"):
		return &Error{Kind: "invalid_request", Status: 400, Message: Truncate(msg, 500)}
	}
	return &Error{Kind: "upstream", Status: 502, Message: Truncate(msg, 500)}
}

// ExtractJSON returns the first JSON object or array found in s (also inside
// a ```json fence), or nil.
func ExtractJSON(s string) json.RawMessage {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			rest = rest[nl+1:]
		}
		if j := strings.Index(rest, "```"); j >= 0 {
			if c := strings.TrimSpace(rest[:j]); json.Valid([]byte(c)) {
				return json.RawMessage(c)
			}
		}
	}
	for i, ch := range s {
		if ch != '{' && ch != '[' {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(s[i:]))
		var raw json.RawMessage
		if dec.Decode(&raw) == nil {
			return raw
		}
	}
	return nil
}

var modelNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\[\]-]{0,127}$`)

// ValidModelName reports whether s is a plausible model name: letters,
// digits and . _ : / [ ] -, starting with a letter or digit, at most 128
// characters. Names reach the CLIs as one argument after --model.
func ValidModelName(s string) bool { return modelNameRe.MatchString(s) }

var reservedTagRe = regexp.MustCompile(`(?i)<(\s*/?\s*)(user|assistant|tool_call|tool_result|instructions)\b`)

// EscapeTags neutralizes the tags that give a prompt its structure (<user>,
// <assistant>, <tool_call>, <tool_result>, <instructions>) inside text that
// came from a client, so message content cannot close its own element and
// add a fake turn. Nothing else is changed.
func EscapeTags(s string) string { return reservedTagRe.ReplaceAllString(s, "&lt;${1}${2}") }

// MaxLineBytes caps one line of CLI output.
const MaxLineBytes = 32 << 20

// ReadLine reads one newline-terminated line of at most max bytes. A longer
// line is an error, so a misbehaving CLI cannot make the server buffer
// without limit.
func ReadLine(rd *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, err := rd.ReadSlice('\n')
		if len(line)+len(chunk) > max {
			return nil, fmt.Errorf("CLI output line longer than %d bytes", max)
		}
		line = append(line, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, err
	}
}

// Truncate shortens s to n bytes, adding an ellipsis when cut.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TailBuffer keeps the last Max bytes written to it.
type TailBuffer struct {
	mu  sync.Mutex
	Max int
	buf []byte
}

func (t *TailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.Max {
		t.buf = t.buf[len(t.buf)-t.Max:]
	}
	return len(p), nil
}

func (t *TailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
