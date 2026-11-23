// Package cursor is the Cursor backend: it answers completions with the
// Cursor CLI (`cursor-agent -p`, stream-json) and signs in with
// `cursor-agent login` or a Cursor API key.
//
// Unlike Claude Code, the Cursor CLI cannot switch its built-in tools off.
// Every run therefore happens in an empty, throwaway workspace with deny rules
// for shell commands, writes, web fetches and reads of sensitive paths, and
// without --force. Client tools are caught at the MCP bridge.
package cursor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/atb00ker/CreditAPI/src/internal/agent"
	"github.com/atb00ker/CreditAPI/src/internal/fsutil"
	"github.com/atb00ker/CreditAPI/src/internal/logging"
)

// Provider is the identifier stored with Cursor subscriptions.
const Provider = agent.ProviderCursor

// maxArgPrompt keeps the prompt argument well under Linux's 128 KiB limit
// for a single argument; longer prompts are passed as a file.
const maxArgPrompt = 96 << 10

// toolGrace is how long to wait for more tool calls after the first one.
const toolGrace = 400 * time.Millisecond

// Backend runs the Cursor CLI.
type Backend struct {
	Bin     string
	Bridge  *agent.Bridge
	Default string
	List    []string
	// Protected are paths the agent must never read (data directory, ...).
	Protected []string

	cfgMu sync.Mutex
}

var _ agent.Backend = (*Backend)(nil)

// Provider implements agent.Backend.
func (b *Backend) Provider() string { return Provider }

// Label implements agent.Backend.
func (b *Backend) Label() string { return "Cursor" }

// CredentialEnv implements agent.Backend.
func (b *Backend) CredentialEnv() string { return "CURSOR_API_KEY" }

// Models implements agent.Backend.
func (b *Backend) Models() []string { return b.List }

// DefaultModel implements agent.Backend.
func (b *Backend) DefaultModel() string { return b.Default }

// Accepts implements agent.Backend: the configured Cursor models.
func (b *Backend) Accepts(model string) bool {
	for _, m := range b.List {
		if strings.EqualFold(m, model) {
			return true
		}
	}
	return strings.EqualFold(model, b.Default)
}

// Version implements agent.Backend.
func (b *Backend) Version(ctx context.Context, env []string) string {
	return agent.CLIVersion(ctx, b.Bin, env)
}

// ValidateCredential implements agent.Backend.
func (b *Backend) ValidateCredential(key string) error {
	if len(key) < 20 {
		return errors.New("that does not look like a Cursor API key (create one in the Cursor dashboard)")
	}
	return nil
}

func loginEnv(acc *agent.Account) []string {
	return append(acc.BaseEnv(), "NO_OPEN_BROWSER=1", "BROWSER=/bin/false", "TERM=xterm-256color", "COLUMNS=1000", "LINES=50")
}

// Login implements agent.Backend: `cursor-agent login` prints a sign-in URL
// and finishes by itself once the browser sign-in is approved. The CLI keeps
// the credentials in the account's home directory.
func (b *Backend) Login(acc *agent.Account) agent.LoginSpec {
	return agent.LoginSpec{
		Bin:  b.Bin,
		Args: []string{"login"},
		Env:  loginEnv(acc),
		Done: acc.Tokens.Touch,
		// Unconfirmed against the real CLI; the documented sign-in host.
		URLHosts: []string{"cursor.com", "cursor.sh"},
	}
}

// Logout implements agent.Backend: `cursor-agent logout` clears what the CLI
// stored in the account's home directory.
func (b *Backend) Logout(ctx context.Context, acc *agent.Account) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Bin, "logout")
	cmd.Env = loginEnv(acc)
	cmd.Dir = os.TempDir()
	out, err := cmd.CombinedOutput()
	if err != nil && !strings.Contains(strings.ToLower(string(out)), "not logged in") {
		return fmt.Errorf("cursor-agent logout: %v: %s", err, agent.Truncate(strings.TrimSpace(string(out)), 200))
	}
	return nil
}

// --- hardening ------------------------------------------------------------------

func (b *Backend) permissions(acc *agent.Account) map[string]any {
	deny := []string{"Shell(*)", "Write(**)", "Write(/**)", "WebFetch(*)"}
	protected := append([]string{acc.Home, "/proc", "/sys", "/etc", "/root", "/home", "/var", "/run"}, b.Protected...)
	for _, p := range protected {
		if p != "" {
			deny = append(deny, "Read("+filepath.Clean(p)+"/**)")
		}
	}
	return map[string]any{
		"allow": []string{"Read(./attachments/**)", "Read(./conversation.md)", "Mcp(" + agent.BridgeServerName + ":*)"},
		"deny":  deny,
	}
}

// harden writes the workspace permission file and merges the same rules
// into the account's global CLI config, keeping fields the CLI stores there.
func (b *Backend) harden(acc *agent.Account, ws string) error {
	perms := b.permissions(acc)
	project, _ := json.MarshalIndent(map[string]any{"permissions": perms}, "", "  ")
	if err := os.MkdirAll(filepath.Join(ws, ".cursor"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(ws, ".cursor", "cli.json"), project, 0o600); err != nil {
		return err
	}

	b.cfgMu.Lock()
	defer b.cfgMu.Unlock()
	path := filepath.Join(acc.Home, ".cursor", "cli-config.json")
	cfg := map[string]any{}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &cfg)
	}
	cfg["permissions"] = perms
	cfg["approvalMode"] = "allowlist"
	out, _ := json.MarshalIndent(cfg, "", "  ")
	return fsutil.WriteFileAtomic(path, out, 0o600)
}

// --- prompt ----------------------------------------------------------------------

func extension(mediaType string) string {
	switch mediaType {
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ".jpg"
}

var closeInstructionsRe = regexp.MustCompile(`(?i)<(\s*/\s*instructions)`)

// buildPrompt renders instructions and the conversation as one prompt. Images
// are written to ./attachments and referenced by path. Prompts too long for a
// command-line argument are written to ./conversation.md.
func buildPrompt(ws string, opts agent.RunOptions) (string, error) {
	var conv strings.Builder
	var images []string
	for _, blk := range opts.Content {
		switch blk.Type {
		case "text":
			if opts.Transcript {
				conv.WriteString(blk.Text) // already escaped by the transcript builder
			} else {
				conv.WriteString(agent.EscapeTags(blk.Text))
			}
		case "image":
			if blk.Source == nil {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(blk.Source.Data)
			if err != nil {
				return "", err
			}
			rel := fmt.Sprintf("./attachments/image-%d%s", len(images)+1, extension(blk.Source.MediaType))
			path := filepath.Join(ws, rel)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return "", err
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				return "", err
			}
			images = append(images, rel)
			conv.WriteString("\n[image: " + rel + "]\n")
		}
	}
	body := strings.TrimSpace(conv.String())
	if !opts.Transcript {
		body = "<user>\n" + body + "\n</user>\n\nWrite the assistant's reply to the last user message."
	}

	var p strings.Builder
	p.WriteString("<instructions>\n")
	// The instructions may mention tags; only a closing </instructions>
	// could break the structure, so only that is neutralized.
	p.WriteString(closeInstructionsRe.ReplaceAllString(strings.TrimSpace(opts.SystemPrompt), "&lt;$1"))
	p.WriteString("\n</instructions>\n\n")
	p.WriteString("You are answering a chat request through an API. Reply to the user directly. Do not create, edit or run anything.")
	if len(opts.Tools) > 0 {
		p.WriteString(" The only tools you may use are the functions from the `" + agent.BridgeServerName + "` MCP server.")
	}
	if len(images) > 0 {
		p.WriteString(" The user attached images as files; open them with your file-reading tool to look at them: " + strings.Join(images, ", ") + ".")
	}
	if len(opts.JSONSchema) > 0 {
		p.WriteString("\n\nReply with only a JSON value that matches this JSON schema, with no code fences and no other text:\n")
		p.Write(opts.JSONSchema)
	}
	p.WriteString("\n\n")
	p.WriteString(body)
	prompt := p.String()
	if len(prompt) <= maxArgPrompt {
		return prompt, nil
	}
	if err := os.WriteFile(filepath.Join(ws, "conversation.md"), []byte(prompt), 0o600); err != nil {
		return "", err
	}
	return "Read the file ./conversation.md in the current directory and follow it exactly: it contains your instructions and the conversation to answer.", nil
}

// --- run -------------------------------------------------------------------------

type event struct {
	Type        string  `json:"type"`
	Subtype     string  `json:"subtype"`
	Model       string  `json:"model"`
	CallID      string  `json:"call_id"`
	IsError     bool    `json:"is_error"`
	Result      string  `json:"result"`
	TimestampMS *int64  `json:"timestamp_ms"`
	ModelCallID *string `json:"model_call_id"`
	Message     *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

func (e *event) text() string {
	if e.Message == nil {
		return ""
	}
	var sb strings.Builder
	for _, c := range e.Message.Content {
		if c.Type == "text" || c.Type == "" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

type outcome struct {
	res *agent.Result
	err error
}

func newCallID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "call_" + hex.EncodeToString(b)
}

// Run implements agent.Backend.
func (b *Backend) Run(ctx context.Context, acc *agent.Account, opts agent.RunOptions, cb agent.Callbacks) (*agent.Result, error) {
	root, err := agent.RunRoot(Provider)
	if err != nil {
		return nil, err
	}
	ws, err := os.MkdirTemp(root, "run-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(ws)
	if err := b.harden(acc, ws); err != nil {
		return nil, fmt.Errorf("prepare cursor workspace: %w", err)
	}
	prompt, err := buildPrompt(ws, opts)
	if err != nil {
		return nil, err
	}

	args := []string{"-p", "--output-format", "stream-json", "--stream-partial-output",
		"--model", opts.Model, "--workspace", ws, "--trust"}
	var calls <-chan agent.BridgeCall
	if len(opts.Tools) > 0 && b.Bridge != nil {
		reg := b.Bridge.Register(opts.Tools)
		defer reg.Close()
		cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
			agent.BridgeServerName: map[string]any{"url": reg.URL},
		}})
		if err := os.WriteFile(filepath.Join(ws, ".cursor", "mcp.json"), cfg, 0o600); err != nil {
			return nil, err
		}
		args = append(args, "--approve-mcps")
		calls = reg.Calls
	}
	args = append(args, prompt)

	// TMPDIR keeps the CLI's own temporary files inside this run's workspace.
	env := append(acc.Env(b), "NO_OPEN_BROWSER=1", "TMPDIR="+ws)
	start := time.Now()
	proc, err := agent.StartProc(ctx, b.Bin, args, env, ws, nil)
	if err != nil {
		return nil, &agent.Error{Kind: agent.KindUpstream, Status: 502, Message: fmt.Sprintf("cannot start %s: %v", b.Bin, err)}
	}

	var textMu sync.Mutex
	var streamed strings.Builder
	suppress := len(opts.JSONSchema) > 0 || opts.JSONObject
	emit := func(s string) {
		textMu.Lock()
		streamed.WriteString(s)
		textMu.Unlock()
		if cb.Text != nil && !suppress {
			cb.Text(s)
		}
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := b.parse(proc.Stdout, emit)
		done <- outcome{res, err}
	}()

	var out outcome
	select {
	case out = <-done:
	case first := <-calls:
		// The model wants the client to run a tool. Collect any parallel
		// calls, then stop the CLI and hand them back.
		pending := collectCalls(first, calls)
		proc.Kill()
		<-done
		_ = proc.Wait()
		textMu.Lock()
		text := streamed.String()
		textMu.Unlock()
		return toolCallResult(pending, text, prompt, opts, cb), nil
	}
	waitErr := proc.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if out.err != nil {
		return nil, out.err
	}
	res := out.res
	if res == nil {
		msg := strings.TrimSpace(proc.Stderr.String())
		if msg == "" && waitErr != nil {
			msg = waitErr.Error()
		}
		if msg == "" {
			msg = "cursor-agent exited without a result"
		}
		return nil, agent.Classify(b.Label(), 0, msg)
	}
	if res.Model == "" {
		res.Model = opts.Model
	}
	switch {
	case len(opts.JSONSchema) > 0:
		j := agent.ExtractJSON(res.Text)
		if j == nil {
			return nil, &agent.Error{Kind: agent.KindUpstream, Status: 502, Message: "Cursor did not return the requested structured JSON output"}
		}
		res.StructuredOutput = j
	case opts.JSONObject:
		res.StructuredOutput = agent.ExtractJSON(res.Text)
	}
	res.Usage = agent.Usage{InputTokens: agent.EstimateTokens(prompt), OutputTokens: agent.EstimateTokens(res.Text)}
	slog.Debug(logging.Event("cursor", "run", "finished"), slog.String("model", res.Model),
		slog.Int64("duration_ms", time.Since(start).Milliseconds()))
	return res, nil
}

// collectCalls waits briefly for parallel tool calls after the first one.
func collectCalls(first agent.BridgeCall, calls <-chan agent.BridgeCall) []agent.BridgeCall {
	pending := []agent.BridgeCall{first}
	timer := time.NewTimer(toolGrace)
	defer timer.Stop()
	for {
		select {
		case c := <-calls:
			pending = append(pending, c)
		case <-timer.C:
			return pending
		}
	}
}

// toolCallResult turns the calls that reached the bridge into an OpenAI
// tool_calls result, restoring the client's tool names.
func toolCallResult(pending []agent.BridgeCall, text, prompt string, opts agent.RunOptions, cb agent.Callbacks) *agent.Result {
	res := &agent.Result{Text: text, FinishReason: "tool_calls", Model: opts.Model}
	for i, c := range pending {
		name := c.Name
		if orig, ok := opts.ToolNames[name]; ok {
			name = orig
		}
		argsJSON := strings.TrimSpace(string(c.Arguments))
		if argsJSON == "" || argsJSON == "null" {
			argsJSON = "{}"
		}
		call := agent.ToolCall{ID: newCallID(), Name: name, Arguments: argsJSON}
		res.ToolCalls = append(res.ToolCalls, call)
		if cb.ToolStart != nil {
			cb.ToolStart(i, call.ID, call.Name)
		}
		if cb.ToolArgs != nil {
			cb.ToolArgs(i, call.Arguments)
		}
	}
	res.Usage = agent.Usage{InputTokens: agent.EstimateTokens(prompt), OutputTokens: agent.EstimateTokens(text) + 20*int64(len(pending))}
	return res
}

// parse reads the stream-json events. With --stream-partial-output, text
// deltas carry timestamp_ms and no model_call_id; other assistant events are
// buffered duplicates and are only used when no deltas arrived.
func (b *Backend) parse(stdout io.Reader, emit func(string)) (*agent.Result, error) {
	res := &agent.Result{}
	var deltas, complete strings.Builder
	sawDelta := false
	rd := bufio.NewReaderSize(stdout, 256<<10)
	for {
		line, err := agent.ReadLine(rd, agent.MaxLineBytes)
		if len(bytes.TrimSpace(line)) > 0 {
			var ev event
			if jerr := json.Unmarshal(line, &ev); jerr != nil {
				slog.Debug(logging.Event("cursor", "output", "not-json"), slog.Int("bytes", len(line)))
			} else {
				switch ev.Type {
				case "system":
					if ev.Subtype == "init" && ev.Model != "" {
						res.Model = ev.Model
					}
				case "assistant":
					t := ev.text()
					if ev.TimestampMS != nil && ev.ModelCallID == nil {
						if t != "" {
							sawDelta = true
							deltas.WriteString(t)
							emit(t)
						}
					} else if ev.TimestampMS == nil && ev.ModelCallID == nil {
						complete.WriteString(t)
					}
				case "tool_call":
					slog.Debug(logging.Event("cursor", "tool", ev.Subtype), slog.String("call_id", ev.CallID))
				case "result":
					if ev.IsError || (ev.Subtype != "" && ev.Subtype != "success") {
						msg := ev.Result
						if msg == "" {
							msg = "cursor reported an error (" + ev.Subtype + ")"
						}
						return nil, agent.Classify(b.Label(), 0, msg)
					}
					switch {
					case sawDelta:
						res.Text = deltas.String()
					case ev.Result != "":
						res.Text = ev.Result
						emit(res.Text)
					default:
						res.Text = complete.String()
						emit(res.Text)
					}
					res.FinishReason = "stop"
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
	return nil, nil
}
