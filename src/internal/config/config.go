// Package config loads server settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds every runtime setting. All values come from the environment
// so the server can be configured entirely through Docker.
type Config struct {
	Host          string
	Port          string
	DataDir       string
	AdminPassword string
	// PublicURL is how users reach this server (e.g. https://claude.example.com).
	// It is used for the login link in "login required" errors.
	PublicURL string

	ClaudeBin string

	// DefaultModel and Models are the Claude defaults and advertised models.
	DefaultModel string
	Models       []string

	CursorBin          string
	CursorModels       []string
	CursorDefaultModel string

	ModelAliases  map[string]string
	StrictModels  bool
	DefaultEffort string

	DefaultSystemPrompt string
	MaxConcurrent       int
	QueueTimeout        time.Duration
	RequestTimeout      time.Duration

	ImageMaxDimension     int
	MaxImages             int
	AllowImageURLs        bool
	AllowPrivateImageURLs bool
	MaxBodyBytes          int64

	LogLevel string
}

// OwnEnvVars are consumed by this server and never forwarded to the CLI
// subprocesses (credentials are passed per subscription).
var OwnEnvVars = []string{
	"ADMIN_PASSWORD", "HOST", "PORT", "DATA_DIR", "ENV_CREDITAPI", "ENV_PATH", "PUBLIC_URL",
	"CLAUDE_BIN", "DEFAULT_MODEL", "MODELS", "CURSOR_BIN", "CURSOR_MODELS", "CURSOR_DEFAULT_MODEL",
	"MODEL_ALIASES", "STRICT_MODELS", "DEFAULT_EFFORT",
	"DEFAULT_SYSTEM_PROMPT", "MAX_CONCURRENT", "QUEUE_TIMEOUT", "REQUEST_TIMEOUT",
	"IMAGE_MAX_DIMENSION", "MAX_IMAGES", "ALLOW_IMAGE_URLS",
	"ALLOW_PRIVATE_IMAGE_URLS", "MAX_BODY_MB", "LOG_LEVEL",
	"CLAUDE_CODE_OAUTH_TOKEN", "CURSOR_API_KEY", "ANTHROPIC_API_KEY",
}

// Load reads the configuration from the environment.
func Load() (*Config, error) {
	c := &Config{
		Host:                  env("HOST", "127.0.0.1"),
		Port:                  env("PORT", "8080"),
		DataDir:               env("DATA_DIR", "./data"),
		AdminPassword:         os.Getenv("ADMIN_PASSWORD"),
		PublicURL:             strings.TrimRight(strings.TrimSpace(os.Getenv("PUBLIC_URL")), "/"),
		ClaudeBin:             env("CLAUDE_BIN", "claude"),
		DefaultModel:          env("DEFAULT_MODEL", "sonnet"),
		Models:                splitList(env("MODELS", "opus,sonnet,haiku")),
		CursorBin:             env("CURSOR_BIN", "cursor-agent"),
		CursorModels:          splitList(env("CURSOR_MODELS", "auto")),
		CursorDefaultModel:    env("CURSOR_DEFAULT_MODEL", "auto"),
		StrictModels:          envBool("STRICT_MODELS", false),
		DefaultEffort:         strings.ToLower(os.Getenv("DEFAULT_EFFORT")),
		DefaultSystemPrompt:   env("DEFAULT_SYSTEM_PROMPT", "You are a helpful assistant."),
		AllowImageURLs:        envBool("ALLOW_IMAGE_URLS", true),
		AllowPrivateImageURLs: envBool("ALLOW_PRIVATE_IMAGE_URLS", false),
		LogLevel:              strings.ToLower(env("LOG_LEVEL", "info")),
	}
	var err error
	if c.MaxConcurrent, err = envInt("MAX_CONCURRENT", 2); err != nil {
		return nil, err
	}
	if c.ImageMaxDimension, err = envInt("IMAGE_MAX_DIMENSION", 1568); err != nil {
		return nil, err
	}
	if c.MaxImages, err = envInt("MAX_IMAGES", 20); err != nil {
		return nil, err
	}
	maxBodyMB, err := envInt("MAX_BODY_MB", 50)
	if err != nil {
		return nil, err
	}
	c.MaxBodyBytes = int64(maxBodyMB) << 20
	if c.QueueTimeout, err = envDuration("QUEUE_TIMEOUT", 2*time.Minute); err != nil {
		return nil, err
	}
	if c.RequestTimeout, err = envDuration("REQUEST_TIMEOUT", 10*time.Minute); err != nil {
		return nil, err
	}
	if c.ModelAliases, err = parseAliases(os.Getenv("MODEL_ALIASES")); err != nil {
		return nil, err
	}

	if c.AdminPassword == "" {
		return nil, errors.New("ADMIN_PASSWORD must be set (it protects /admin and /login)")
	}
	if len(c.AdminPassword) < 8 {
		return nil, errors.New("ADMIN_PASSWORD must be at least 8 characters")
	}
	if p, perr := strconv.Atoi(c.Port); perr != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("PORT must be a number between 1 and 65535 (got %q)", c.Port)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "warning", "error":
	default:
		return nil, errors.New("LOG_LEVEL must be debug, info, warn, or error")
	}
	if c.MaxConcurrent < 1 {
		return nil, errors.New("MAX_CONCURRENT must be at least 1")
	}
	if c.PublicURL != "" && !strings.HasPrefix(c.PublicURL, "http://") && !strings.HasPrefix(c.PublicURL, "https://") {
		return nil, errors.New("PUBLIC_URL must start with http:// or https://")
	}
	switch c.DefaultEffort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		return nil, fmt.Errorf("DEFAULT_EFFORT must be one of low, medium, high, xhigh, max (got %q)", c.DefaultEffort)
	}

	if c.DataDir, err = filepath.Abs(c.DataDir); err != nil {
		return nil, err
	}
	// The CLIs run in their own working directories, so relative paths such
	// as ./bin/fakeclaude must be made absolute here.
	for _, bin := range []*string{&c.ClaudeBin, &c.CursorBin} {
		if strings.ContainsRune(*bin, filepath.Separator) && !filepath.IsAbs(*bin) {
			if *bin, err = filepath.Abs(*bin); err != nil {
				return nil, err
			}
		}
	}
	return c, nil
}

// Addr is the listen address built from HOST and PORT.
func (c *Config) Addr() string { return net.JoinHostPort(c.Host, c.Port) }

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch v {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration like 90s or 5m: %w", key, err)
	}
	return d, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// parseAliases parses "gpt-4o=sonnet,gpt-4o-mini=haiku".
func parseAliases(s string) (map[string]string, error) {
	m := map[string]string{}
	for _, pair := range splitList(s) {
		k, v, ok := strings.Cut(pair, "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("MODEL_ALIASES entry %q must look like name=model", pair)
		}
		m[k] = v
	}
	return m, nil
}
