// Package config loads and validates the gateway YAML configuration.
//
// v1 loads configuration once at startup; there is no hot reload. The schema
// contract lives in specs/001-multichain-rpc-routing/contracts/config-contract.md
// and config.example.yaml documents every field.
package config

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root gateway configuration.
type Config struct {
	Server  Server  `yaml:"server"`
	Prober  Prober  `yaml:"prober"`
	Retry   Retry   `yaml:"retry"`
	Circuit Circuit `yaml:"circuit"`
	Chains  []Chain `yaml:"chains"`
}

// Server holds listen addresses and request limits.
type Server struct {
	Listen           string         `yaml:"listen"`             // required
	MetricsListen    string         `yaml:"metrics_listen"`     // required
	MaxBatchElements int            `yaml:"max_batch_elements"` // optional; default 100
	MaxBodyBytes     int64          `yaml:"max_body_bytes"`     // optional; default 1 MiB
	Timeouts         map[string]int `yaml:"timeouts"`           // optional; per-method-class seconds
}

// Prober configures active upstream health probing.
type Prober struct {
	Interval      Duration `yaml:"interval"`       // default 10s
	Timeout       Duration `yaml:"timeout"`        // default 5s
	FailThreshold int      `yaml:"fail_threshold"` // default 3
}

// Retry configures safe-method retry behaviour.
type Retry struct {
	MaxAttempts int      `yaml:"max_attempts"` // default 2 (includes first attempt)
	BaseDelay   Duration `yaml:"base_delay"`   // default 10ms
	MaxElapsed  Duration `yaml:"max_elapsed"`  // default 30s
}

// Circuit configures per-upstream circuit breaking.
type Circuit struct {
	FailThreshold int      `yaml:"fail_threshold"` // default 5
	Cooldown      Duration `yaml:"cooldown"`       // default 30s
}

// Chain describes one configured chain and its upstream endpoints.
type Chain struct {
	ChainID   string     `yaml:"chain_id"`  // required; unique decimal string
	Adapter   string     `yaml:"adapter"`   // required; adapter name (checked in main)
	Upstreams []Upstream `yaml:"upstreams"` // required; at least one
}

// Upstream is a configured RPC endpoint for a chain.
type Upstream struct {
	Name string `yaml:"name"` // optional; log/metric alias
	URL  string `yaml:"url"`  // required; http/https
}

// Duration parses Go duration strings ("10s", "10ms") from YAML.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler for Duration.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var raw string
	if err := value.Decode(&raw); err != nil {
		return fmt.Errorf("duration must be a string like \"10s\": %w", err)
	}
	parsed, err := time.ParseDuration(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	if parsed < 0 {
		return fmt.Errorf("duration %q must not be negative", raw)
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the underlying time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

var envVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load reads the config file at path, substitutes ${VAR} placeholders from the
// environment (fail-fast when unset), strict-parses the YAML, applies defaults,
// and validates the result.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	substituted, err := substituteEnv(raw)
	if err != nil {
		return nil, err
	}

	cfg := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader(substituted))
	dec.KnownFields(true) // reject unknown fields per contract
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyDefaults(cfg)
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// substituteEnv replaces ${VAR} placeholders with their environment values.
// os.ExpandEnv is deliberately not used: it would also corrupt literal "$"
// sequences that are not gateway placeholders. An unset variable is an error.
func substituteEnv(raw []byte) ([]byte, error) {
	var firstErr error
	out := envVarRe.ReplaceAllFunc(raw, func(m []byte) []byte {
		name := string(m[2 : len(m)-1]) // strip ${ and }
		val, ok := os.LookupEnv(name)
		if !ok {
			if firstErr == nil {
				firstErr = fmt.Errorf("environment variable %s not set", name)
			}
			return m // leave placeholder; error aborts Load anyway
		}
		return []byte(val)
	})
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Server.MaxBatchElements == 0 {
		cfg.Server.MaxBatchElements = 100
	}
	if cfg.Server.MaxBodyBytes == 0 {
		cfg.Server.MaxBodyBytes = 1048576 // 1 MiB
	}
	if len(cfg.Server.Timeouts) == 0 {
		cfg.Server.Timeouts = map[string]int{"default": 10}
	} else if _, ok := cfg.Server.Timeouts["default"]; !ok {
		cfg.Server.Timeouts["default"] = 10
	}

	if cfg.Prober.Interval == 0 {
		cfg.Prober.Interval = Duration(10 * time.Second)
	}
	if cfg.Prober.Timeout == 0 {
		cfg.Prober.Timeout = Duration(5 * time.Second)
	}
	if cfg.Prober.FailThreshold == 0 {
		cfg.Prober.FailThreshold = 3
	}

	if cfg.Retry.MaxAttempts == 0 {
		cfg.Retry.MaxAttempts = 2
	}
	if cfg.Retry.BaseDelay == 0 {
		cfg.Retry.BaseDelay = Duration(10 * time.Millisecond)
	}
	if cfg.Retry.MaxElapsed == 0 {
		cfg.Retry.MaxElapsed = Duration(30 * time.Second)
	}

	if cfg.Circuit.FailThreshold == 0 {
		cfg.Circuit.FailThreshold = 5
	}
	if cfg.Circuit.Cooldown == 0 {
		cfg.Circuit.Cooldown = Duration(30 * time.Second)
	}
}

var decimalRe = regexp.MustCompile(`^[0-9]+$`)

// Validate checks a fully-built Config. It is exported so callers can validate
// programmatically constructed configs as well as loaded ones.
func Validate(cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("config is nil")
	}
	if cfg.Server.Listen == "" {
		return fmt.Errorf("server.listen is required")
	}
	if cfg.Server.MetricsListen == "" {
		return fmt.Errorf("server.metrics_listen is required")
	}
	if cfg.Server.MaxBatchElements < 0 {
		return fmt.Errorf("server.max_batch_elements must be >= 1, got %d", cfg.Server.MaxBatchElements)
	}
	if cfg.Server.MaxBodyBytes != 0 && cfg.Server.MaxBodyBytes < 1024 {
		return fmt.Errorf("server.max_body_bytes must be >= 1024, got %d", cfg.Server.MaxBodyBytes)
	}
	if len(cfg.Chains) == 0 {
		return fmt.Errorf("chains must not be empty")
	}
	seen := make(map[string]bool, len(cfg.Chains))
	for i, ch := range cfg.Chains {
		if !decimalRe.MatchString(ch.ChainID) {
			return fmt.Errorf("chains[%d].chain_id %q must be a decimal non-negative integer string", i, ch.ChainID)
		}
		// Canonicalize before the duplicate check: strip leading zeros
		// ("01" -> "1", "00" -> "0"), matching the key form router.New
		// derives via chain.ParseChainID, so ids that would collide at
		// runtime are rejected at load time.
		canonical := strings.TrimLeft(ch.ChainID, "0")
		if canonical == "" {
			canonical = "0"
		}
		if seen[canonical] {
			return fmt.Errorf("chains[%d].chain_id %q is duplicated", i, ch.ChainID)
		}
		seen[canonical] = true
		if ch.Adapter == "" {
			return fmt.Errorf("chains[%d].adapter is required", i)
		}
		if len(ch.Upstreams) == 0 {
			return fmt.Errorf("chains[%d].upstreams must not be empty", i)
		}
		for j, u := range ch.Upstreams {
			parsed, err := url.Parse(u.URL)
			if err != nil {
				return fmt.Errorf("chains[%d].upstreams[%d].url %q: %w", i, j, u.URL, err)
			}
			if parsed.Scheme != "http" && parsed.Scheme != "https" {
				return fmt.Errorf("chains[%d].upstreams[%d].url %q: scheme must be http or https", i, j, u.URL)
			}
		}
	}
	return nil
}
