// Command claude-max-to-api exposes an OpenAI-compatible API backed by the
// Claude Code CLI (`claude -p`) running on a Claude subscription.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/atb00ker/claude-max-to-api/internal/admin"
	"github.com/atb00ker/claude-max-to-api/internal/claude"
	"github.com/atb00ker/claude-max-to-api/internal/config"
	"github.com/atb00ker/claude-max-to-api/internal/keys"
	"github.com/atb00ker/claude-max-to-api/internal/openai"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "-v", "--version", "version":
			fmt.Println(version)
			return
		case "healthcheck":
			os.Exit(healthcheck())
		}
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// healthcheck probes /healthz on the configured port (used by Docker).
func healthcheck() int {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		port = "8080"
	}
	client := &http.Client{Timeout: 4 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

	workDir := filepath.Join(cfg.DataDir, "workspace")
	for _, d := range []string{cfg.DataDir, cfg.ClaudeHome, workDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", d, err)
		}
	}

	ks, err := keys.Open(filepath.Join(cfg.DataDir, "keys.json"))
	if err != nil {
		return err
	}
	tokens := claude.NewTokenStore(claude.TokenPath(cfg.DataDir), cfg.EnvToken)
	envb := &claude.EnvBuilder{Home: cfg.ClaudeHome, Tokens: tokens, Drop: config.OwnEnvVars}
	status := &claude.Status{}

	bridge, err := claude.NewBridge()
	if err != nil {
		return fmt.Errorf("start MCP bridge: %w", err)
	}

	runner := claude.NewRunner(cfg.MaxConcurrent)
	runner.Bin = cfg.ClaudeBin
	runner.WorkDir = workDir
	runner.Bridge = bridge
	runner.Tokens = tokens
	runner.Env = envb.ForRun
	runner.Status = status

	login := claude.NewLoginManager(cfg.ClaudeBin, envb.ForLogin, tokens)

	mux := http.NewServeMux()
	openai.NewHandler(cfg, ks, runner).Register(mux)
	adm, err := admin.New(cfg, ks, tokens, login, runner, status)
	if err != nil {
		return err
	}
	adm.Register(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, source := tokens.Token()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"claude_token":%t,"claude_login":%q}`+"\n",
			source != "", status.Auth(tokens.Version()).State)
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		v := status.DetectVersion(cfg.ClaudeBin, envb.ForLogin())
		slog.Info("claude CLI detected", "version", v)
	}()

	_, source := tokens.Token()
	if source == "" {
		slog.Warn("no Claude token configured yet: open /login in a browser to sign in")
	}
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		slog.Warn("ANTHROPIC_API_KEY is set; the claude CLI will bill that API key instead of your subscription")
	}
	slog.Info("starting claude-max-to-api", "version", version, "listen", cfg.ListenAddr, "data_dir", cfg.DataDir,
		"default_model", cfg.DefaultModel, "max_concurrent", cfg.MaxConcurrent, "token_source", source,
		"public_url", cfg.PublicURL)
	if cfg.PublicURL == "" {
		slog.Info("PUBLIC_URL is not set: login links in error messages will use the address the client called")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				if err := ks.Flush(); err != nil {
					slog.Error("save key usage", "err", err)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		slog.Info("shutting down")
		login.Cancel()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}
	return ks.Flush()
}
