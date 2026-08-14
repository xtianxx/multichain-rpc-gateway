package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fullValidYAML = `
server:
  listen: ":8545"
  metrics_listen: ":9090"
  max_batch_elements: 200
  max_body_bytes: 2097152
  timeouts:
    default: 10
    eth_getLogs: 30
prober:
  interval: 5s
  timeout: 2s
  fail_threshold: 2
retry:
  max_attempts: 3
  base_delay: 20ms
  max_elapsed: 10s
circuit:
  fail_threshold: 4
  cooldown: 15s
chains:
  - chain_id: "1"
    adapter: ethereum
    upstreams:
      - name: mainnet-a
        url: "https://mainnet.example.com"
  - chain_id: "8453"
    adapter: base
    upstreams:
      - url: "http://base.example.com"
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadFullValid(t *testing.T) {
	cfg, err := Load(writeConfig(t, fullValidYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Listen != ":8545" || cfg.Server.MetricsListen != ":9090" {
		t.Errorf("listen addresses: %+v", cfg.Server)
	}
	if cfg.Server.MaxBatchElements != 200 || cfg.Server.MaxBodyBytes != 2097152 {
		t.Errorf("limits: %+v", cfg.Server)
	}
	if cfg.Server.Timeouts["eth_getLogs"] != 30 || cfg.Server.Timeouts["default"] != 10 {
		t.Errorf("timeouts: %v", cfg.Server.Timeouts)
	}
	if cfg.Prober.Interval.Std() != 5*time.Second || cfg.Prober.Timeout.Std() != 2*time.Second || cfg.Prober.FailThreshold != 2 {
		t.Errorf("prober: %+v", cfg.Prober)
	}
	if cfg.Retry.MaxAttempts != 3 || cfg.Retry.BaseDelay.Std() != 20*time.Millisecond {
		t.Errorf("retry: %+v", cfg.Retry)
	}
	if cfg.Circuit.FailThreshold != 4 || cfg.Circuit.Cooldown.Std() != 15*time.Second {
		t.Errorf("circuit: %+v", cfg.Circuit)
	}
	if len(cfg.Chains) != 2 {
		t.Fatalf("chains: got %d want 2", len(cfg.Chains))
	}
	if cfg.Chains[0].ChainID != "1" || cfg.Chains[0].Adapter != "ethereum" || cfg.Chains[0].Upstreams[0].Name != "mainnet-a" {
		t.Errorf("chain 0: %+v", cfg.Chains[0])
	}
	if cfg.Chains[1].ChainID != "8453" || cfg.Chains[1].Adapter != "base" {
		t.Errorf("chain 1: %+v", cfg.Chains[1])
	}
}

func TestLoadEnvSubstitution(t *testing.T) {
	t.Setenv("TEST_RPC_URL", "https://rpc.example.com/key123")
	t.Setenv("TEST_LISTEN", ":9999")
	yaml := `
server:
  listen: "${TEST_LISTEN}"
  metrics_listen: ":9090"
chains:
  - chain_id: "1"
    adapter: ethereum
    upstreams:
      - url: '${TEST_RPC_URL}'
`
	cfg, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Listen != ":9999" {
		t.Errorf("listen: got %q", cfg.Server.Listen)
	}
	if cfg.Chains[0].Upstreams[0].URL != "https://rpc.example.com/key123" {
		t.Errorf("url: got %q", cfg.Chains[0].Upstreams[0].URL)
	}
}

func TestLoadUnsetEnvFails(t *testing.T) {
	os.Unsetenv("DEFINITELY_NOT_SET_ANYWHERE_123")
	yaml := `
server:
  listen: ":8545"
  metrics_listen: ":9090"
chains:
  - chain_id: "1"
    adapter: ethereum
    upstreams:
      - url: "${DEFINITELY_NOT_SET_ANYWHERE_123}"
`
	_, err := Load(writeConfig(t, yaml))
	if err == nil {
		t.Fatal("unset env var must fail")
	}
	if !strings.Contains(err.Error(), "DEFINITELY_NOT_SET_ANYWHERE_123") {
		t.Errorf("error must name the variable: %v", err)
	}
}

func TestLoadUnknownFieldRejected(t *testing.T) {
	yaml := `
server:
  listen: ":8545"
  metrics_listen: ":9090"
  max_bach_elements: 50
chains:
  - chain_id: "1"
    adapter: ethereum
    upstreams:
      - url: "https://x.example.com"
`
	_, err := Load(writeConfig(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "max_bach_elements") {
		t.Errorf("typo'd field must be rejected: %v", err)
	}
}

func TestLoadChainIDValidation(t *testing.T) {
	for _, id := range []string{"-1", "abc", "1.5", "0x1", ""} {
		yaml := `
server:
  listen: ":8545"
  metrics_listen: ":9090"
chains:
  - chain_id: "` + id + `"
    adapter: ethereum
    upstreams:
      - url: "https://x.example.com"
`
		if _, err := Load(writeConfig(t, yaml)); err == nil {
			t.Errorf("chain_id %q must be rejected", id)
		}
	}
}

func TestLoadDuplicateChainID(t *testing.T) {
	yaml := `
server:
  listen: ":8545"
  metrics_listen: ":9090"
chains:
  - chain_id: "1"
    adapter: ethereum
    upstreams:
      - url: "https://x.example.com"
  - chain_id: "1"
    adapter: ethereum
    upstreams:
      - url: "https://y.example.com"
`
	_, err := Load(writeConfig(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Errorf("duplicate chain id must be rejected: %v", err)
	}
}

func TestLoadDuplicateChainIDLeadingZeros(t *testing.T) {
	// "1" and "01" canonicalize to the same id (router.New key) and must
	// collide at load time, not silently overwrite in the router map.
	yaml := `
server:
  listen: ":8545"
  metrics_listen: ":9090"
chains:
  - chain_id: "1"
    adapter: ethereum
    upstreams:
      - url: "https://x.example.com"
  - chain_id: "01"
    adapter: ethereum
    upstreams:
      - url: "https://y.example.com"
`
	_, err := Load(writeConfig(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Errorf(`chain ids "1" and "01" must collide after canonicalization: %v`, err)
	}
}

func TestLoadLeadingZeroChainIDAccepted(t *testing.T) {
	yaml := `
server:
  listen: ":8545"
  metrics_listen: ":9090"
chains:
  - chain_id: "01"
    adapter: ethereum
    upstreams:
      - url: "https://x.example.com"
`
	cfg, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Chains[0].ChainID != "01" {
		t.Errorf("raw chain id must be preserved: got %q want %q", cfg.Chains[0].ChainID, "01")
	}
}

func TestLoadZeroChainIDEdge(t *testing.T) {
	// A single all-zeros id is a valid chain id.
	yaml := `
server:
  listen: ":8545"
  metrics_listen: ":9090"
chains:
  - chain_id: "0"
    adapter: ethereum
    upstreams:
      - url: "https://x.example.com"
`
	if _, err := Load(writeConfig(t, yaml)); err != nil {
		t.Errorf(`single chain_id "0" must load: %v`, err)
	}

	// "0" and "00" both canonicalize to "0" and must collide.
	yaml = `
server:
  listen: ":8545"
  metrics_listen: ":9090"
chains:
  - chain_id: "0"
    adapter: ethereum
    upstreams:
      - url: "https://x.example.com"
  - chain_id: "00"
    adapter: ethereum
    upstreams:
      - url: "https://y.example.com"
`
	_, err := Load(writeConfig(t, yaml))
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Errorf(`chain ids "0" and "00" must collide after canonicalization: %v`, err)
	}
}

func TestLoadUpstreamURLValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
	}{
		{"ftp-scheme", "ftp://x.example.com"},
		{"no-scheme", "x.example.com/rpc"},
	} {
		yaml := `
server:
  listen: ":8545"
  metrics_listen: ":9090"
chains:
  - chain_id: "1"
    adapter: ethereum
    upstreams:
      - url: "` + tc.url + `"
`
		if _, err := Load(writeConfig(t, yaml)); err == nil {
			t.Errorf("%s: url %q must be rejected", tc.name, tc.url)
		}
	}
}

func TestLoadEmptyUpstreams(t *testing.T) {
	yaml := `
server:
  listen: ":8545"
  metrics_listen: ":9090"
chains:
  - chain_id: "1"
    adapter: ethereum
    upstreams: []
`
	if _, err := Load(writeConfig(t, yaml)); err == nil {
		t.Error("empty upstreams must be rejected")
	}
}

func TestLoadDefaultsApplied(t *testing.T) {
	yaml := `
server:
  listen: ":8545"
  metrics_listen: ":9090"
chains:
  - chain_id: "1"
    adapter: ethereum
    upstreams:
      - url: "https://x.example.com"
`
	cfg, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.MaxBatchElements != 100 || cfg.Server.MaxBodyBytes != 1048576 {
		t.Errorf("server defaults: %+v", cfg.Server)
	}
	if cfg.Server.Timeouts["default"] != 10 {
		t.Errorf("timeout default: %v", cfg.Server.Timeouts)
	}
	if cfg.Prober.Interval.Std() != 10*time.Second || cfg.Prober.Timeout.Std() != 5*time.Second || cfg.Prober.FailThreshold != 3 {
		t.Errorf("prober defaults: %+v", cfg.Prober)
	}
	if cfg.Retry.MaxAttempts != 2 || cfg.Retry.BaseDelay.Std() != 10*time.Millisecond || cfg.Retry.MaxElapsed.Std() != 30*time.Second {
		t.Errorf("retry defaults: %+v", cfg.Retry)
	}
	if cfg.Circuit.FailThreshold != 5 || cfg.Circuit.Cooldown.Std() != 30*time.Second {
		t.Errorf("circuit defaults: %+v", cfg.Circuit)
	}
}

func TestLoadBodyBytesLowerBound(t *testing.T) {
	yaml := `
server:
  listen: ":8545"
  metrics_listen: ":9090"
  max_body_bytes: 512
chains:
  - chain_id: "1"
    adapter: ethereum
    upstreams:
      - url: "https://x.example.com"
`
	if _, err := Load(writeConfig(t, yaml)); err == nil {
		t.Error("max_body_bytes below 1 KiB must be rejected")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("missing file must error")
	}
}

func TestDurationInvalid(t *testing.T) {
	yaml := `
server:
  listen: ":8545"
  metrics_listen: ":9090"
prober:
  interval: 10
chains:
  - chain_id: "1"
    adapter: ethereum
    upstreams:
      - url: "https://x.example.com"
`
	// A bare YAML number is not a duration string and must be rejected.
	if _, err := Load(writeConfig(t, yaml)); err == nil {
		t.Error("numeric duration must be rejected")
	}
}

func TestValidateStandalone(t *testing.T) {
	cfg, err := Load(writeConfig(t, fullValidYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := Validate(cfg); err != nil {
		t.Errorf("Validate on valid config: %v", err)
	}
	if err := Validate(&Config{}); err == nil {
		t.Error("Validate on empty config must fail")
	}
}
