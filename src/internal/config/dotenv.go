package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"
)

const (
	envPathVar      = "ENV_CREDITAPI"
	envPathKey      = "ENV_PATH"
	maxEnvPathDepth = 10
	defaultEnvPath  = ".env"
)

// LoadDotEnv loads the project .env chain into the process environment.
// The entry file is shell ENV_CREDITAPI when set, otherwise .env
// (optional). Each file may chain to another via ENV_PATH (up to 10 hops).
// Variables already set in the process environment win.
func LoadDotEnv() error {
	start, optional := resolveEnvStart()
	return loadEnvChain(start, optional)
}

func resolveEnvStart() (string, bool) {
	if path := strings.TrimSpace(os.Getenv(envPathVar)); path != "" {
		return path, false
	}
	return defaultEnvPath, true
}

func loadEnvChain(start string, optionalStart bool) error {
	merged := make(map[string]string)
	current := start
	currentDir, err := filepath.Abs(".")
	if err != nil {
		return err
	}

	for depth := 0; depth < maxEnvPathDepth; depth++ {
		resolved, err := resolveEnvFilePath(current, currentDir)
		if err != nil {
			return err
		}

		values, err := godotenv.Read(resolved)
		if err != nil {
			if os.IsNotExist(err) {
				if optionalStart && depth == 0 {
					return nil
				}
				return fmt.Errorf("env file %q: %w", resolved, err)
			}
			return err
		}

		for key, value := range values {
			if key != envPathKey {
				merged[key] = value
			}
		}

		hop := strings.TrimSpace(values[envPathKey])
		if hop == "" {
			applyMerged(merged)
			return nil
		}

		currentDir = filepath.Dir(resolved)
		current = hop
		optionalStart = false
	}

	return fmt.Errorf("ENV_PATH chain exceeds maximum depth of %d", maxEnvPathDepth)
}

func resolveEnvFilePath(path, baseDir string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("env file path is empty")
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Join(baseDir, path), nil
}

func applyMerged(merged map[string]string) {
	for key, value := range merged {
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		_ = os.Setenv(key, value)
	}
}
