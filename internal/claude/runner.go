package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ContentBlock is an Anthropic Messages API content block (text or image).
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
	// Tools are client-side functions exposed through the MCP bridge.
	Tools []Tool
	// ToolNames maps the name Claude sees back to the client's original name.
	ToolNames  map[string]string
	JSONSchema json.RawMessage
	// JSONObject asks for a JSON object without a schema (OpenAI json_object
	// mode). The reply is buffered and cleaned up (fences removed).
	JSONObject      bool
	MaxOutputTokens int
	// Probe marks a login check: it always runs claude, even when the login
	// is already known to be broken.
	Probe bool
}

// ToolCall is a function call requested by Claude for the client to execute.
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
	Kind    string // auth, rate_limit, overloaded, invalid_request, busy, upstream
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// Runner executes `claude -p` processes.
type Runner struct {
	Bin     string
	WorkDir string
	Bridge  *Bridge
	Env     func() []string
	Status  *Status
	// Tokens (optional) lets the runner tell when the login has changed.
	Tokens *TokenStore

	sem chan struct{}
}

// structuredTurns allows the StructuredOutput tool call plus one retry.
const structuredTurns = 3

// failFastWindow is how long a "login required" result is reused for new
// requests (with the same token) before claude is tried again.
const failFastWindow = 5 * time.Minute

func (r *Runner) tokenVersion() int64 {
	if r.Tokens == nil {
		return 0
	}
	return r.Tokens.Version()
}

// NewRunner creates a runner allowing maxConcurrent simultaneous processes.
func NewRunner(maxConcurrent int) *Runner {
	return &Runner{sem: make(chan struct{}, maxConcurrent)}
}

// Acquire reserves a process slot, waiting up to timeout.
func (r *Runner) Acquire(ctx context.Context, timeout time.Duration) (func(), error) {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case r.sem <- struct{}{}:
		r.Status.active(1)
		var once sync.Once
		return func() {
			once.Do(func() {
				<-r.sem
				r.Status.active(-1)
			})
		}, nil
	case <-t.C:
		return nil, &Error{Kind: "busy", Status: 429, Message: "server is busy: all Claude slots are in use, try again shortly (raise MAX_CONCURRENT to allow more)"}
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// --- stream-json wire types -------------------------------------------------

type streamLine struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype"`
	Event   json.RawMessage `json:"event"`
	// result
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StopReason       string          `json:"stop_reason"`
	APIErrorStatus   *int            `json:"api_error_status"`
	Usage            *Usage          `json:"usage"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	Errors           []string        `json:"errors"`
	// system init
	MCPServers []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"mcp_servers"`
	// rate_limit_event
	RateLimitInfo json.RawMessage `json:"rate_limit_info"`
	// assistant
	Message *struct {
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	Error string `json:"error"`
}

type streamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Model string `json:"model"`
		Usage *Usage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *Usage `json:"usage"`
}

type block struct {
	kind   string
	id     string
	name   string
	client bool
	index  int
	args   strings.Builder
}

// Run executes one completion and blocks until it finishes.
func (r *Runner) Run(ctx context.Context, opts RunOptions, cb Callbacks) (*Result, error) {
	version := r.tokenVersion()
	if !opts.Probe {
		if reason, ok := r.Status.loginKnownBroken(version, failFastWindow); ok {
			return nil, &Error{Kind: "auth", Status: 401, Message: reason}
		}
	}
	res, err := r.run(ctx, opts, cb)
	source := "request"
	if opts.Probe {
		source = "check"
	}
	var ce *Error
	switch {
	case err == nil:
		r.Status.recordAuth(true, "", version, source)
	case errors.As(err, &ce) && ce.Kind == "auth":
		r.Status.recordAuth(false, ce.Message, version, source)
	}
	return res, err
}

func (r *Runner) run(ctx context.Context, opts RunOptions, cb Callbacks) (*Result, error) {
	tmp, err := os.MkdirTemp("", "cmta-run-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	sysFile := filepath.Join(tmp, "system.txt")
	if err := os.WriteFile(sysFile, []byte(opts.SystemPrompt), 0o600); err != nil {
		return nil, err
	}

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--model", opts.Model,
		"--system-prompt-file", sysFile,
		"--tools", "",
		"--setting-sources", "",
		"--strict-mcp-config",
		"--no-session-persistence",
	}
	if opts.Effort != "" {
		args = append(args, "--effort", opts.Effort)
	}

	// Client tools are offered through a per-request loopback MCP endpoint.
	if len(opts.Tools) > 0 && r.Bridge != nil {
		url, unregister := r.Bridge.Register(opts.Tools)
		defer unregister()
		b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
			BridgeServerName: map[string]any{"type": "http", "url": url},
		}})
		mcpFile := filepath.Join(tmp, "mcp.json")
		if err := os.WriteFile(mcpFile, b, 0o600); err != nil {
			return nil, err
		}
		args = append(args, "--mcp-config", mcpFile, "--allowedTools", "mcp__"+BridgeServerName)
	}

	maxTurns := 1
	if len(opts.JSONSchema) > 0 {
		maxTurns = structuredTurns
	}
	args = append(args, "--max-turns", strconv.Itoa(maxTurns))
	if len(opts.JSONSchema) > 0 {
		args = append(args, "--json-schema", string(opts.JSONSchema))
	}

	env := r.Env()
	if opts.MaxOutputTokens > 0 {
		env = append(env, "CLAUDE_CODE_MAX_OUTPUT_TOKENS="+strconv.Itoa(opts.MaxOutputTokens))
	}

	input, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": opts.Content},
	})
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(r.Bin, args...)
	cmd.Dir = r.WorkDir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdin = bytes.NewReader(append(input, '\n'))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr := &tailBuffer{max: 8 << 10}
	cmd.Stderr = stderr

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, &Error{Kind: "upstream", Status: 500, Message: fmt.Sprintf("cannot start %s: %v", r.Bin, err)}
	}
	var killOnce sync.Once
	kill := func() {
		killOnce.Do(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	}
	stopWatch := context.AfterFunc(ctx, kill)
	defer stopWatch()

	res, perr := r.parse(stdout, opts, cb, kill)
	if perr != nil || (res != nil && res.FinishReason == "tool_calls") {
		kill()
	}
	// Drain anything left so the process is not blocked on a full pipe.
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if perr != nil {
		r.Status.recordError(perr.Error())
		return nil, perr
	}
	if res == nil || (res.FinishReason == "" && res.Text == "" && len(res.ToolCalls) == 0 && res.StructuredOutput == nil) {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" && waitErr != nil {
			msg = waitErr.Error()
		}
		e := classify(0, "claude exited without a result: "+msg)
		r.Status.recordError(e.Message)
		return nil, e
	}
	if res.FinishReason == "" {
		res.FinishReason = "stop"
	}
	r.Status.recordSuccess()
	slog.Debug("claude run finished", "model", res.Model, "finish", res.FinishReason,
		"duration", time.Since(start).Round(time.Millisecond), "in", res.Usage.PromptTokens(), "out", res.Usage.OutputTokens)
	return res, nil
}

func (r *Runner) parse(stdout io.Reader, opts RunOptions, cb Callbacks, kill func()) (*Result, error) {
	res := &Result{}
	var (
		text         strings.Builder
		blocks       = map[int]*block{}
		pending      []ToolCall
		stopReason   string
		msgUsage     Usage
		sumUsage     Usage
		sawStream    bool
		needSep      bool
		msgHasText   bool
		suppressText = len(opts.JSONSchema) > 0 || opts.JSONObject
	)
	emitText := func(s string) {
		text.WriteString(s)
		if cb.Text != nil && !suppressText {
			cb.Text(s)
		}
	}

	rd := bufio.NewReaderSize(stdout, 256<<10)
	for {
		line, err := rd.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var sl streamLine
			if jerr := json.Unmarshal(line, &sl); jerr != nil {
				slog.Debug("claude: non-JSON output line", "line", truncate(string(line), 200))
			} else {
				switch sl.Type {
				case "system":
					if sl.Subtype == "init" && len(opts.Tools) > 0 {
						for _, s := range sl.MCPServers {
							if s.Name == BridgeServerName && s.Status != "connected" {
								slog.Warn("client tool bridge did not connect", "status", s.Status)
							}
						}
					}
				case "rate_limit_event":
					r.Status.recordRateLimit(sl.RateLimitInfo)
				case "stream_event":
					sawStream = true
					var ev streamEvent
					if json.Unmarshal(sl.Event, &ev) != nil {
						break
					}
					switch ev.Type {
					case "message_start":
						blocks = map[int]*block{}
						pending = nil
						stopReason = ""
						msgUsage = Usage{}
						msgHasText = false
						if text.Len() > 0 {
							needSep = true
						}
						if ev.Message != nil {
							res.Model = ev.Message.Model
							if ev.Message.Usage != nil {
								msgUsage = *ev.Message.Usage
							}
						}
					case "content_block_start":
						if ev.ContentBlock == nil {
							break
						}
						b := &block{kind: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
						blocks[ev.Index] = b
						if b.kind == "tool_use" && strings.HasPrefix(b.name, BridgeToolPrefix) {
							b.client = true
							short := strings.TrimPrefix(b.name, BridgeToolPrefix)
							if orig, ok := opts.ToolNames[short]; ok {
								b.name = orig
							} else {
								b.name = short
							}
							b.index = len(pending)
							b.id = "call_" + strings.TrimPrefix(b.id, "toolu_")
							if cb.ToolStart != nil {
								cb.ToolStart(b.index, b.id, b.name)
							}
						}
					case "content_block_delta":
						if ev.Delta == nil {
							break
						}
						b := blocks[ev.Index]
						switch ev.Delta.Type {
						case "text_delta":
							if ev.Delta.Text == "" {
								break
							}
							if needSep && !msgHasText {
								emitText("\n\n")
							}
							needSep = false
							msgHasText = true
							emitText(ev.Delta.Text)
						case "input_json_delta":
							if b != nil && b.client {
								b.args.WriteString(ev.Delta.PartialJSON)
								if cb.ToolArgs != nil && ev.Delta.PartialJSON != "" {
									cb.ToolArgs(b.index, ev.Delta.PartialJSON)
								}
							}
						}
					case "content_block_stop":
						if b := blocks[ev.Index]; b != nil && b.client {
							args := strings.TrimSpace(b.args.String())
							if args == "" {
								args = "{}"
							}
							pending = append(pending, ToolCall{ID: b.id, Name: b.name, Arguments: args})
						}
					case "message_delta":
						if ev.Delta != nil && ev.Delta.StopReason != "" {
							stopReason = ev.Delta.StopReason
						}
						if ev.Usage != nil {
							msgUsage.OutputTokens = ev.Usage.OutputTokens
							if ev.Usage.InputTokens > 0 {
								msgUsage.InputTokens = ev.Usage.InputTokens
								msgUsage.CacheReadTokens = ev.Usage.CacheReadTokens
								msgUsage.CacheCreationTokens = ev.Usage.CacheCreationTokens
							}
						}
					case "message_stop":
						sumUsage.InputTokens += msgUsage.InputTokens
						sumUsage.OutputTokens += msgUsage.OutputTokens
						sumUsage.CacheReadTokens += msgUsage.CacheReadTokens
						sumUsage.CacheCreationTokens += msgUsage.CacheCreationTokens
						if len(pending) > 0 && stopReason == "tool_use" {
							// Claude wants the client to run tools: hand them back
							// and stop the process instead of executing anything.
							res.ToolCalls = pending
							res.FinishReason = "tool_calls"
							res.Text = text.String()
							res.Usage = sumUsage
							kill()
							return res, nil
						}
					}
				case "assistant":
					if sl.Error != "" {
						slog.Debug("claude assistant error", "error", sl.Error)
					}
				case "result":
					if sl.Usage != nil {
						res.Usage = *sl.Usage
					} else {
						res.Usage = sumUsage
					}
					res.CostUSD = sl.TotalCostUSD
					if sl.IsError || strings.HasPrefix(sl.Subtype, "error") {
						status := 0
						if sl.APIErrorStatus != nil {
							status = *sl.APIErrorStatus
						}
						msg := sl.Result
						if msg == "" && len(sl.Errors) > 0 {
							msg = strings.Join(sl.Errors, "; ")
						}
						if msg == "" {
							msg = "claude reported an error (" + sl.Subtype + ")"
						}
						if sl.Subtype == "error_max_turns" && text.Len() > 0 && len(opts.JSONSchema) == 0 {
							res.Text = text.String()
							res.FinishReason = "length"
							return res, nil
						}
						return nil, classify(status, msg)
					}
					res.Text = text.String()
					if !sawStream && res.Text == "" {
						res.Text = sl.Result
						if cb.Text != nil && !suppressText && res.Text != "" {
							cb.Text(res.Text)
						}
					}
					if opts.JSONObject {
						res.StructuredOutput = ExtractJSON(res.Text)
					}
					if len(sl.StructuredOutput) > 0 && string(sl.StructuredOutput) != "null" {
						res.StructuredOutput = sl.StructuredOutput
					} else if len(opts.JSONSchema) > 0 {
						// Fall back to parsing the text result as JSON.
						if j := ExtractJSON(sl.Result); j != nil {
							res.StructuredOutput = j
						} else if j := ExtractJSON(res.Text); j != nil {
							res.StructuredOutput = j
						} else {
							return nil, &Error{Kind: "upstream", Status: 502, Message: "Claude did not return the requested structured JSON output"}
						}
					}
					res.FinishReason = "stop"
					if sl.StopReason == "max_tokens" {
						res.FinishReason = "length"
					}
					return res, nil
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
	}
	// Stream ended without a result line.
	if text.Len() > 0 {
		res.Text = text.String()
		res.Usage = sumUsage
		res.FinishReason = "stop"
		return res, nil
	}
	return nil, nil
}

func classify(status int, msg string) *Error {
	lower := strings.ToLower(msg)
	switch {
	case status == 401 || status == 403 || strings.Contains(lower, "/login") ||
		strings.Contains(lower, "failed to authenticate") || strings.Contains(lower, "not logged in") ||
		strings.Contains(lower, "oauth token") || strings.Contains(lower, "invalid api key"):
		return &Error{Kind: "auth", Status: 401, Message: "Claude login required (" + truncate(strings.TrimSpace(msg), 300) + ")"}
	case status == 429 || strings.Contains(lower, "rate limit") || strings.Contains(lower, "usage limit"):
		return &Error{Kind: "rate_limit", Status: 429, Message: truncate(msg, 500)}
	case status == 529 || strings.Contains(lower, "overloaded"):
		return &Error{Kind: "overloaded", Status: 503, Message: truncate(msg, 500)}
	case status == 400 || status == 413:
		return &Error{Kind: "invalid_request", Status: 400, Message: truncate(msg, 500)}
	}
	return &Error{Kind: "upstream", Status: 502, Message: truncate(msg, 500)}
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
