// Package server wires configuration, stores, subscriptions, the AI backends
// and the HTTP handlers into a runnable server.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/atb00ker/CreditAPI/src/internal/admin"
	"github.com/atb00ker/CreditAPI/src/internal/agent"
	"github.com/atb00ker/CreditAPI/src/internal/agent/claude"
	"github.com/atb00ker/CreditAPI/src/internal/agent/cursor"
	"github.com/atb00ker/CreditAPI/src/internal/config"
	"github.com/atb00ker/CreditAPI/src/internal/keys"
	"github.com/atb00ker/CreditAPI/src/internal/logging"
	"github.com/atb00ker/CreditAPI/src/internal/openai"
	"github.com/atb00ker/CreditAPI/src/internal/subscriptions"
)

// Server is a fully wired CreditAPI instance.
type Server struct {
	Config  *config.Config
	Version string
	Keys    *keys.Store
	Subs    *subscriptions.Manager
	Slots   *agent.Slots

	bridge  *agent.Bridge
	admin   *admin.Admin
	handler http.Handler
}

// New builds the server: data directories, key store, subscriptions (with a
// first-run migration from the single-subscription layout), the MCP bridge
// for client tools, the Claude and Cursor backends and all HTTP routes.
func New(cfg *config.Config, version string) (*Server, error) {
	logging.RegisterSecret(cfg.AdminPassword)
	workDir := filepath.Join(cfg.DataDir, "workspace")
	for _, d := range []string{cfg.DataDir, workDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}
	ks, err := keys.Open(filepath.Join(cfg.DataDir, "keys.json"))
	if err != nil {
		return nil, err
	}
	store, existed, err := subscriptions.OpenStore(filepath.Join(cfg.DataDir, "subscriptions.json"))
	if err != nil {
		return nil, err
	}
	bridge, err := agent.NewBridge()
	if err != nil {
		return nil, fmt.Errorf("start MCP bridge: %w", err)
	}

	env := &agent.EnvBuilder{Drop: config.OwnEnvVars, Defaults: map[string]string{
		"DISABLE_AUTOUPDATER":                      "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"ENABLE_TOOL_SEARCH":                       "false",
	}}
	claudeBackend := &claude.Backend{Bin: cfg.ClaudeBin, WorkDir: workDir, Bridge: bridge, Default: cfg.DefaultModel, List: cfg.Models}
	cursorBackend := &cursor.Backend{Bin: cfg.CursorBin, Bridge: bridge, Default: cfg.CursorDefaultModel, List: cfg.CursorModels,
		// Claude's per-request files (prompts, bridge addresses) live in its
		// own run root; Cursor's tools must not read them.
		Protected: []string{cfg.DataDir, agent.RunRootPath(claude.Provider)}}
	subs := subscriptions.NewManager(cfg.DataDir, store, env,
		subscriptions.Routing{Aliases: cfg.ModelAliases, Strict: cfg.StrictModels}, claudeBackend, cursorBackend)
	if !existed {
		var keyIDs []string
		for _, k := range ks.List() {
			keyIDs = append(keyIDs, k.ID)
		}
		if err := subs.Bootstrap(subscriptions.Migration{
			KeyIDs:      keyIDs,
			LegacyToken: filepath.Join(cfg.DataDir, "claude-oauth-token"),
			LegacyHome:  filepath.Join(cfg.DataDir, "claude-home"),
			CursorEnv:   os.Getenv("CURSOR_API_KEY") != "",
		}); err != nil {
			_ = bridge.Close()
			return nil, fmt.Errorf("create first subscriptions: %w", err)
		}
	}

	known := map[string]bool{}
	for _, k := range ks.List() {
		known[k.ID] = true
	}
	if n, err := store.PruneKeys(known); err != nil {
		_ = bridge.Close()
		return nil, fmt.Errorf("clean up subscription grants: %w", err)
	} else if n > 0 {
		slog.Warn(logging.Event("subscription", "grants", "pruned"), slog.Int("removed", n))
	}

	s := &Server{Config: cfg, Version: version, Keys: ks, Subs: subs, Slots: agent.NewSlots(cfg.MaxConcurrent), bridge: bridge}
	mux := http.NewServeMux()
	openai.NewHandler(cfg, ks, subs, s.Slots).Register(mux)
	adm, err := admin.New(cfg, ks, subs, s.Slots)
	if err != nil {
		_ = bridge.Close()
		return nil, err
	}
	adm.Register(mux)
	s.admin = adm
	mux.HandleFunc("GET /healthz", s.healthz)

	s.handler = withRequestLogging(mux)
	return s, nil
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// healthz reports liveness and whether any enabled subscription is known to
// need a sign-in. The per-subscription list (names included) is shown only
// to the admin (session cookie or bearer password).
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	type subHealth struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Provider string `json:"provider"`
		Login    string `json:"login"`
	}
	list := []subHealth{}
	ok := true
	needed := 0
	for _, sub := range s.Subs.Store.List() {
		if sub.Disabled {
			continue
		}
		state := "unknown"
		if acc, _, err := s.Subs.Account(sub); err == nil {
			state = acc.Status.Auth(acc.Tokens.Version()).State
		}
		if state == "login_required" {
			ok = false
			needed++
		}
		list = append(list, subHealth{ID: sub.ID, Name: sub.Name, Provider: sub.Provider, Login: state})
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	out := map[string]any{"ok": true, "logins_ok": ok, "subscription_count": len(list), "logins_needed": needed}
	if s.admin != nil && s.admin.Authorized(r) {
		out["subscriptions"] = list
	}
	_ = json.NewEncoder(w).Encode(out)
}

// Start runs background work until ctx ends: CLI version detection and
// periodic saving of key usage counters.
func (s *Server) Start(ctx context.Context) {
	go s.Subs.DetectVersions(ctx)
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := s.Keys.Flush(); err != nil {
					slog.Error(logging.Event("key", "usage", "save-failed"), logging.Err(err))
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Close stops a running sign-in, the MCP bridge and saves key usage.
func (s *Server) Close() error {
	s.Subs.Login.CancelAll()
	_ = s.bridge.Close()
	return s.Keys.Flush()
}

// Run serves HTTP on HOST:PORT until ctx is cancelled, then shuts down.
func (s *Server) Run(ctx context.Context) error {
	cfg := s.Config
	signedIn := 0
	for _, sub := range s.Subs.Store.List() {
		if acc, _, err := s.Subs.Account(sub); err == nil {
			if tok, _ := acc.Tokens.Token(); tok != "" {
				signedIn++
			}
		}
	}
	if signedIn == 0 {
		slog.Warn(logging.Event("config", "credential", "missing"), slog.String("hint", "open /login in a browser to sign in"))
	}
	if len(cfg.AdminPassword) < 12 {
		slog.Warn(logging.Event("config", "admin-password", "short"), slog.String("hint", "use at least 12 characters for ADMIN_PASSWORD"))
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		slog.Warn(logging.Event("config", "api-key", "ignored"), slog.String("hint", "ANTHROPIC_API_KEY is not passed to the claude CLI; requests use the subscriptions"))
	}
	if cfg.PublicURL == "" {
		slog.Info(logging.Event("config", "public-url", "unset"), slog.String("hint", "login links in errors use the address the client called"))
	}

	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	srv := &http.Server{
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
		Addr:              cfg.Addr(),
		Handler:           s.handler,
		ReadHeaderTimeout: 15 * time.Second,
		// Bounds slow request bodies. No WriteTimeout: it would cut streams.
		ReadTimeout:    5 * time.Minute,
		MaxHeaderBytes: 64 << 10,
		// Connection and TLS errors go to the JSON log too.
		ErrorLog:    slog.NewLogLogger(slog.Default().Handler(), slog.LevelWarn),
		IdleTimeout: 120 * time.Second,
	}
	s.Start(ctx)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info(logging.Event("server", "start", "succeeded"), slog.String("version", s.Version),
		slog.String("addr", cfg.Addr()), slog.String("data_dir", cfg.DataDir), slog.String("default_model", cfg.DefaultModel),
		slog.Int("max_concurrent", cfg.MaxConcurrent), slog.Int("subscriptions", len(s.Subs.Store.List())), slog.String("public_url", cfg.PublicURL))

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			_ = s.Close()
			return err
		}
	case <-ctx.Done():
		slog.Info(logging.Event("server", "shutdown", "started"))
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			// Requests still running after the grace period: cancel them,
			// which kills their CLI process groups, and close connections.
			slog.Warn(logging.Event("server", "shutdown", "forced"), logging.Err(err))
		}
		cancelBase()
		_ = srv.Close()
	}
	return s.Close()
}
