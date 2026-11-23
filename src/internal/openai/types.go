// Package openai implements the OpenAI-compatible HTTP API on top of the
// claude CLI runner.
package openai

import (
	"encoding/json"
	"strings"
)

// ChatRequest is the subset of the OpenAI chat completions request we use.
// Sampling parameters (temperature, top_p, ...) are accepted and ignored.
type ChatRequest struct {
	Model               string          `json:"model"`
	Messages            []Message       `json:"messages"`
	Stream              bool            `json:"stream"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
	Tools               []ToolDef       `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	Functions           []FunctionDef   `json:"functions,omitempty"`     // legacy
	FunctionCall        json.RawMessage `json:"function_call,omitempty"` // legacy
	ResponseFormat      *ResponseFormat `json:"response_format,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	N                   *int            `json:"n,omitempty"`
}

// StreamOptions controls streaming extras.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Message is one chat message.
type Message struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content,omitempty"`
	Name         string          `json:"name,omitempty"`
	ToolCalls    []ToolCallMsg   `json:"tool_calls,omitempty"`
	ToolCallID   string          `json:"tool_call_id,omitempty"`
	FunctionCall *FunctionCall   `json:"function_call,omitempty"`
}

// ContentPart is one element of an array-valued message content.
type ContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL json.RawMessage `json:"image_url,omitempty"` // {"url": "..."} or "..."
}

// ToolDef is an OpenAI tool definition.
type ToolDef struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef describes a callable function.
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolCallMsg is a tool call inside an assistant message (request or response).
type ToolCallMsg struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function FunctionCall `json:"function"`
}

// FunctionCall is the function part of a tool call.
type FunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// ResponseFormat selects text, JSON object or JSON schema output.
type ResponseFormat struct {
	Type       string `json:"type"`
	JSONSchema *struct {
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
		Strict *bool           `json:"strict,omitempty"`
	} `json:"json_schema,omitempty"`
}

// --- responses ---------------------------------------------------------------

// ChatCompletion is a non-streaming response.
type ChatCompletion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []Choice `json:"choices"`
	Usage   *Usage   `json:"usage,omitempty"`
}

// Choice is one completion choice.
type Choice struct {
	Index        int             `json:"index"`
	Message      *ResponseMsg    `json:"message,omitempty"`
	Delta        *ResponseMsg    `json:"delta,omitempty"`
	FinishReason *string         `json:"finish_reason"`
	Logprobs     json.RawMessage `json:"logprobs"`
}

// ResponseMsg is an assistant message or streaming delta.
type ResponseMsg struct {
	Role      string        `json:"role,omitempty"`
	Content   *string       `json:"content,omitempty"`
	Refusal   *string       `json:"refusal,omitempty"`
	ToolCalls []ToolCallMsg `json:"tool_calls,omitempty"`
}

// Usage is OpenAI token usage.
type Usage struct {
	PromptTokens        int64                `json:"prompt_tokens"`
	CompletionTokens    int64                `json:"completion_tokens"`
	TotalTokens         int64                `json:"total_tokens"`
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

// PromptTokensDetails reports cached prompt tokens.
type PromptTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

// ErrorBody is the OpenAI error envelope.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail describes an error.
type ErrorDetail struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    *string `json:"code"`
}

// contentParts parses message content (string, array of parts, or null).
func contentParts(raw json.RawMessage) ([]ContentPart, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil, nil
	}
	if strings.HasPrefix(s, "\"") {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, err
		}
		return []ContentPart{{Type: "text", Text: text}}, nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, err
	}
	return parts, nil
}

// imageURL extracts the URL from an image_url value ({"url": ...} or "...").
func imageURL(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, "\"") {
		var u string
		_ = json.Unmarshal(raw, &u)
		return u
	}
	var obj struct {
		URL string `json:"url"`
	}
	_ = json.Unmarshal(raw, &obj)
	return obj.URL
}

// textOnly joins the text parts of a content value.
func textOnly(raw json.RawMessage) string {
	parts, err := contentParts(raw)
	if err != nil {
		return strings.TrimSpace(string(raw))
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}
