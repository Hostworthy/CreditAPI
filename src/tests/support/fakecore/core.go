// Package fakecore holds what the fake Claude and Cursor CLIs share:
// [fake:...] directives, conversation parsing, image size reports, sample
// values generated from JSON schemas and a client for the MCP bridge.
package fakecore

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif" // register decoders for image size reports
	_ "image/jpeg"
	_ "image/png"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	_ "golang.org/x/image/webp"
)

var directiveRe = regexp.MustCompile(`\[fake:([a-z-]+)\]`)

// Directives returns the [fake:...] directives found in text.
//
//	[fake:crash]        exit with an error and no result
//	[fake:rate-limit]   usage limit error
//	[fake:overloaded]   overloaded error
//	[fake:bad-request]  invalid request error
//	[fake:slow]         "one two three STOP four five", one word every 150 ms
//	[fake:echo-system]  reply with the system prompt (instructions)
//	[fake:echo-input]   reply with the full user input
//	[fake:long]         a reply of about 4000 characters
//	[fake:whoami]       reply with the provider and subscription that answered
//	[fake:not-json]     ignore a JSON-object instruction and reply in prose
func Directives(text string) map[string]bool {
	out := map[string]bool{}
	for _, m := range directiveRe.FindAllStringSubmatch(text, -1) {
		out[m[1]] = true
	}
	return out
}

// SlowReply and LongReply are the canned replies for [fake:slow] and
// [fake:long].
const SlowReply = "one two three STOP four five"

// LongReply returns about 4000 characters of text.
func LongReply() string {
	return strings.Repeat("All work and no play makes a long fake reply. ", 90)
}

// Whoami describes the provider and subscription serving a request. The
// subscription ID is the name of the directory holding the CLI's HOME.
func Whoami(provider string) string {
	home := os.Getenv("HOME")
	return fmt.Sprintf("Served by %s subscription %s", provider, filepath.Base(filepath.Dir(home)))
}

// Chunk splits a reply into a few streaming deltas.
func Chunk(s string) []string {
	if s == "" {
		return []string{""}
	}
	words := strings.SplitAfter(s, " ")
	var parts []string
	for i := 0; i < len(words); i += 4 {
		end := min(i+4, len(words))
		parts = append(parts, strings.Join(words[i:end], ""))
	}
	return parts
}

// LastUserLine returns the first line of the last user turn, whether the
// input is a single message or a rendered transcript.
func LastUserLine(text string) string {
	if i := strings.LastIndex(text, "<user"); i >= 0 {
		rest := text[i:]
		if j := strings.Index(rest, ">"); j >= 0 {
			rest = rest[j+1:]
		}
		if k := strings.Index(rest, "</user>"); k >= 0 {
			rest = rest[:k]
		}
		text = rest
	}
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "[image:") {
			if len(l) > 120 {
				l = l[:120]
			}
			return l
		}
	}
	return "(empty message)"
}

// ToolResults counts complete <tool_result> blocks.
func ToolResults(text string) int { return strings.Count(text, "</tool_result>") }

// EndsWithToolResults reports whether the conversation ends with tool
// results (no user turn after the last one).
func EndsWithToolResults(text string) bool {
	last := strings.LastIndex(text, "</tool_result>")
	return last >= 0 && !strings.Contains(text[last:], "<user")
}

// FirstToolResult returns the body of the first complete <tool_result> block
// (the transcript's introduction mentions the tag name, so anchor on the
// closing tag and search backwards for its opening tag).
func FirstToolResult(text string) string {
	end := strings.Index(text, "</tool_result>")
	if end < 0 {
		return ""
	}
	start := strings.LastIndex(text[:end], "<tool_result")
	if start < 0 {
		return ""
	}
	rest := text[start:end]
	if j := strings.Index(rest, ">"); j >= 0 {
		rest = rest[j+1:]
	}
	rest = strings.TrimSpace(rest)
	if len(rest) > 200 {
		rest = rest[:200]
	}
	return rest
}

// ImageSize returns "WxH" for image bytes, or "unreadable".
func ImageSize(raw []byte) string {
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(raw)); err == nil {
		return fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)
	}
	return "unreadable"
}

// DescribeImages formats "I see N image(s): image/png 8x8, ...".
func DescribeImages(items []string) string {
	if len(items) == 0 {
		return ""
	}
	return fmt.Sprintf("I see %d image(s): %s.", len(items), strings.Join(items, ", "))
}

// --- MCP bridge client ---------------------------------------------------------

// Tool is a function listed by the MCP bridge.
type Tool struct {
	Name        string         `json:"name"`
	InputSchema map[string]any `json:"inputSchema"`
}

// RPC sends one JSON-RPC request to an MCP endpoint and returns its result.
func RPC(url, method string, params any) json.RawMessage {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out struct {
		Result json.RawMessage `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Result
}

// ListTools initializes an MCP session and lists its tools.
func ListTools(url string) []Tool {
	RPC(url, "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "fake"}})
	var tl struct {
		Tools []Tool `json:"tools"`
	}
	_ = json.Unmarshal(RPC(url, "tools/list", map[string]any{}), &tl)
	return tl.Tools
}

// ChooseTools picks the tools whose names appear in text, or the first one.
func ChooseTools(tools []Tool, text string) []Tool {
	var chosen []Tool
	for _, t := range tools {
		if strings.Contains(text, t.Name) {
			chosen = append(chosen, t)
		}
	}
	if len(chosen) == 0 && len(tools) > 0 {
		chosen = tools[:1]
	}
	return chosen
}

// RandomHex returns n random bytes as hex.
func RandomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Truncate shortens s to n bytes.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
