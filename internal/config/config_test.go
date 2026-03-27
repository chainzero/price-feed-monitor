package config

import (
	"os"
	"testing"
	"time"
)

// writeTemp writes content to a temp file and returns its path.
// The file is automatically removed when the test ends.
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", "pfm-config-*.yaml")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })
	return f.Name()
}

const minimalConfig = `
slack:
  webhook_url: "https://hooks.slack.com/services/TEST"
  channel: "#test"
networks:
  - name: mainnet
    akash_api:
      - "https://api.example.com"
`

func TestLoad_Minimal(t *testing.T) {
	path := writeTemp(t, minimalConfig)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.Slack.WebhookURL != "https://hooks.slack.com/services/TEST" {
		t.Errorf("webhook URL = %q, want TEST URL", cfg.Slack.WebhookURL)
	}
	if len(cfg.Networks) != 1 || cfg.Networks[0].Name != "mainnet" {
		t.Errorf("networks not parsed correctly: %+v", cfg.Networks)
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/path/config.yaml")
	if err == nil {
		t.Fatal("Load() expected error for missing file, got nil")
	}
}

func TestLoad_InvalidYAML(t *testing.T) {
	path := writeTemp(t, "this: is: not: valid: yaml: ::::")
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected error for invalid YAML, got nil")
	}
}

func TestLoad_UnknownField(t *testing.T) {
	// KnownFields(true) should reject unknown top-level keys.
	path := writeTemp(t, minimalConfig+"\nunknown_field: true\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected error for unknown field, got nil")
	}
}

func TestLoad_MissingWebhookURL(t *testing.T) {
	yaml := `
slack:
  channel: "#test"
networks:
  - name: mainnet
    akash_api:
      - "https://api.example.com"
`
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected validation error for missing webhook_url, got nil")
	}
}

func TestLoad_MissingNetworkName(t *testing.T) {
	yaml := `
slack:
  webhook_url: "https://hooks.slack.com/services/TEST"
networks:
  - akash_api:
      - "https://api.example.com"
`
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected validation error for missing network name, got nil")
	}
}

func TestLoad_MissingAkashAPI(t *testing.T) {
	yaml := `
slack:
  webhook_url: "https://hooks.slack.com/services/TEST"
networks:
  - name: mainnet
`
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected validation error for missing akash_api, got nil")
	}
}

func TestLoad_EnvOverrideSlackWebhook(t *testing.T) {
	// Config has a placeholder; env var should override it.
	path := writeTemp(t, minimalConfig)
	t.Setenv("SLACK_WEBHOOK_URL", "https://hooks.slack.com/services/FROM_ENV")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.Slack.WebhookURL != "https://hooks.slack.com/services/FROM_ENV" {
		t.Errorf("webhook URL = %q, want env override", cfg.Slack.WebhookURL)
	}
}

func TestLoad_EnvOverrideSendGridKey(t *testing.T) {
	path := writeTemp(t, minimalConfig)
	t.Setenv("SENDGRID_API_KEY", "SG.test-key")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.Email.APIKey != "SG.test-key" {
		t.Errorf("Email.APIKey = %q, want SG.test-key", cfg.Email.APIKey)
	}
}

func TestLoad_EmailAPIKeyNotInYAML(t *testing.T) {
	// Even if someone puts sendgrid_api_key in the YAML it should be ignored
	// because the field has yaml:"-". The only way to set it is via env var.
	yaml := minimalConfig + `
email:
  enabled: true
  from: "test@example.com"
  to:
    - "test@example.com"
  min_severity: "warning"
`
	path := writeTemp(t, yaml)
	// No env var set — APIKey should be empty.
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.Email.APIKey != "" {
		t.Errorf("Email.APIKey should be empty without env var, got %q", cfg.Email.APIKey)
	}
}

func TestDurationUnmarshal(t *testing.T) {
	cases := []struct {
		yaml string
		want time.Duration
	}{
		{"poll_interval: 60s", 60 * time.Second},
		{"poll_interval: 5m", 5 * time.Minute},
		{"poll_interval: 1h", time.Hour},
		{"poll_interval: 30m", 30 * time.Minute},
	}

	for _, tc := range cases {
		yaml := `
slack:
  webhook_url: "https://hooks.slack.com/services/TEST"
networks:
  - name: mainnet
    akash_api:
      - "https://api.example.com"
bme_monitor:
  enabled: true
  ` + tc.yaml + `
`
		path := writeTemp(t, yaml)
		cfg, err := Load(path)
		if err != nil {
			t.Errorf("Load() with %q: unexpected error: %v", tc.yaml, err)
			continue
		}
		if cfg.BMEMonitor.PollInterval.Duration != tc.want {
			t.Errorf("PollInterval for %q = %v, want %v",
				tc.yaml, cfg.BMEMonitor.PollInterval.Duration, tc.want)
		}
	}
}

func TestDurationUnmarshal_Invalid(t *testing.T) {
	yaml := `
slack:
  webhook_url: "https://hooks.slack.com/services/TEST"
networks:
  - name: mainnet
    akash_api:
      - "https://api.example.com"
bme_monitor:
  enabled: true
  poll_interval: "not-a-duration"
`
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected error for invalid duration, got nil")
	}
}

func TestLoad_MultipleNetworks(t *testing.T) {
	yaml := `
slack:
  webhook_url: "https://hooks.slack.com/services/TEST"
networks:
  - name: mainnet
    akash_api:
      - "https://api.mainnet.com"
  - name: testnet
    akash_api:
      - "https://api.testnet.com"
  - name: sandbox
    akash_api:
      - "https://api.sandbox.com"
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if len(cfg.Networks) != 3 {
		t.Fatalf("expected 3 networks, got %d", len(cfg.Networks))
	}
	if cfg.Networks[1].Name != "testnet" {
		t.Errorf("networks[1].Name = %q, want testnet", cfg.Networks[1].Name)
	}
}

func TestLoad_MultipleAkashNodes(t *testing.T) {
	yaml := `
slack:
  webhook_url: "https://hooks.slack.com/services/TEST"
networks:
  - name: mainnet
    akash_api:
      - "https://node1.example.com"
      - "https://node2.example.com"
      - "https://node3.example.com"
`
	path := writeTemp(t, yaml)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	nodes := cfg.Networks[0].AkashAPINodes
	if len(nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(nodes))
	}
	if nodes[1] != "https://node2.example.com" {
		t.Errorf("nodes[1] = %q, want node2", nodes[1])
	}
}

func TestLoad_EmptyAkashNodes(t *testing.T) {
	// akash_api with no entries should fail validation.
	yaml := `
slack:
  webhook_url: "https://hooks.slack.com/services/TEST"
networks:
  - name: mainnet
    akash_api: []
`
	path := writeTemp(t, yaml)
	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected validation error for empty akash_api, got nil")
	}
}
