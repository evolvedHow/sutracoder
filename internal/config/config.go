// Package config loads Sutracoder configuration from defaults, an optional
// TOML file, and SUTRACODER_* environment variables. Precedence is
// flags > env > file > defaults (flags are applied by the caller afterward).
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Provider names.
const (
	// ProviderLlamaCPP is the default: the user's llama.cpp server (llama-server)
	// on the Windows host at http://127.0.0.1:8080/v1, reachable from WSL via
	// mirrored networking. It speaks the OpenAI-compatible API and needs no key.
	ProviderLlamaCPP  = "llamacpp"
	ProviderMarga     = "marga"
	ProviderOpenAI    = "openai"
	ProviderAnthropic = "anthropic"
	ProviderOllama    = "ollama"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	// Provider is the selected LLM provider name.
	Provider string `toml:"provider"`
	// Model overrides the provider's default model when non-empty.
	Model string `toml:"model"`
	// BaseURL overrides the provider's default endpoint when non-empty.
	BaseURL string `toml:"base_url"`
	// Mode is "autonomous" or "supervised".
	Mode string `toml:"mode"`
	// MaxTurns caps agent turns before the loop halts.
	MaxTurns int `toml:"max_turns"`
	// MaxTokensBudget caps cumulative tokens across a session.
	MaxTokensBudget int `toml:"max_tokens_budget"`
	// MaxToolOutputBytes caps every tool's output.
	MaxToolOutputBytes int `toml:"max_tool_output_bytes"`
	// ShellTimeoutSec bounds execute_shell.
	ShellTimeoutSec int `toml:"shell_timeout_sec"`
	// ContextLimit is the model's context window in tokens (for windowing).
	ContextLimit int `toml:"context_limit"`
	// Temperature controls sampling (0 = provider default).
	Temperature float64 `toml:"temperature"`
	// Workspace is the default workspace root.
	Workspace string `toml:"workspace"`
	// DataDir holds the SQLite DB, diagnostics log, and interaction logs.
	DataDir string `toml:"data_dir"`
	// ConfigPath is the resolved path of the loaded config file (empty if none).
	ConfigPath string `toml:"-"`
}

// Defaults returns the built-in configuration.
func Defaults() Config {
	return Config{
		Provider:           ProviderLlamaCPP,
		Mode:               "autonomous",
		MaxTurns:           50,
		MaxTokensBudget:    200_000,
		MaxToolOutputBytes: 10 * 1024,
		ShellTimeoutSec:    120,
		ContextLimit:       128_000,
		Workspace:          ".",
	}
}

// Load resolves configuration: defaults → TOML file → env. A missing config
// file is not an error. Returns the resolved config and its source path.
func Load(explicitPath string) (Config, error) {
	cfg := Defaults()

	path := explicitPath
	if path == "" {
		if p, err := defaultConfigPath(); err == nil {
			path = p
		}
	}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if err := toml.Unmarshal(data, &cfg); err != nil {
				return cfg, fmt.Errorf("parse config %s: %w", path, err)
			}
			cfg.ConfigPath = path
		} else if explicitPath != "" {
			// An explicitly-requested config that is missing is an error.
			return cfg, fmt.Errorf("read config %s: %w", path, err)
		}
	}

	applyEnv(&cfg)

	if cfg.DataDir == "" {
		cfg.DataDir = defaultDataDir()
	}
	if cfg.Workspace == "" {
		cfg.Workspace = "."
	}
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// applyEnv overlays SUTRACODER_* environment variables onto cfg.
func applyEnv(cfg *Config) {
	set := func(key string, fn func(string)) {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			fn(v)
		}
	}
	set("SUTRACODER_PROVIDER", func(v string) { cfg.Provider = v })
	set("SUTRACODER_MODEL", func(v string) { cfg.Model = v })
	set("SUTRACODER_BASE_URL", func(v string) { cfg.BaseURL = v })
	set("SUTRACODER_MODE", func(v string) { cfg.Mode = v })
	set("SUTRACODER_WORKSPACE", func(v string) { cfg.Workspace = v })
	set("SUTRACODER_DATA_DIR", func(v string) { cfg.DataDir = v })
	setInt("SUTRACODER_MAX_TURNS", &cfg.MaxTurns)
	setInt("SUTRACODER_MAX_TOKENS_BUDGET", &cfg.MaxTokensBudget)
	setInt("SUTRACODER_MAX_TOOL_OUTPUT_BYTES", &cfg.MaxToolOutputBytes)
	setInt("SUTRACODER_SHELL_TIMEOUT_SEC", &cfg.ShellTimeoutSec)
	setInt("SUTRACODER_CONTEXT_LIMIT", &cfg.ContextLimit)
	set("SUTRACODER_TEMPERATURE", func(v string) {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Temperature = f
		}
	})
}

func setInt(key string, dst *int) {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			*dst = n
		}
	}
}

// Validate checks config invariants.
func (c Config) Validate() error {
	switch c.Provider {
	case ProviderLlamaCPP, ProviderMarga, ProviderOpenAI, ProviderAnthropic, ProviderOllama:
	default:
		return fmt.Errorf("unknown provider %q (want llamacpp, marga, openai, anthropic, or ollama)", c.Provider)
	}
	switch c.Mode {
	case "autonomous", "supervised":
	default:
		return fmt.Errorf("unknown mode %q (want autonomous or supervised)", c.Mode)
	}
	return nil
}

// DBPath returns the SQLite database path within the data dir.
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "sutracoder.db") }

// LogPath returns the diagnostics log path within the data dir.
func (c Config) LogPath() string { return filepath.Join(c.DataDir, "sutracoder.log") }

// InteractionsDir returns the JSONL interaction-log directory.
func (c Config) InteractionsDir() string { return filepath.Join(c.DataDir, "interactions") }

func defaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "sutracoder", "config.toml"), nil
}

func defaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".sutracoder"
	}
	return filepath.Join(home, ".local", "share", "sutracoder")
}
