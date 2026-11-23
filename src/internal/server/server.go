// Package server wires configuration, stores, the claude runner and the HTTP
// handlers into a runnable server.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/atb00ker/CreditAPI/src/internal/admin"
	"github.com/atb00ker/CreditAPI/src/internal/claude"
	"github.com/atb00ker/CreditAPI/src/internal/config"
	"github.com/atb00ker/CreditAPI/src/internal/keys"
	"github.com/atb00ker/CreditAPI/src/internal/logging"
	"github.com/atb00ker/CreditAPI/src/internal/openai"
)

// Server is a fully wired CreditAPI instance.
type Server struct {
	Config  *config.Config
	Version string
	Keys    *keys.Store
	Tokens  *claude.TokenStore
	Status  *claude.Status
	Runner  *claude.Runner
	Login   *claude.LoginManager

	bridge  *claude.Bridge
	envb    *claude.EnvBuilder
	handler http.Handler
}

// New builds the server: data directories, key store, saved login, the MCP
// bridge for client tools, the claude runner and all HTTP routes.
func New(cfg *config.Config, version string) (*Server, error) {
	workDir := filepath.Join(cfg.DataDir, "workspace")
	for _, d := range []string{cfg.DataDir, cfg.ClaudeHome, workDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("create %s: %w", d, err)
		}
	}
	ks, err := keys.Open(filepath.Join(cfg.DataDir, "keys.json"))
	if err != nil {
		return nil, err
	}
	bridge, err := claude.NewBridge()
	if err != nil {
		return nil, fmt.Errorf("start MCP bridge: %w", err)
	}

	s := &Server{Config: cfg, Version: version, Keys: ks, bridge: bridge, Status: &claude.Status{}}
	s.Tokens = claude.NewTokenStore(claude.TokenPath(cfg.DataDir), cfg.EnvToken)
	s.envb = &claude.EnvBuilder{Home: cfg.ClaudeHome, Tokens: s.Tokens, Drop: config.OwnEnvVars}

	s.Runner = claude.NewRunner(cfg.MaxConcurrent)
	s.Runner.Bin = cfg.ClaudeBin
	s.Runner.WorkDir = workDir
	s.Runner.Bridge = bridge
	s.Runner.Tokens = s.Tokens
	s.Runner.Env = s.envb.ForRun
	s.Runner.Status = s.Status

	s.Login = claude.NewLoginManager(cfg.ClaudeBin, s.envb.ForLogin, s.Tokens)

	mux := http.NewServeMux()
	openai.NewHandler(cfg, ks, s.Runner).Register(mux)
	adm, err := admin.New(cfg, ks, s.Tokens, s.Login, s.Runner, s.Status)
	if err != nil {
		bridge.Close()
		return nil, err
	}
	adm.Register(mux)
	mux.HandleFunc("GET /healthz", s.healthz)

	s.handler = withRequestLogging(mux)
	return s, nil
}

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	_, source := s.Tokens.Token()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"ok":true,"claude_token":%t,"claude_login":%q}`+"\n",
		source != "", s.Status.Auth(s.Tokens.Version()).State)
}

// Start runs background work until ctx ends: CLI version detection and
// periodic saving of key usage counters.
func (s *Server) Start(ctx context.Context) {
	go func() {
		v := s.Status.DetectVersion(s.Config.ClaudeBin, s.envb.ForLogin())
		slog.Info(logging.Event("claude", "version", "detected"), slog.String("version", v))
	}()
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

// Close stops a running login, the MCP bridge and saves key usage.
func (s *Server) Close() error {
	s.Login.Cancel()
	_ = s.bridge.Close()
	return s.Keys.Flush()
}

// Run serves HTTP on HOST:PORT until ctx is cancelled, then shuts down.
func (s *Server) Run(ctx context.Context) error {
	cfg := s.Config
	_, source := s.Tokens.Token()
	if source == "" {
		slog.Warn(logging.Event("config", "token", "missing"), slog.String("hint", "open /login in a browser to sign in"))
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		slog.Warn(logging.Event("config", "api-key", "set"), slog.String("hint", "ANTHROPIC_API_KEY makes the claude CLI bill that key instead of your subscription"))
	}
	if cfg.PublicURL == "" {
		slog.Info(logging.Event("config", "public-url", "unset"), slog.String("hint", "login links in errors use the address the client called"))
	}

	srv := &http.Server{
		Addr:              cfg.Addr(),
		Handler:           s.handler,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	s.Start(ctx)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info(logging.Event("server", "start", "succeeded"), slog.String("version", s.Version),
		slog.String("addr", cfg.Addr()), slog.String("data_dir", cfg.DataDir), slog.String("default_model", cfg.DefaultModel),
		slog.Int("max_concurrent", cfg.MaxConcurrent), slog.String("token_source", source), slog.String("public_url", cfg.PublicURL))

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
		_ = srv.Shutdown(shutdownCtx)
	}
	return s.Close()
}
