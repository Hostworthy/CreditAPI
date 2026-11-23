// Package fakecursor is a fake Cursor CLI (`cursor-agent`) for tests and the
// Bruno end-to-end stack. It imitates `cursor-agent -p --output-format
// stream-json` (shapes taken from the Cursor CLI documentation), `login`,
// `logout`, `status` and `--version`, so Cursor subscriptions can be exercised
// without a Cursor account or network access.
//
// It is enabled with CREDITAPI_FAKE_CURSOR=auto. Go tests re-execute their
// test binary as the fake (see the fakes package); `make e2e` uses the
// fakecursor binary.
//
// Authentication: CURSOR_API_KEY must start with KeyPrefix (one containing
// "expired" is rejected), or a `login` must have stored a session in
// $HOME/.cursor. `login` prints a sign-in URL and, like the real CLI, finishes
// by itself once the browser sign-in is approved; the fake approves after
// LoginDelay unless CREDITAPI_FAKE_CURSOR_LOGIN is "deny" or "hang".
//
// Every -p run checks that the server hardened the workspace (deny rules for
// shell, writes and web fetches, allowlist approval mode, no --force) and
// fails with exit code 4 if not.
package fakecursor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/atb00ker/CreditAPI/src/tests/support/fakecore"
)

// Environment variables understood by the fake.
const (
	EnvMode   = "CREDITAPI_FAKE_CURSOR"
	EnvLogin  = "CREDITAPI_FAKE_CURSOR_LOGIN"
	EnvRecord = "CREDITAPI_FAKE_RECORD"
)

const (
	// KeyPrefix marks API keys the fake accepts.
	KeyPrefix = "fake-cursor-key-"
	// ValidKey is an API key the fake accepts.
	ValidKey = KeyPrefix + "0123456789abcdefghijklmnop"
	// ExpiredKey looks like a fake key but is rejected.
	ExpiredKey = KeyPrefix + "expired-0123456789abcdefgh"
	// LoginURLPrefix starts every sign-in URL the fake prints.
	LoginURLPrefix = "https://cursor.com/loginDeepControl?mode=login&challenge="
	// LoginDelay is how long the fake "browser" takes to approve a sign-in.
	LoginDelay = 600 * time.Millisecond
	// Version is what --version prints.
	Version = "2026.11.22-fake"
	// NotLoggedIn is printed when there is no credential.
	NotLoggedIn = "Error: Authentication required. Please run 'cursor-agent login' first, or set CURSOR_API_KEY environment variable."
)

// MaybeRun runs the fake and exits when CREDITAPI_FAKE_CURSOR is set.
func MaybeRun() {
	if os.Getenv(EnvMode) == "" {
		return
	}
	os.Exit(Main(os.Args[1:]))
}

// Main runs the fake CLI with the given arguments and returns the exit code.
func Main(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "--version", "-v", "version":
			fmt.Println(Version)
			return 0
		case "login":
			return login()
		case "logout":
			return logout()
		case "status", "whoami":
			return status()
		}
	}
	if !slices.Contains(args, "-p") && !slices.Contains(args, "--print") {
		fmt.Fprintln(os.Stderr, "fakecursor: only -p, login, logout, status and --version are supported")
		return 2
	}
	inv, err := parseInvocation(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakecursor:", err)
		return 2
	}
	inv.record()
	return run(inv)
}

// --- sessions ---------------------------------------------------------------------

func sessionPath() string {
	return filepath.Join(os.Getenv("HOME"), ".cursor", "fake-session.json")
}

func loggedIn() bool {
	_, err := os.Stat(sessionPath())
	return err == nil
}

func login() int {
	fmt.Print("\x1b[?25l⠋ Starting login process...\r\n")
	url := LoginURLPrefix + fakecore.RandomHex(12) + "&uuid=" + fakecore.RandomHex(8)
	if os.Getenv("NO_OPEN_BROWSER") == "" {
		fmt.Print("Opening browser...\r\n")
	}
	fmt.Printf("\r\nOpen the following link to sign in:\r\n\x1b[36m%s\x1b[39m\r\n\r\nWaiting for browser approval...\r\n", url)
	switch os.Getenv(EnvLogin) {
	case "deny":
		time.Sleep(LoginDelay)
		fmt.Print("\x1b[31m✗ Login failed: the request was denied\x1b[39m\r\n")
		return 1
	case "hang":
		time.Sleep(10 * time.Minute)
		return 1
	}
	time.Sleep(LoginDelay)
	if err := os.MkdirAll(filepath.Dir(sessionPath()), 0o700); err != nil {
		fmt.Println("fakecursor:", err)
		return 1
	}
	if err := os.WriteFile(sessionPath(), []byte(`{"user":"fake@example.com"}`), 0o600); err != nil {
		fmt.Println("fakecursor:", err)
		return 1
	}
	fmt.Print("\x1b[32m✓ Login successful!\x1b[39m\r\n")
	return 0
}

func logout() int {
	if err := os.Remove(sessionPath()); err != nil {
		fmt.Println("Not logged in")
		return 0
	}
	fmt.Println("✓ Logged out successfully")
	return 0
}

func status() int {
	switch {
	case strings.HasPrefix(os.Getenv("CURSOR_API_KEY"), KeyPrefix):
		fmt.Println("✓ Logged in with an API key")
	case loggedIn():
		fmt.Println("✓ Logged in as fake@example.com")
	default:
		fmt.Println("Not logged in")
	}
	return 0
}

// --- invocation -------------------------------------------------------------------

type invocation struct {
	args      []string
	model     string
	workspace string
	partial   bool
	prompt    string
	// instructions and body are the two halves of the prompt the server
	// builds: <instructions>...</instructions> and the conversation.
	instructions string
	body         string
	schema       map[string]any
	bridgeURL    string
	tools        []fakecore.Tool
	images       []string
}

var valueFlags = map[string]bool{"--output-format": true, "--model": true, "-m": true, "--workspace": true,
	"--api-key": true, "--resume": true, "--input-format": true}

func parseInvocation(args []string) (*invocation, error) {
	inv := &invocation{args: args}
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case valueFlags[a] && i+1 < len(args):
			switch a {
			case "--model", "-m":
				inv.model = args[i+1]
			case "--workspace":
				inv.workspace = args[i+1]
			case "--output-format":
				if args[i+1] != "stream-json" {
					return nil, fmt.Errorf("the server should ask for stream-json, got %q", args[i+1])
				}
			}
			i++
		case a == "--stream-partial-output":
			inv.partial = true
		case strings.HasPrefix(a, "-"):
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) == 0 {
		return nil, fmt.Errorf("no prompt")
	}
	inv.prompt = positional[len(positional)-1]
	if inv.workspace == "" {
		inv.workspace, _ = os.Getwd()
	}

	full := inv.prompt
	if strings.Contains(full, "./conversation.md") && !strings.Contains(full, "<instructions>") {
		b, err := os.ReadFile(filepath.Join(inv.workspace, "conversation.md"))
		if err != nil {
			return nil, fmt.Errorf("read conversation.md: %w", err)
		}
		full = string(b)
	}
	if i := strings.Index(full, "<instructions>"); i >= 0 {
		if j := strings.Index(full, "</instructions>"); j > i {
			inv.instructions = strings.TrimSpace(full[i+len("<instructions>") : j])
			full = full[j+len("</instructions>"):]
		}
	}
	const schemaIntro = "matches this JSON schema, with no code fences and no other text:\n"
	if i := strings.Index(full, schemaIntro); i >= 0 {
		rest := full[i+len(schemaIntro):]
		line, _, _ := strings.Cut(rest, "\n")
		if err := json.Unmarshal([]byte(line), &inv.schema); err != nil {
			return nil, fmt.Errorf("schema is not JSON: %w", err)
		}
	}
	inv.body = full

	for _, line := range strings.Split(full, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[image: ") && strings.HasSuffix(line, "]") {
			inv.images = append(inv.images, strings.TrimSuffix(strings.TrimPrefix(line, "[image: "), "]"))
		}
	}

	if raw, err := os.ReadFile(filepath.Join(inv.workspace, ".cursor", "mcp.json")); err == nil {
		var cfg struct {
			MCPServers map[string]struct {
				URL string `json:"url"`
			} `json:"mcpServers"`
		}
		_ = json.Unmarshal(raw, &cfg)
		inv.bridgeURL = cfg.MCPServers["oai"].URL
		if inv.bridgeURL != "" && slices.Contains(args, "--approve-mcps") {
			inv.tools = fakecore.ListTools(inv.bridgeURL)
		}
	}
	return inv, nil
}

// Record is what the fake saw; written to CREDITAPI_FAKE_RECORD when set.
type Record struct {
	Args          []string          `json:"args"`
	Prompt        string            `json:"prompt"`
	Instructions  string            `json:"instructions"`
	Env           map[string]string `json:"env"`
	BridgeTools   []string          `json:"bridge_tools"`
	ProjectConfig json.RawMessage   `json:"project_config"`
	GlobalConfig  json.RawMessage   `json:"global_config"`
	Attachments   []string          `json:"attachments"`
}

func (inv *invocation) record() {
	path := os.Getenv(EnvRecord)
	if path == "" {
		return
	}
	rec := Record{Args: inv.args, Prompt: inv.prompt, Instructions: inv.instructions, Env: map[string]string{}}
	for _, k := range []string{"CURSOR_API_KEY", "HOME", "NO_OPEN_BROWSER", "ADMIN_PASSWORD", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if v, ok := os.LookupEnv(k); ok {
			rec.Env[k] = v
		}
	}
	for _, t := range inv.tools {
		rec.BridgeTools = append(rec.BridgeTools, t.Name)
	}
	rec.ProjectConfig, _ = os.ReadFile(filepath.Join(inv.workspace, ".cursor", "cli.json"))
	rec.GlobalConfig, _ = os.ReadFile(filepath.Join(os.Getenv("HOME"), ".cursor", "cli-config.json"))
	entries, _ := os.ReadDir(filepath.Join(inv.workspace, "attachments"))
	for _, e := range entries {
		rec.Attachments = append(rec.Attachments, e.Name())
	}
	b, _ := json.Marshal(rec)
	_ = os.WriteFile(path, b, 0o600)
}

// checkHardening verifies the deny rules the server must set up.
func (inv *invocation) checkHardening() error {
	if slices.Contains(inv.args, "--force") || slices.Contains(inv.args, "-f") || slices.Contains(inv.args, "--yolo") {
		return fmt.Errorf("--force must never be passed")
	}
	var project struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	raw, err := os.ReadFile(filepath.Join(inv.workspace, ".cursor", "cli.json"))
	if err != nil {
		return fmt.Errorf("workspace has no .cursor/cli.json: %w", err)
	}
	if err := json.Unmarshal(raw, &project); err != nil {
		return err
	}
	for _, want := range []string{"Shell(*)", "Write(**)", "WebFetch(*)"} {
		if !slices.Contains(project.Permissions.Deny, want) {
			return fmt.Errorf("workspace permissions do not deny %s", want)
		}
	}
	var global struct {
		ApprovalMode string `json:"approvalMode"`
	}
	raw, err = os.ReadFile(filepath.Join(os.Getenv("HOME"), ".cursor", "cli-config.json"))
	if err != nil {
		return fmt.Errorf("no cli-config.json in HOME: %w", err)
	}
	if err := json.Unmarshal(raw, &global); err != nil {
		return err
	}
	if global.ApprovalMode != "allowlist" {
		return fmt.Errorf("approvalMode is %q, want allowlist", global.ApprovalMode)
	}
	return nil
}
