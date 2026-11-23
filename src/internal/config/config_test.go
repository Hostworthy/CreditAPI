package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// setEnv clears every variable this package reads, then applies vars.
func setEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	for _, k := range OwnEnvVars {
		t.Setenv(k, "")
	}
	for k, v := range vars {
		t.Setenv(k, v)
	}
}

func TestLoadDefaults(t *testing.T) {
	setEnv(t, map[string]string{"ADMIN_PASSWORD": "long-enough", "DATA_DIR": t.TempDir()})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr() != "127.0.0.1:8080" || c.DefaultModel != "sonnet" || c.MaxConcurrent != 2 {
		t.Fatalf("defaults %+v", c)
	}
	if strings.Join(c.Models, ",") != "opus,sonnet,haiku" || c.ImageMaxDimension != 1568 || c.MaxImages != 20 {
		t.Fatalf("defaults %+v", c)
	}
	if c.QueueTimeout != 2*time.Minute || c.RequestTimeout != 10*time.Minute || c.MaxBodyBytes != 50<<20 {
		t.Fatalf("durations %+v", c)
	}
	if !c.AllowImageURLs || c.AllowPrivateImageURLs || c.StrictModels {
		t.Fatalf("flags %+v", c)
	}
	if !filepath.IsAbs(c.DataDir) || c.ClaudeBin != "claude" || c.CursorBin != "cursor-agent" {
		t.Fatalf("paths %+v", c)
	}
	if c.CursorDefaultModel != "auto" || strings.Join(c.CursorModels, ",") != "auto" {
		t.Fatalf("cursor defaults %+v", c)
	}
}

func TestLoadOverrides(t *testing.T) {
	setEnv(t, map[string]string{
		"ADMIN_PASSWORD": "long-enough", "HOST": "0.0.0.0", "PORT": "9000",
		"PUBLIC_URL": "https://claude.example.com/", "DEFAULT_MODEL": "haiku",
		"MODELS": " opus , haiku ,", "MODEL_ALIASES": "gpt-4o=sonnet, gpt-4o-mini = haiku",
		"STRICT_MODELS": "yes", "DEFAULT_EFFORT": "LOW", "MAX_CONCURRENT": "4",
		"QUEUE_TIMEOUT": "30", "REQUEST_TIMEOUT": "90s", "MAX_BODY_MB": "5",
		"ALLOW_IMAGE_URLS": "off", "ALLOW_PRIVATE_IMAGE_URLS": "1",
		"LOG_LEVEL": "DEBUG", "DATA_DIR": t.TempDir(), "CLAUDE_BIN": "./bin/fakeclaude", "CURSOR_BIN": "/opt/cursor/agent",
		"CURSOR_MODELS": "auto, gpt-5", "CURSOR_DEFAULT_MODEL": "gpt-5",
	})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr() != "0.0.0.0:9000" || c.PublicURL != "https://claude.example.com" || c.DefaultModel != "haiku" {
		t.Fatalf("%+v", c)
	}
	if strings.Join(c.Models, ",") != "opus,haiku" || c.ModelAliases["gpt-4o-mini"] != "haiku" || !c.StrictModels {
		t.Fatalf("%+v", c)
	}
	if c.DefaultEffort != "low" || c.MaxConcurrent != 4 || c.QueueTimeout != 30*time.Second || c.RequestTimeout != 90*time.Second {
		t.Fatalf("%+v", c)
	}
	if c.MaxBodyBytes != 5<<20 || c.AllowImageURLs || !c.AllowPrivateImageURLs || c.LogLevel != "debug" {
		t.Fatalf("%+v", c)
	}
	// Relative binary paths are made absolute because the CLIs run elsewhere.
	if !filepath.IsAbs(c.ClaudeBin) || !strings.HasSuffix(c.ClaudeBin, "/bin/fakeclaude") || c.CursorBin != "/opt/cursor/agent" {
		t.Fatalf("bins %q %q", c.ClaudeBin, c.CursorBin)
	}
	if strings.Join(c.CursorModels, ",") != "auto,gpt-5" || c.CursorDefaultModel != "gpt-5" {
		t.Fatalf("cursor %+v", c)
	}
}

func TestLoadValidation(t *testing.T) {
	cases := map[string]map[string]string{
		"ADMIN_PASSWORD must be set":        {},
		"at least 8 characters":             {"ADMIN_PASSWORD": "short"},
		"PORT must be a number":             {"ADMIN_PASSWORD": "long-enough", "PORT": "http"},
		"MAX_CONCURRENT must be at least 1": {"ADMIN_PASSWORD": "long-enough", "MAX_CONCURRENT": "0"},
		"MAX_CONCURRENT must be an integer": {"ADMIN_PASSWORD": "long-enough", "MAX_CONCURRENT": "two"},
		"PUBLIC_URL must start with http":   {"ADMIN_PASSWORD": "long-enough", "PUBLIC_URL": "claude.example.com"},
		"DEFAULT_EFFORT must be one of":     {"ADMIN_PASSWORD": "long-enough", "DEFAULT_EFFORT": "extreme"},
		"QUEUE_TIMEOUT must be a duration":  {"ADMIN_PASSWORD": "long-enough", "QUEUE_TIMEOUT": "soon"},
		"MODEL_ALIASES entry":               {"ADMIN_PASSWORD": "long-enough", "MODEL_ALIASES": "gpt-4o"},
		"LOG_LEVEL must be debug":           {"ADMIN_PASSWORD": "long-enough", "LOG_LEVEL": "loud"},
		"MAX_PENDING must be at least":      {"ADMIN_PASSWORD": "long-enough", "MAX_CONCURRENT": "3", "MAX_PENDING": "2"},
		"REQUEST_TIMEOUT must be positive":  {"ADMIN_PASSWORD": "long-enough", "REQUEST_TIMEOUT": "0"},
		"QUEUE_TIMEOUT must be positive":    {"ADMIN_PASSWORD": "long-enough", "QUEUE_TIMEOUT": "-5s"},
		"MAX_BODY_MB must be at least 1":    {"ADMIN_PASSWORD": "long-enough", "MAX_BODY_MB": "0"},
		"MAX_IMAGES must not be negative":   {"ADMIN_PASSWORD": "long-enough", "MAX_IMAGES": "-1"},
		"MODELS must list":                  {"ADMIN_PASSWORD": "long-enough", "MODELS": " , "},
		"invalid model name":                {"ADMIN_PASSWORD": "long-enough", "MODEL_ALIASES": "gpt 4o=sonnet"},
	}
	for want, vars := range cases {
		t.Run(want, func(t *testing.T) {
			setEnv(t, vars)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("got %v, want error containing %q", err, want)
			}
		})
	}
}

func TestExamplePasswordIsRefused(t *testing.T) {
	setEnv(t, map[string]string{"ADMIN_PASSWORD": "Change-Me-To-Something-Long", "DATA_DIR": t.TempDir()})
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "example value") {
		t.Fatalf("got %v", err)
	}
}

func TestImageMaxPixels(t *testing.T) {
	setEnv(t, map[string]string{"ADMIN_PASSWORD": "long-enough", "DATA_DIR": t.TempDir()})
	c, err := Load()
	if err != nil || c.ImageMaxPixels != 40_000_000 {
		t.Fatalf("default %d %v", c.ImageMaxPixels, err)
	}
	t.Setenv("IMAGE_MAX_PIXELS", "1000")
	if c, _ = Load(); c.ImageMaxPixels != 1000 {
		t.Fatalf("override %d", c.ImageMaxPixels)
	}
}

func TestOwnEnvVarsIncludeCredentials(t *testing.T) {
	// Credentials are passed per subscription, never inherited by every CLI.
	for _, k := range []string{"ADMIN_PASSWORD", "CLAUDE_CODE_OAUTH_TOKEN", "CURSOR_API_KEY", "ANTHROPIC_API_KEY"} {
		found := false
		for _, v := range OwnEnvVars {
			found = found || v == k
		}
		if !found {
			t.Errorf("%s is not in OwnEnvVars", k)
		}
	}
}

func TestDotEnvChain(t *testing.T) {
	dir := t.TempDir()
	secrets := filepath.Join(dir, "secrets.env")
	entry := filepath.Join(dir, "entry.env")
	if err := os.WriteFile(entry, []byte("CMTA_TEST_A=from-entry\nCMTA_TEST_B=from-entry\nENV_PATH=secrets.env\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secrets, []byte("CMTA_TEST_B=from-secrets\nCMTA_TEST_C=from-secrets\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENV_CREDITAPI", entry)
	t.Setenv("CMTA_TEST_C", "from-shell")
	for _, k := range []string{"CMTA_TEST_A", "CMTA_TEST_B"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	if err := LoadDotEnv(); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CMTA_TEST_A") != "from-entry" || os.Getenv("CMTA_TEST_B") != "from-secrets" || os.Getenv("CMTA_TEST_C") != "from-shell" {
		t.Fatalf("A=%q B=%q C=%q", os.Getenv("CMTA_TEST_A"), os.Getenv("CMTA_TEST_B"), os.Getenv("CMTA_TEST_C"))
	}
	if _, ok := os.LookupEnv("ENV_PATH"); ok {
		t.Fatal("ENV_PATH must not leak into the environment")
	}
}

func TestDotEnvMissing(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("ENV_CREDITAPI", "")
	if err := LoadDotEnv(); err != nil {
		t.Fatalf("missing default .env should be fine: %v", err)
	}
	t.Setenv("ENV_CREDITAPI", "does-not-exist.env")
	if err := LoadDotEnv(); err == nil {
		t.Fatal("explicit missing env file should fail")
	}
}

func TestDotEnvLoop(t *testing.T) {
	dir := t.TempDir()
	loop := filepath.Join(dir, "loop.env")
	if err := os.WriteFile(loop, []byte("ENV_PATH=loop.env\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENV_CREDITAPI", loop)
	if err := LoadDotEnv(); err == nil || !strings.Contains(err.Error(), "maximum depth") {
		t.Fatalf("expected depth error, got %v", err)
	}
}
