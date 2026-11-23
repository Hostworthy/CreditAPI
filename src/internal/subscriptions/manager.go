package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/atb00ker/CreditAPI/src/internal/agent"
	"github.com/atb00ker/CreditAPI/src/internal/logging"
)

// Routing controls how model names map to subscriptions.
type Routing struct {
	// Aliases map extra names (for example gpt-4o) to model names.
	Aliases map[string]string
	// Strict rejects model names no subscription knows instead of using
	// each subscription's default model.
	Strict bool
}

// Manager ties the store to backends and per-subscription runtimes.
type Manager struct {
	Store   *Store
	Login   *agent.LoginManager
	dataDir string
	env     *agent.EnvBuilder
	routing Routing

	backends map[string]agent.Backend
	order    []string

	mu       sync.Mutex
	accounts map[string]*agent.Account
	versions map[string]string
}

// NewManager creates a manager for the given backends.
func NewManager(dataDir string, store *Store, env *agent.EnvBuilder, routing Routing, backends ...agent.Backend) *Manager {
	m := &Manager{Store: store, Login: agent.NewLoginManager(), dataDir: dataDir, env: env, routing: routing,
		backends: map[string]agent.Backend{}, accounts: map[string]*agent.Account{}, versions: map[string]string{}}
	for _, b := range backends {
		m.backends[b.Provider()] = b
		m.order = append(m.order, b.Provider())
	}
	return m
}

// Backend returns the backend for a provider.
func (m *Manager) Backend(provider string) (agent.Backend, bool) {
	b, ok := m.backends[provider]
	return b, ok
}

// Providers lists the supported providers in a stable order.
func (m *Manager) Providers() []agent.Backend {
	out := make([]agent.Backend, 0, len(m.order))
	for _, p := range m.order {
		out = append(out, m.backends[p])
	}
	return out
}

var idRe = regexp.MustCompile(`^[a-z0-9]{1,32}$`)

func (m *Manager) dir(id string) string { return filepath.Join(m.dataDir, "subscriptions", id) }

// Account returns the runtime (home directory, credential, status) of a
// subscription, creating it on first use.
func (m *Manager) Account(sub Subscription) (*agent.Account, agent.Backend, error) {
	b, ok := m.backends[sub.Provider]
	if !ok {
		return nil, nil, fmt.Errorf("subscription %q uses unknown provider %q", sub.Name, sub.Provider)
	}
	if !idRe.MatchString(sub.ID) {
		return nil, nil, fmt.Errorf("invalid subscription id %q", sub.ID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if acc, ok := m.accounts[sub.ID]; ok {
		return acc, b, nil
	}
	home := filepath.Join(m.dir(sub.ID), "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, nil, err
	}
	envCred := ""
	if sub.EnvCredential != "" {
		envCred = os.Getenv(sub.EnvCredential)
	}
	acc := &agent.Account{
		ID: sub.ID, Name: sub.Name, Provider: sub.Provider, Home: home,
		Tokens:  agent.NewTokenStore(filepath.Join(m.dir(sub.ID), "credential"), envCred, b.ValidateCredential),
		Status:  &agent.Status{},
		BaseEnv: func() []string { return m.env.Base(home) },
	}
	m.accounts[sub.ID] = acc
	return acc, b, nil
}

// AccountByID looks up a subscription and its runtime.
func (m *Manager) AccountByID(id string) (Subscription, *agent.Account, agent.Backend, error) {
	sub, ok := m.Store.Get(id)
	if !ok {
		return Subscription{}, nil, nil, ErrNotFound
	}
	acc, b, err := m.Account(sub)
	return sub, acc, b, err
}

// Create adds a subscription for a known provider.
func (m *Manager) Create(name, provider string, keyIDs []string) (Subscription, error) {
	if _, ok := m.backends[provider]; !ok {
		return Subscription{}, fmt.Errorf("provider must be one of %s", strings.Join(m.order, ", "))
	}
	sub, err := m.Store.Create(name, provider, keyIDs, "")
	if err != nil {
		return Subscription{}, err
	}
	slog.Info(logging.Event("subscription", "create", "succeeded"), slog.String("subscription", sub.ID), slog.String("provider", provider))
	return sub, nil
}

// Delete removes a subscription with its credential and CLI files.
func (m *Manager) Delete(id string) error {
	if !idRe.MatchString(id) {
		return ErrNotFound
	}
	m.Login.Cancel(id)
	if err := m.Store.Delete(id); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.accounts, id)
	m.mu.Unlock()
	if err := os.RemoveAll(m.dir(id)); err != nil {
		return err
	}
	slog.Info(logging.Event("subscription", "delete", "succeeded"), slog.String("subscription", id))
	return nil
}

// SignOut removes a subscription's saved credential and whatever its CLI
// stored itself.
func (m *Manager) SignOut(ctx context.Context, id string) error {
	_, acc, b, err := m.AccountByID(id)
	if err != nil {
		return err
	}
	m.Login.Cancel(id)
	logoutErr := b.Logout(ctx, acc)
	if err := acc.Tokens.Clear(); err != nil {
		return err
	}
	return logoutErr
}

// DetectVersions records each backend's CLI version.
func (m *Manager) DetectVersions(ctx context.Context) {
	for _, b := range m.Providers() {
		v := b.Version(ctx, m.env.Base(os.TempDir()))
		m.mu.Lock()
		m.versions[b.Provider()] = v
		m.mu.Unlock()
		slog.Info(logging.Event("cli", "version", "detected"), slog.String("provider", b.Provider()), slog.String("version", v))
	}
}

// Version returns the detected CLI version for a provider.
func (m *Manager) Version(provider string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.versions[provider]
}

// ProbeModel is the model used for login checks (the cheapest one).
func (m *Manager) ProbeModel(b agent.Backend) string {
	if b.Accepts("haiku") {
		return "haiku"
	}
	return b.DefaultModel()
}

// Migration describes what an older single-subscription install had.
type Migration struct {
	KeyIDs []string
	// LegacyToken and LegacyHome are the old claude-oauth-token file and
	// claude-home directory.
	LegacyToken string
	LegacyHome  string
	// ClaudeEnv and CursorEnv report whether CLAUDE_CODE_OAUTH_TOKEN and
	// CURSOR_API_KEY are set.
	ClaudeEnv bool
	CursorEnv bool
}

// Bootstrap creates the first subscriptions for a new store: always one for
// Claude (taking over an older install's token and CLI home), and one for
// Cursor when CURSOR_API_KEY is set. Existing keys get access to both.
func (m *Manager) Bootstrap(mig Migration) error {
	if _, ok := m.backends[agent.ProviderClaude]; ok {
		sub, err := m.Store.Create("Claude", agent.ProviderClaude, mig.KeyIDs, "CLAUDE_CODE_OAUTH_TOKEN")
		if err != nil {
			return err
		}
		dir := m.dir(sub.ID)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if mig.LegacyToken != "" {
			if err := moveIfExists(mig.LegacyToken, filepath.Join(dir, "credential")); err != nil {
				return err
			}
		}
		if mig.LegacyHome != "" {
			if err := moveIfExists(mig.LegacyHome, filepath.Join(dir, "home")); err != nil {
				return err
			}
		}
	}
	if _, ok := m.backends[agent.ProviderCursor]; ok && mig.CursorEnv {
		if _, err := m.Store.Create("Cursor", agent.ProviderCursor, mig.KeyIDs, "CURSOR_API_KEY"); err != nil {
			return err
		}
	}
	slog.Info(logging.Event("subscription", "bootstrap", "succeeded"), slog.Int("keys", len(mig.KeyIDs)))
	return nil
}

func moveIfExists(from, to string) error {
	if _, err := os.Stat(from); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if _, err := os.Stat(to); err == nil {
		return nil
	}
	return os.Rename(from, to)
}

// --- routing ----------------------------------------------------------------------

// Candidate is one way to answer a request: a subscription and the model to
// ask it for.
type Candidate struct {
	Sub     Subscription
	Account *agent.Account
	Backend agent.Backend
	Model   string
}

// Candidates lists, in the order to try them, the subscriptions that can
// answer a request from keyID for the requested model:
//
//   - "slug/model" (for example "cursor-personal/auto") pins one subscription;
//   - otherwise subscriptions whose provider knows the model come first, then
//     the key's other subscriptions with their default model (unless strict
//     routing is on), each group in priority order.
func (m *Manager) Candidates(keyID, requested string) ([]Candidate, error) {
	requested = strings.TrimSpace(requested)
	if requested != "" && !agent.ValidModelName(requested) {
		return nil, &agent.Error{Kind: agent.KindInvalidModel, Status: 400,
			Message: "invalid model name: use letters, digits and . _ : / [ ] - (at most 128 characters)"}
	}
	subs := m.Store.ForKey(keyID)

	if slug, rest, ok := strings.Cut(requested, "/"); ok {
		for _, s := range m.Store.List() {
			if s.Slug() != slug && s.ID != slug {
				continue
			}
			if s.Disabled || !s.HasKey(keyID) {
				return nil, &agent.Error{Kind: agent.KindForbidden, Status: 403,
					Message: fmt.Sprintf("this API key cannot use the subscription %q", s.Name)}
			}
			acc, b, err := m.Account(s)
			if err != nil {
				return nil, err
			}
			model := m.alias(rest)
			if model == "" {
				model = b.DefaultModel()
			} else if !b.Accepts(model) {
				return nil, &agent.Error{Kind: agent.KindNotFound, Status: 404,
					Message: fmt.Sprintf("model %s is not available on %q; see /v1/models", rest, s.Name)}
			}
			return []Candidate{{Sub: s, Account: acc, Backend: b, Model: model}}, nil
		}
		// Not a subscription name: treat the whole string as a model name.
	}

	if len(subs) == 0 {
		return nil, &agent.Error{Kind: agent.KindForbidden, Status: 403,
			Message: "this API key has no subscriptions; grant it one on the /admin page"}
	}
	model := m.alias(requested)
	var accept, rest []Candidate
	for _, s := range subs {
		acc, b, err := m.Account(s)
		if err != nil {
			slog.Warn(logging.Event("subscription", "account", "failed"), slog.String("subscription", s.ID), logging.Err(err))
			continue
		}
		if model != "" && b.Accepts(model) {
			accept = append(accept, Candidate{Sub: s, Account: acc, Backend: b, Model: model})
		} else {
			rest = append(rest, Candidate{Sub: s, Account: acc, Backend: b, Model: b.DefaultModel()})
		}
	}
	if model != "" && len(accept) == 0 && m.routing.Strict {
		return nil, &agent.Error{Kind: agent.KindNotFound, Status: 404,
			Message: fmt.Sprintf("model %s is not available; see /v1/models", requested)}
	}
	if m.routing.Strict && len(accept) > 0 {
		return accept, nil
	}
	return append(accept, rest...), nil
}

func (m *Manager) alias(model string) string {
	if a, ok := m.routing.Aliases[model]; ok {
		return a
	}
	return model
}

// ModelsForKey lists the model names keyID can use: each provider's models,
// the same pinned to each subscription ("slug/model"), and aliases.
func (m *Manager) ModelsForKey(keyID string) []string {
	seen := map[string]bool{}
	var plain, pinned []string
	add := func(list *[]string, s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			*list = append(*list, s)
		}
	}
	for _, s := range m.Store.ForKey(keyID) {
		b, ok := m.backends[s.Provider]
		if !ok {
			continue
		}
		models := append([]string{b.DefaultModel()}, b.Models()...)
		for _, model := range models {
			add(&plain, model)
			add(&pinned, s.Slug()+"/"+model)
		}
	}
	if len(plain) > 0 {
		aliases := make([]string, 0, len(m.routing.Aliases))
		for a := range m.routing.Aliases {
			aliases = append(aliases, a)
		}
		sort.Strings(aliases)
		for _, a := range aliases {
			add(&plain, a)
		}
	}
	return append(plain, pinned...)
}
