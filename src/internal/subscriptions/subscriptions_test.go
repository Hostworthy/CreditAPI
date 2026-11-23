package subscriptions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/atb00ker/CreditAPI/src/internal/agent"
)

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Claude (Personal)": "claude-personal",
		"  Cursor / Friend": "cursor-friend",
		"ÄÖ":                "subscription",
		"Work2":             "work2",
	}
	for in, want := range cases {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func openStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "subscriptions.json")
	st, existed, err := OpenStore(path)
	if err != nil || existed {
		t.Fatalf("open: %v existed=%v", err, existed)
	}
	return st, path
}

func TestStoreCRUD(t *testing.T) {
	st, path := openStore(t)
	a, err := st.Create("Claude (Personal)", "claude", []string{"k1", "k2", "k1", " "}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(a.Keys, []string{"k1", "k2"}) || a.Slug() != "claude-personal" || a.ID == "" {
		t.Fatalf("created %+v", a)
	}
	if _, err := st.Create("claude personal", "cursor", nil, ""); err == nil {
		t.Fatal("names with the same slug must be refused")
	}
	if _, err := st.Create(" ", "cursor", nil, ""); err == nil {
		t.Fatal("empty name must be refused")
	}
	if _, err := st.Create(strings.Repeat("x", 61), "cursor", nil, ""); err == nil {
		t.Fatal("long name must be refused")
	}
	b, _ := st.Create("Cursor (Personal)", "cursor", nil, "CURSOR_API_KEY")

	name, off, keys := "Claude (Home)", true, []string{"k3"}
	u, err := st.Update(a.ID, &name, &off, &keys)
	if err != nil || u.Name != "Claude (Home)" || !u.Disabled || !slices.Equal(u.Keys, []string{"k3"}) {
		t.Fatalf("update %+v %v", u, err)
	}
	dup := "cursor personal"
	if _, err := st.Update(a.ID, &dup, nil, nil); err == nil {
		t.Fatal("rename to a taken slug must be refused")
	}
	if _, err := st.Update("nope", &name, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v", err)
	}

	// Persisted, in order, with 0600 permissions.
	reopened, existed, err := OpenStore(path)
	if err != nil || !existed {
		t.Fatal(err)
	}
	list := reopened.List()
	if len(list) != 2 || list[0].ID != a.ID || list[1].ID != b.ID || list[1].EnvCredential != "CURSOR_API_KEY" {
		t.Fatalf("list %+v", list)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm %v", fi.Mode().Perm())
	}

	if err := st.Move(b.ID, -1); err != nil {
		t.Fatal(err)
	}
	if list := st.List(); list[0].ID != b.ID {
		t.Fatalf("after move %+v", list)
	}
	if err := st.Move(b.ID, -5); err != nil { // clamped, no change
		t.Fatal(err)
	}
	if err := st.Move("nope", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v", err)
	}
	if err := st.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Get(a.ID); ok || len(st.List()) != 1 {
		t.Fatal("not deleted")
	}
	if err := st.Delete(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v", err)
	}
}

func TestStoreCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subscriptions.json")
	_ = os.WriteFile(path, []byte("{nope"), 0o600)
	if _, _, err := OpenStore(path); err == nil {
		t.Fatal("expected a parse error")
	}
}

// The scenario from the feature request: five keys, four subscriptions.
func scenario(t *testing.T, st *Store) map[string]Subscription {
	t.Helper()
	subs := map[string]Subscription{}
	for _, s := range []struct {
		name, provider string
		keys           []string
	}{
		{"Claude (Personal)", "claude", []string{"k1", "k2", "k3"}},
		{"Cursor (Personal)", "cursor", []string{"k4", "k5", "k1"}},
		{"Claude (Company)", "claude", []string{"k4", "k5"}},
		{"Cursor (Friend)", "cursor", nil},
	} {
		sub, err := st.Create(s.name, s.provider, s.keys, "")
		if err != nil {
			t.Fatal(err)
		}
		subs[s.name] = sub
	}
	return subs
}

func ids(list []Subscription) []string {
	var out []string
	for _, s := range list {
		out = append(out, s.Name)
	}
	return out
}

func TestGrants(t *testing.T) {
	st, _ := openStore(t)
	subs := scenario(t, st)
	if got := ids(st.ForKey("k1")); !slices.Equal(got, []string{"Claude (Personal)", "Cursor (Personal)"}) {
		t.Fatalf("k1 %v", got)
	}
	if got := ids(st.ForKey("k4")); !slices.Equal(got, []string{"Cursor (Personal)", "Claude (Company)"}) {
		t.Fatalf("k4 %v", got)
	}
	if got := st.ForKey("k9"); len(got) != 0 {
		t.Fatalf("k9 %v", got)
	}

	friend := subs["Cursor (Friend)"].ID
	company := subs["Claude (Company)"].ID
	if err := st.SetKeyGrants("k5", []string{friend, company}); err != nil {
		t.Fatal(err)
	}
	if got := ids(st.ForKey("k5")); !slices.Equal(got, []string{"Claude (Company)", "Cursor (Friend)"}) {
		t.Fatalf("k5 %v", got)
	}
	if err := st.SetKeyGrants("k5", []string{"missing"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v", err)
	}

	// Disabled subscriptions keep their grants but are not used.
	off := true
	if _, err := st.Update(company, nil, &off, nil); err != nil {
		t.Fatal(err)
	}
	if got := ids(st.ForKey("k5")); !slices.Equal(got, []string{"Cursor (Friend)"}) {
		t.Fatalf("k5 with company disabled %v", got)
	}
	if got := st.GrantsOf("k5"); !slices.Equal(got, []string{company, friend}) {
		t.Fatalf("grants %v", got)
	}
	if err := st.SetKeyGrants("k5", nil); err != nil {
		t.Fatal(err)
	}
	if len(st.GrantsOf("k5")) != 0 {
		t.Fatal("grants not removed")
	}
}

// stub is a backend for routing tests; it never runs a CLI.
type stub struct {
	provider string
	models   []string
}

func (s *stub) Provider() string                             { return s.provider }
func (s *stub) Label() string                                { return strings.ToUpper(s.provider[:1]) + s.provider[1:] }
func (s *stub) Login(*agent.Account) agent.LoginSpec         { return agent.LoginSpec{} }
func (s *stub) Logout(context.Context, *agent.Account) error { return nil }
func (s *stub) ValidateCredential(string) error              { return nil }
func (s *stub) CredentialEnv() string                        { return strings.ToUpper(s.provider) + "_KEY" }
func (s *stub) Models() []string                             { return s.models }
func (s *stub) DefaultModel() string                         { return s.models[0] }
func (s *stub) Accepts(m string) bool                        { return slices.Contains(s.models, m) }
func (s *stub) Version(context.Context, []string) string     { return "1.0" }
func (s *stub) Run(context.Context, *agent.Account, agent.RunOptions, agent.Callbacks) (*agent.Result, error) {
	return &agent.Result{}, nil
}

func newManager(t *testing.T, strict bool) (*Manager, map[string]Subscription, string) {
	t.Helper()
	dir := t.TempDir()
	st, _, err := OpenStore(filepath.Join(dir, "subscriptions.json"))
	if err != nil {
		t.Fatal(err)
	}
	subs := scenario(t, st)
	m := NewManager(dir, st, &agent.EnvBuilder{}, Routing{Aliases: map[string]string{"gpt-4o": "opus"}, Strict: strict},
		&stub{"claude", []string{"sonnet", "opus", "haiku"}}, &stub{"cursor", []string{"auto", "gpt-5"}})
	return m, subs, dir
}

type cand struct{ name, model string }

func candidates(t *testing.T, m *Manager, key, model string) ([]cand, error) {
	t.Helper()
	list, err := m.Candidates(key, model)
	var out []cand
	for _, c := range list {
		out = append(out, cand{c.Sub.Name, c.Model})
	}
	return out, err
}

func TestCandidates(t *testing.T) {
	m, _, _ := newManager(t, false)
	cases := []struct {
		key, model string
		want       []cand
	}{
		// Subscriptions that know the model come first, then the others with
		// their default model.
		{"k1", "opus", []cand{{"Claude (Personal)", "opus"}, {"Cursor (Personal)", "auto"}}},
		{"k1", "gpt-5", []cand{{"Cursor (Personal)", "gpt-5"}, {"Claude (Personal)", "sonnet"}}},
		{"k1", "gpt-4o", []cand{{"Claude (Personal)", "opus"}, {"Cursor (Personal)", "auto"}}},
		{"k1", "", []cand{{"Claude (Personal)", "sonnet"}, {"Cursor (Personal)", "auto"}}},
		{"k4", "sonnet", []cand{{"Claude (Company)", "sonnet"}, {"Cursor (Personal)", "auto"}}},
		{"k2", "auto", []cand{{"Claude (Personal)", "sonnet"}}},
		// Pinned to one subscription by slug.
		{"k1", "cursor-personal/gpt-5", []cand{{"Cursor (Personal)", "gpt-5"}}},
		{"k1", "cursor-personal/", []cand{{"Cursor (Personal)", "auto"}}},
		{"k4", "claude-company/opus", []cand{{"Claude (Company)", "opus"}}},
		// A slash that is not a subscription is part of a model name.
		{"k1", "vendor/model", []cand{{"Claude (Personal)", "sonnet"}, {"Cursor (Personal)", "auto"}}},
	}
	for _, c := range cases {
		got, err := candidates(t, m, c.key, c.model)
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%s %q: got %v %v, want %v", c.key, c.model, got, err, c.want)
		}
	}

	var ce *agent.Error
	for _, bad := range []string{"-x", "a b", "--model=opus", strings.Repeat("m", 200)} {
		if _, err := m.Candidates("k1", bad); !errors.As(err, &ce) || ce.Kind != "invalid_model" || ce.Status != 400 {
			t.Errorf("model %q: %v", bad, err)
		}
	}
	if _, err := m.Candidates("k1", "cursor-personal/nope"); !errors.As(err, &ce) || ce.Status != 404 {
		t.Fatalf("pinned unknown model: %v", err)
	}
	if _, err := m.Candidates("k1", "claude-company/opus"); !errors.As(err, &ce) || ce.Status != 403 {
		t.Fatalf("pin to a subscription the key may not use: %v", err)
	}
	if _, err := m.Candidates("k9", "opus"); !errors.As(err, &ce) || ce.Kind != "forbidden" || !strings.Contains(ce.Message, "/admin") {
		t.Fatalf("key without subscriptions: %v", err)
	}
}

func TestCandidatesStrict(t *testing.T) {
	m, _, _ := newManager(t, true)
	got, err := candidates(t, m, "k1", "gpt-5")
	if err != nil || !slices.Equal(got, []cand{{"Cursor (Personal)", "gpt-5"}}) {
		t.Fatalf("strict known model: %v %v", got, err)
	}
	var ce *agent.Error
	if _, err := m.Candidates("k1", "llama"); !errors.As(err, &ce) || ce.Status != 404 {
		t.Fatalf("strict unknown model: %v", err)
	}
	if _, err := m.Candidates("k2", "gpt-5"); !errors.As(err, &ce) || ce.Status != 404 {
		t.Fatalf("strict model only another subscription knows: %v", err)
	}
}

func TestModelsForKey(t *testing.T) {
	m, _, _ := newManager(t, false)
	got := m.ModelsForKey("k1")
	want := []string{"sonnet", "opus", "haiku", "auto", "gpt-5", "gpt-4o",
		"claude-personal/sonnet", "claude-personal/opus", "claude-personal/haiku", "cursor-personal/auto", "cursor-personal/gpt-5"}
	if !slices.Equal(got, want) {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	if got := m.ModelsForKey("k9"); len(got) != 0 {
		t.Fatalf("no subscriptions, no models: %v", got)
	}
}

func TestAccountsAndDelete(t *testing.T) {
	m, subs, dir := newManager(t, false)
	t.Setenv("MY_ENV_KEY", "env-credential-123")
	sub, err := m.Store.Create("From Env", "cursor", nil, "MY_ENV_KEY")
	if err != nil {
		t.Fatal(err)
	}
	acc, b, err := m.Account(sub)
	if err != nil || b.Provider() != "cursor" {
		t.Fatal(err)
	}
	if tok, src := acc.Tokens.Token(); tok != "env-credential-123" || src != "env" {
		t.Fatalf("env credential %q %q", tok, src)
	}
	if acc.Home != filepath.Join(dir, "subscriptions", sub.ID, "home") {
		t.Fatalf("home %s", acc.Home)
	}
	if again, _, _ := m.Account(sub); again != acc {
		t.Fatal("accounts are cached")
	}
	if err := acc.Tokens.Save("saved-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "subscriptions", sub.ID, "credential")); err != nil {
		t.Fatal("credential not saved in the subscription directory")
	}
	if err := m.SignOut(context.Background(), sub.ID); err != nil {
		t.Fatal(err)
	}
	if _, src := acc.Tokens.Token(); src != "env" {
		t.Fatalf("after sign-out %q", src)
	}
	if err := m.Delete(sub.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "subscriptions", sub.ID)); !os.IsNotExist(err) {
		t.Fatal("subscription directory not removed")
	}
	if err := m.Delete("../etc"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("invalid id: %v", err)
	}
	if _, err := m.Create("Other", "openai", nil); err == nil {
		t.Fatal("unknown provider must be refused")
	}
	if _, _, err := m.Account(Subscription{ID: "x", Provider: "gemini"}); err == nil {
		t.Fatal("unknown provider account must fail")
	}
	if _, _, _, err := m.AccountByID("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err %v", err)
	}
	if m.ProbeModel(&stub{"claude", []string{"sonnet", "haiku"}}) != "haiku" || m.ProbeModel(&stub{"cursor", []string{"auto"}}) != "auto" {
		t.Fatal("probe model")
	}
	_ = subs
}

func TestBootstrapMigratesLegacyInstall(t *testing.T) {
	dir := t.TempDir()
	legacyToken := filepath.Join(dir, "claude-oauth-token")
	legacyHome := filepath.Join(dir, "claude-home")
	_ = os.WriteFile(legacyToken, []byte("sk-ant-oat01-legacy\n"), 0o600)
	_ = os.MkdirAll(filepath.Join(legacyHome, ".claude"), 0o700)
	st, _, _ := OpenStore(filepath.Join(dir, "subscriptions.json"))
	m := NewManager(dir, st, &agent.EnvBuilder{}, Routing{},
		&stub{"claude", []string{"sonnet"}}, &stub{"cursor", []string{"auto"}})
	if err := m.Bootstrap(Migration{KeyIDs: []string{"k1", "k2"}, LegacyToken: legacyToken, LegacyHome: legacyHome, CursorEnv: true}); err != nil {
		t.Fatal(err)
	}
	list := st.List()
	if len(list) != 2 || list[0].Provider != "claude" || list[1].Provider != "cursor" ||
		list[0].EnvCredential != "CLAUDE_CODE_OAUTH_TOKEN" || list[1].EnvCredential != "CURSOR_API_KEY" {
		t.Fatalf("list %+v", list)
	}
	if !slices.Equal(list[0].Keys, []string{"k1", "k2"}) || !slices.Equal(list[1].Keys, []string{"k1", "k2"}) {
		t.Fatalf("keys %+v", list)
	}
	acc, _, _ := m.Account(list[0])
	if tok, src := acc.Tokens.Token(); tok != "sk-ant-oat01-legacy" || src != "login" {
		t.Fatalf("legacy token %q %q", tok, src)
	}
	if _, err := os.Stat(filepath.Join(acc.Home, ".claude")); err != nil {
		t.Fatal("legacy CLI home not moved")
	}
	if _, err := os.Stat(legacyToken); !os.IsNotExist(err) {
		t.Fatal("legacy token file left behind")
	}
}

func TestBootstrapFreshInstall(t *testing.T) {
	dir := t.TempDir()
	st, _, _ := OpenStore(filepath.Join(dir, "subscriptions.json"))
	m := NewManager(dir, st, &agent.EnvBuilder{}, Routing{}, &stub{"claude", []string{"sonnet"}}, &stub{"cursor", []string{"auto"}})
	if err := m.Bootstrap(Migration{}); err != nil {
		t.Fatal(err)
	}
	if list := st.List(); len(list) != 1 || list[0].Name != "Claude" || len(list[0].Keys) != 0 {
		t.Fatalf("list %+v", list)
	}
	if len(m.Providers()) != 2 || m.Providers()[0].Provider() != "claude" {
		t.Fatal("providers")
	}
	if _, ok := m.Backend("cursor"); !ok {
		t.Fatal("backend lookup")
	}
	m.DetectVersions(context.Background())
	if m.Version("cursor") != "1.0" {
		t.Fatalf("version %q", m.Version("cursor"))
	}
}
