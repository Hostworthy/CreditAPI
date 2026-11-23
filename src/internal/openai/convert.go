package openai

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/atb00ker/CreditAPI/src/internal/agent"
)

// requestError is a client error (HTTP 400).
type requestError struct{ msg string }

func (e *requestError) Error() string { return e.msg }

func badRequest(format string, a ...any) error {
	return &requestError{msg: fmt.Sprintf(format, a...)}
}

var invalidToolChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// claudeToolName makes a function name safe for Claude's tool naming rules
// (including the mcp__oai__ prefix staying within 64 characters).
func claudeToolName(name string) string {
	s := invalidToolChars.ReplaceAllString(name, "_")
	const maxLen = 64 - len(agent.BridgeToolPrefix)
	if len(s) > maxLen {
		sum := sha256.Sum256([]byte(name))
		s = s[:maxLen-9] + "_" + hex.EncodeToString(sum[:])[:8]
	}
	return s
}

type toolChoice struct {
	mode string // auto, none, required, function
	name string
}

func parseToolChoice(raw json.RawMessage, legacy json.RawMessage) toolChoice {
	parse := func(r json.RawMessage) (toolChoice, bool) {
		s := strings.TrimSpace(string(r))
		if s == "" || s == "null" {
			return toolChoice{}, false
		}
		var str string
		if json.Unmarshal(r, &str) == nil {
			switch str {
			case "none", "required", "auto":
				return toolChoice{mode: str}, true
			case "any":
				return toolChoice{mode: "required"}, true
			}
			return toolChoice{mode: "auto"}, true
		}
		var obj struct {
			Type     string `json:"type"`
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if json.Unmarshal(r, &obj) == nil {
			name := obj.Function.Name
			if name == "" {
				name = obj.Name
			}
			if name != "" {
				return toolChoice{mode: "function", name: name}, true
			}
		}
		return toolChoice{mode: "auto"}, true
	}
	if tc, ok := parse(raw); ok {
		return tc
	}
	if tc, ok := parse(legacy); ok {
		return tc
	}
	return toolChoice{mode: "auto"}
}

// buildTools converts OpenAI tools into bridged MCP tools.
func buildTools(req *ChatRequest, tc toolChoice) ([]agent.Tool, map[string]string, error) {
	defs := make([]FunctionDef, 0, len(req.Tools)+len(req.Functions))
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			continue // e.g. built-in OpenAI tools we cannot provide
		}
		defs = append(defs, t.Function)
	}
	defs = append(defs, req.Functions...)
	if tc.mode == "none" || len(defs) == 0 {
		return nil, nil, nil
	}

	var tools []agent.Tool
	names := map[string]string{}
	for _, d := range defs {
		if d.Name == "" {
			return nil, nil, badRequest("every tool needs a function name")
		}
		if tc.mode == "function" && d.Name != tc.name {
			continue
		}
		schema, err := normalizeSchema(d.Parameters)
		if err != nil {
			return nil, nil, badRequest("tool %q has invalid parameters: %v", d.Name, err)
		}
		cn := claudeToolName(d.Name)
		if prev, dup := names[cn]; dup && prev != d.Name {
			return nil, nil, badRequest("tool names %q and %q collide after sanitizing", prev, d.Name)
		}
		names[cn] = d.Name
		tools = append(tools, agent.Tool{Name: cn, Description: d.Description, InputSchema: schema})
	}
	if tc.mode == "function" && len(tools) == 0 {
		return nil, nil, badRequest("tool_choice names function %q, which is not in tools", tc.name)
	}
	return tools, names, nil
}

// normalizeSchema makes sure an input schema is a JSON object schema.
func normalizeSchema(raw json.RawMessage) (json.RawMessage, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s == "{}" {
		return json.RawMessage(`{"type":"object","properties":{}}`), nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if _, ok := m["type"]; !ok {
		m["type"] = "object"
	}
	if m["type"] == "object" {
		if _, ok := m["properties"]; !ok {
			m["properties"] = map[string]any{}
		}
	}
	b, err := json.Marshal(m)
	return b, err
}

// conversion holds the outcome of translating a chat request.
type conversion struct {
	system     string
	content    []agent.ContentBlock
	images     int
	transcript bool
}

// contentBuilder accumulates text and image blocks, merging adjacent text.
type contentBuilder struct {
	blocks []agent.ContentBlock
	text   strings.Builder
}

func (b *contentBuilder) writeText(s string) { b.text.WriteString(s) }

func (b *contentBuilder) addImage(img agent.ContentBlock) {
	b.flush()
	b.blocks = append(b.blocks, img)
}

func (b *contentBuilder) flush() {
	if b.text.Len() > 0 {
		b.blocks = append(b.blocks, agent.ContentBlock{Type: "text", Text: b.text.String()})
		b.text.Reset()
	}
}

func (b *contentBuilder) done() []agent.ContentBlock {
	b.flush()
	return b.blocks
}

// convertMessages turns the OpenAI message list into a system prompt and the
// content of a single Claude user message. A single user turn is passed
// through as-is; longer conversations are rendered as a transcript.
func (h *Handler) convertMessages(ctx context.Context, msgs []Message, hasTools bool) (*conversion, error) {
	var system []string
	var conv []Message
	for _, m := range msgs {
		switch m.Role {
		case "system", "developer":
			if t := strings.TrimSpace(textOnly(m.Content)); t != "" {
				system = append(system, t)
			}
		case "user", "assistant", "tool", "function":
			conv = append(conv, m)
		default:
			return nil, badRequest("unsupported message role %q", m.Role)
		}
	}
	if len(conv) == 0 {
		return nil, badRequest("messages must contain at least one user message")
	}

	// Count images so that only the most recent MaxImages are sent.
	totalImages := 0
	for _, m := range conv {
		if m.Role != "user" {
			continue
		}
		parts, _ := contentParts(m.Content)
		for _, p := range parts {
			if p.Type == "image_url" || p.Type == "image" {
				totalImages++
			}
		}
	}
	skip := 0
	if h.cfg.MaxImages > 0 && totalImages > h.cfg.MaxImages {
		skip = totalImages - h.cfg.MaxImages
	}

	out := &conversion{system: strings.Join(system, "\n\n")}
	cb := &contentBuilder{}
	seen := 0

	writeUserParts := func(m Message) error {
		parts, err := contentParts(m.Content)
		if err != nil {
			return badRequest("invalid user message content: %v", err)
		}
		for i, p := range parts {
			switch p.Type {
			case "text", "input_text", "":
				cb.writeText(p.Text)
			case "image_url", "image":
				seen++
				if seen <= skip {
					cb.writeText("[earlier image omitted]")
					continue
				}
				ref := imageURL(p.ImageURL)
				if ref == "" {
					return badRequest("image part %d has no url", i)
				}
				img, err := h.images.Load(ctx, ref)
				if err != nil {
					return badRequest("image %d: %v", seen, err)
				}
				cb.addImage(img)
				out.images++
			case "input_audio", "audio":
				return badRequest("audio input is not supported")
			case "file":
				return badRequest("file content parts are not supported yet; send images or text")
			default:
				return badRequest("unsupported content part type %q", p.Type)
			}
		}
		return nil
	}

	if len(conv) == 1 && conv[0].Role == "user" {
		if err := writeUserParts(conv[0]); err != nil {
			return nil, err
		}
		out.content = cb.done()
		if len(out.content) == 0 {
			return nil, badRequest("the user message is empty")
		}
		return out, nil
	}

	// Transcript mode. Remember tool names so results can be labelled.
	out.transcript = true
	toolNames := map[string]string{}
	for _, m := range conv {
		for _, tc := range m.ToolCalls {
			toolNames[tc.ID] = tc.Function.Name
		}
	}

	cb.writeText("The conversation so far is shown below, oldest first. Earlier tool calls and their results appear as <tool_call> and <tool_result> elements for context only.\n\n")
	for _, m := range conv {
		switch m.Role {
		case "user":
			if m.Name != "" {
				cb.writeText(fmt.Sprintf("<user name=%q>\n", m.Name))
			} else {
				cb.writeText("<user>\n")
			}
			if err := writeUserParts(m); err != nil {
				return nil, err
			}
			cb.writeText("\n</user>\n\n")
		case "assistant":
			cb.writeText("<assistant>\n")
			if t := textOnly(m.Content); t != "" {
				cb.writeText(t + "\n")
			}
			calls := m.ToolCalls
			if m.FunctionCall != nil {
				calls = append(calls, ToolCallMsg{Function: *m.FunctionCall})
			}
			for _, tc := range calls {
				args := strings.TrimSpace(tc.Function.Arguments)
				if args == "" {
					args = "{}"
				}
				if tc.ID != "" {
					cb.writeText(fmt.Sprintf("<tool_call id=%q name=%q>%s</tool_call>\n", tc.ID, tc.Function.Name, args))
				} else {
					cb.writeText(fmt.Sprintf("<tool_call name=%q>%s</tool_call>\n", tc.Function.Name, args))
				}
			}
			cb.writeText("</assistant>\n\n")
		case "tool", "function":
			name := m.Name
			if name == "" {
				name = toolNames[m.ToolCallID]
			}
			attrs := ""
			if m.ToolCallID != "" {
				attrs += fmt.Sprintf(" tool_call_id=%q", m.ToolCallID)
			}
			if name != "" {
				attrs += fmt.Sprintf(" name=%q", name)
			}
			cb.writeText(fmt.Sprintf("<tool_result%s>\n%s\n</tool_result>\n\n", attrs, textOnly(m.Content)))
		}
	}

	switch conv[len(conv)-1].Role {
	case "user":
		cb.writeText("Write the assistant's reply to the last user message.")
	case "tool", "function":
		if hasTools {
			cb.writeText("The tool results above answer the assistant's latest tool calls. Continue as the assistant: use the results, call more tools if needed, or reply to the user.")
		} else {
			cb.writeText("The tool results above answer the assistant's latest tool calls. Continue as the assistant and reply to the user.")
		}
	case "assistant":
		cb.writeText("Continue the assistant's last message.")
	}
	out.content = cb.done()
	return out, nil
}

// systemPrompt assembles the final system prompt.
func (h *Handler) systemPrompt(base string, tools []agent.Tool, tc toolChoice, jsonObject bool) string {
	if strings.TrimSpace(base) == "" {
		base = h.cfg.DefaultSystemPrompt
	}
	var b strings.Builder
	b.WriteString(base)
	if len(tools) > 0 {
		b.WriteString("\n\nYou can call the functions provided by the API client as tools. When a function would help, call it through the tool-use interface; never write tool calls as text or XML in your reply. The client runs the functions and sends the results back to you.")
		switch tc.mode {
		case "required":
			b.WriteString(" You must call at least one of the tools now.")
		case "function":
			b.WriteString(fmt.Sprintf(" You must call the %q tool now.", tc.name))
		}
	}
	if jsonObject {
		b.WriteString("\n\nRespond with a single valid JSON object only: no markdown code fences and no text before or after it.")
	}
	return b.String()
}

// stopSequences parses the "stop" field (string or array).
func stopSequences(raw json.RawMessage) []string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(raw, &many)
	out := many[:0]
	for _, m := range many {
		if m != "" {
			out = append(out, m)
		}
	}
	return out
}

// effortFor maps OpenAI reasoning_effort to a claude --effort level.
func effortFor(requested, def string) string {
	switch strings.ToLower(strings.TrimSpace(requested)) {
	case "minimal", "none", "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh":
		return "xhigh"
	case "max":
		return "max"
	}
	return def
}

// jsonSchemaFor returns the --json-schema argument for a response_format.
// maxSchemaBytes keeps a schema well under the operating system's limit for
// one command-line argument (it is passed to claude as --json-schema).
const maxSchemaBytes = 64 << 10

func jsonSchemaFor(rf *ResponseFormat) (json.RawMessage, error) {
	if rf == nil {
		return nil, nil
	}
	switch rf.Type {
	case "", "text":
		return nil, nil
	case "json_object":
		// Handled with an instruction plus server-side JSON extraction: a
		// schema without properties lets Claude return an empty object.
		return nil, nil
	case "json_schema":
		if rf.JSONSchema == nil || len(rf.JSONSchema.Schema) == 0 {
			return nil, badRequest("response_format.json_schema.schema is required")
		}
		var v any
		if err := json.Unmarshal(rf.JSONSchema.Schema, &v); err != nil {
			return nil, badRequest("response_format.json_schema.schema is not valid JSON: %v", err)
		}
		b, _ := json.Marshal(v) // compact
		if len(b) > maxSchemaBytes {
			return nil, badRequest("response_format.json_schema.schema is larger than %d KB", maxSchemaBytes>>10)
		}
		return b, nil
	}
	return nil, badRequest("unsupported response_format type %q", rf.Type)
}
