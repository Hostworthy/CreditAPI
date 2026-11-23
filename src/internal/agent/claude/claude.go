// Package claude is the Claude Code backend: it answers completions with
// `claude -p` (stream-json) and signs in with `claude setup-token`.
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
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/atb00ker/CreditAPI/src/internal/agent"
	"github.com/atb00ker/CreditAPI/src/internal/logging"
)

// Provider is the identifier stored with Claude subscriptions.
const Provider = "claude"

// TokenLifetime is how long `claude setup-token` tokens are valid.
const TokenLifetime = 365 * 24 * time.Hour

// structuredTurns allows the StructuredOutput tool call plus one retry.
const structuredTurns = 3

// Backend runs the Claude Code CLI.
type Backend struct {
	Bin     string
	WorkDir string
	Bridge  *agent.Bridge
	Default string
	List    []string
}

var _ agent.Backend = (*Backend)(nil)

// Provider implements agent.Backend.
func (b *Backend) Provider() string { return Provider }

// Label implements agent.Backend.
func (b *Backend) Label() string { return "Claude" }

// CredentialEnv implements agent.Backend.
func (b *Backend) CredentialEnv() string { return "CLAUDE_CODE_OAUTH_TOKEN" }

// Models implements agent.Backend.
func (b *Backend) Models() []string { return b.List }

// DefaultModel implements agent.Backend.
func (b *Backend) DefaultModel() string { return b.Default }

// Accepts implements agent.Backend: Claude aliases, full claude-... names
// and the configured model list.
func (b *Backend) Accepts(model string) bool {
	for _, m := range b.List {
		if m == model {
			return true
		}
	}
	switch strings.ToLower(model) {
	case "opus", "sonnet", "haiku", "fable", "default", "opusplan":
		return true
	}
	return strings.HasPrefix(model, "claude-") || strings.Contains(model, "[1m]")
}

// Version implements agent.Backend.
func (b *Backend) Version(ctx context.Context, env []string) string {
	return agent.CLIVersion(ctx, b.Bin, env)
}

// ValidateCredential implements agent.Backend.
func (b *Backend) ValidateCredential(token string) error {
	if !strings.HasPrefix(token, "sk-ant-") || len(token) < 40 {
		return errors.New("that does not look like a Claude OAuth token (expected sk-ant-oat01-...)")
	}
	return nil
}

// Logout implements agent.Backend: Claude credentials are only the saved
// token, which the caller removes.
func (b *Backend) Logout(context.Context, *agent.Account) error { return nil }

var (
	tokenRe = regexp.MustCompile(`sk-ant-oat\d{2}-[A-Za-z0-9_\-]{20,}`)
	// Looser fallback in case the token format changes.
	tokenLooseRe = regexp.MustCompile(`sk-ant-[a-z]{2,8}\d{0,3}-[A-Za-z0-9_\-]{40,}`)
)

// ExtractToken finds a long-lived OAuth token in `claude setup-token` output.
func ExtractToken(raw string) string {
	plain := agent.StripANSI(raw)
	if t := tokenRe.FindString(plain); t != "" {
		return t
	}
	// The PTY is very wide, so the token is never wrapped.
	return tokenLooseRe.FindString(plain)
}

// Login implements agent.Backend: `claude setup-token` prints a sign-in URL,
// waits for the code shown after approving and prints a one-year token.
func (b *Backend) Login(acc *agent.Account) agent.LoginSpec {
	env := append(acc.BaseEnv(), "TERM=xterm-256color", "BROWSER=/bin/false", "COLUMNS=1000", "LINES=50")
	return agent.LoginSpec{
		Bin:       b.Bin,
		Args:      []string{"setup-token"},
		Env:       env,
		NeedsCode: true,
		Token:     ExtractToken,
		Redact:    func(s string) string { return tokenRe.ReplaceAllStringFunc(s, agent.Hint) },
		Save:      acc.Tokens.Save,
		URLHosts:  []string{"claude.ai", "claude.com", "anthropic.com"},
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
	Usage            *agent.Usage    `json:"usage"`
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
	Error         string          `json:"error"`
}

type streamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Model string       `json:"model"`
		Usage *agent.Usage `json:"usage"`
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
	Usage *agent.Usage `json:"usage"`
}

type block struct {
	kind   string
	id     string
	name   string
	client bool
	index  int
	args   strings.Builder
}

// Run implements agent.Backend.
func (b *Backend) Run(ctx context.Context, acc *agent.Account, opts agent.RunOptions, cb agent.Callbacks) (*agent.Result, error) {
	root, err := agent.RunRoot(Provider)
	if err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(root, "run-")
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
	if len(opts.Tools) > 0 && b.Bridge != nil {
		reg := b.Bridge.Register(opts.Tools)
		defer reg.Close()
		cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
			agent.BridgeServerName: map[string]any{"type": "http", "url": reg.URL},
		}})
		mcpFile := filepath.Join(tmp, "mcp.json")
		if err := os.WriteFile(mcpFile, cfg, 0o600); err != nil {
			return nil, err
		}
		args = append(args, "--mcp-config", mcpFile, "--allowedTools", "mcp__"+agent.BridgeServerName)
	}

	maxTurns := 1
	if len(opts.JSONSchema) > 0 {
		maxTurns = structuredTurns
	}
	args = append(args, "--max-turns", strconv.Itoa(maxTurns))
	if len(opts.JSONSchema) > 0 {
		args = append(args, "--json-schema", string(opts.JSONSchema))
	}

	env := acc.Env(b)
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

	start := time.Now()
	proc, err := agent.StartProc(ctx, b.Bin, args, env, b.WorkDir, bytes.NewReader(append(input, '\n')))
	if err != nil {
		return nil, &agent.Error{Kind: "upstream", Status: 502, Message: fmt.Sprintf("cannot start %s: %v", b.Bin, err)}
	}
	res, perr := b.parse(proc.Stdout, acc, opts, cb, proc.Kill)
	if perr != nil || (res != nil && res.FinishReason == "tool_calls") {
		proc.Kill()
	}
	waitErr := proc.Wait()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if perr != nil {
		return nil, perr
	}
	if res == nil || (res.FinishReason == "" && res.Text == "" && len(res.ToolCalls) == 0 && res.StructuredOutput == nil) {
		msg := strings.TrimSpace(proc.Stderr.String())
		if msg == "" && waitErr != nil {
			msg = waitErr.Error()
		}
		return nil, agent.Classify(b.Label(), 0, "claude exited without a result: "+msg)
	}
	if res.FinishReason == "" {
		if waitErr != nil {
			// The process died mid-answer: do not pass a cut-off reply off as
			// complete.
			msg := strings.TrimSpace(proc.Stderr.String())
			if msg == "" {
				msg = waitErr.Error()
			}
			return nil, agent.Classify(b.Label(), 0, "claude stopped before finishing: "+msg)
		}
		res.FinishReason = "stop"
	}
	slog.Debug(logging.Event("claude", "run", "finished"), slog.String("model", res.Model), slog.String("finish", res.FinishReason),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()), slog.Int64("prompt_tokens", res.Usage.PromptTokens()),
		slog.Int64("completion_tokens", res.Usage.OutputTokens))
	return res, nil
}

func (b *Backend) parse(stdout io.Reader, acc *agent.Account, opts agent.RunOptions, cb agent.Callbacks, kill func()) (*agent.Result, error) {
	res := &agent.Result{}
	var (
		text         strings.Builder
		blocks       = map[int]*block{}
		pending      []agent.ToolCall
		stopReason   string
		msgUsage     agent.Usage
		sumUsage     agent.Usage
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
		line, err := agent.ReadLine(rd, agent.MaxLineBytes)
		if len(bytes.TrimSpace(line)) > 0 {
			var sl streamLine
			if jerr := json.Unmarshal(line, &sl); jerr != nil {
				slog.Debug(logging.Event("claude", "output", "not-json"), slog.Int("bytes", len(line)))
			} else {
				switch sl.Type {
				case "system":
					if sl.Subtype == "init" && len(opts.Tools) > 0 {
						for _, s := range sl.MCPServers {
							if s.Name == agent.BridgeServerName && s.Status != "connected" {
								slog.Warn(logging.Event("bridge", "connect", "failed"), slog.String("status", s.Status))
							}
						}
					}
				case "rate_limit_event":
					acc.Status.RecordRateLimit(sl.RateLimitInfo)
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
						msgUsage = agent.Usage{}
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
						bl := &block{kind: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
						blocks[ev.Index] = bl
						if bl.kind == "tool_use" && strings.HasPrefix(bl.name, agent.BridgeToolPrefix) {
							bl.client = true
							short := strings.TrimPrefix(bl.name, agent.BridgeToolPrefix)
							if orig, ok := opts.ToolNames[short]; ok {
								bl.name = orig
							} else {
								bl.name = short
							}
							bl.index = len(pending)
							bl.id = "call_" + strings.TrimPrefix(bl.id, "toolu_")
							if cb.ToolStart != nil {
								cb.ToolStart(bl.index, bl.id, bl.name)
							}
						}
					case "content_block_delta":
						if ev.Delta == nil {
							break
						}
						bl := blocks[ev.Index]
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
							if bl != nil && bl.client {
								bl.args.WriteString(ev.Delta.PartialJSON)
								if cb.ToolArgs != nil && ev.Delta.PartialJSON != "" {
									cb.ToolArgs(bl.index, ev.Delta.PartialJSON)
								}
							}
						}
					case "content_block_stop":
						if bl := blocks[ev.Index]; bl != nil && bl.client {
							args := strings.TrimSpace(bl.args.String())
							if args == "" {
								args = "{}"
							}
							pending = append(pending, agent.ToolCall{ID: bl.id, Name: bl.name, Arguments: args})
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
						slog.Debug(logging.Event("claude", "assistant", "errored"), slog.String("kind", sl.Error))
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
						return nil, agent.Classify(b.Label(), status, msg)
					}
					res.Text = text.String()
					if !sawStream && res.Text == "" {
						res.Text = sl.Result
						if cb.Text != nil && !suppressText && res.Text != "" {
							cb.Text(res.Text)
						}
					}
					if opts.JSONObject {
						res.StructuredOutput = agent.ExtractJSON(res.Text)
					}
					if len(sl.StructuredOutput) > 0 && string(sl.StructuredOutput) != "null" {
						res.StructuredOutput = sl.StructuredOutput
					} else if len(opts.JSONSchema) > 0 {
						// Fall back to parsing the text result as JSON.
						if j := agent.ExtractJSON(sl.Result); j != nil {
							res.StructuredOutput = j
						} else if j := agent.ExtractJSON(res.Text); j != nil {
							res.StructuredOutput = j
						} else {
							return nil, &agent.Error{Kind: "upstream", Status: 502, Message: "Claude did not return the requested structured JSON output"}
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
	// Stream ended without a result line. FinishReason stays empty so Run
	// can tell a clean exit from a crash.
	if text.Len() > 0 {
		res.Text = text.String()
		res.Usage = sumUsage
		return res, nil
	}
	return nil, nil
}
