package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration structure loaded from config.yaml.
type Config struct {
	Slack                SlackConfig           `yaml:"slack"`
	OraclePriceMonitor   OraclePriceConfig     `yaml:"oracle_price_monitor"`
	HermesHealthMonitor  HermesHealthConfig    `yaml:"hermes_health_monitor"`
	GuardianSetMonitor   GuardianSetConfig     `yaml:"guardian_set_monitor"`
	WormholescanMonitor  WormholescanConfig    `yaml:"wormholescan_monitor"`
	BMEMonitor           BMEConfig             `yaml:"bme_monitor"`
	AnnouncementMonitor  AnnouncementConfig    `yaml:"announcement_monitor"`
	Networks             []NetworkConfig       `yaml:"networks"`
}

type SlackConfig struct {
	WebhookURL string `yaml:"webhook_url"`
	Channel    string `yaml:"channel"`
}

type OraclePriceConfig struct {
	Enabled      bool            `yaml:"enabled"`
	PollInterval Duration        `yaml:"poll_interval"`
	Thresholds   PriceThresholds `yaml:"thresholds"`
}

type PriceThresholds struct {
	WarningAge   Duration `yaml:"warning_age"`
	CriticalAge  Duration `yaml:"critical_age"`
	EmergencyAge Duration `yaml:"emergency_age"`
}

type HermesHealthConfig struct {
	Enabled                      bool     `yaml:"enabled"`
	PollInterval                 Duration `yaml:"poll_interval"`
	ConsecutiveFailuresThreshold int      `yaml:"consecutive_failures_threshold"`
}

type GuardianSetConfig struct {
	Enabled          bool     `yaml:"enabled"`
	PollInterval     Duration `yaml:"poll_interval"`
	EthereumRPC      string   `yaml:"ethereum_rpc"`
	WormholeContract string   `yaml:"wormhole_contract"`
}

// WormholescanConfig configures Component 5: the Wormholescan-based reactive guardian
// set monitor. This runs alongside Component 3 (Ethereum RPC) to provide a second
// detection path that also retrieves the governance VAA needed for on-chain submission.
type WormholescanConfig struct {
	Enabled  bool     `yaml:"enabled"`
	PollInterval Duration `yaml:"poll_interval"`

	// APIBaseURL is the Wormholescan REST API base. Default is the public endpoint.
	// No API key is required for the guardian set and VAA endpoints used here.
	APIBaseURL string `yaml:"api_base_url"`

	// GovernanceEmitter is the Wormhole Core governance emitter address on Ethereum
	// (chain 1), zero-padded to 64 hex characters. This is a well-known constant
	// used for all guardian set upgrade governance VAAs since Wormhole mainnet launch.
	// Standard value: 0000000000000000000000000000000000000000000000000000000000000004
	GovernanceEmitter string `yaml:"governance_emitter"`
}

// BMEConfig configures Component 6: the Burn Mint Equilibrium status monitor.
// Thresholds (warn/halt) are read dynamically from the chain on each poll cycle —
// they are NOT configured here. This ensures the monitor automatically respects
// any changes made via Akash governance without requiring a restart.
type BMEConfig struct {
	Enabled      bool     `yaml:"enabled"`
	PollInterval Duration `yaml:"poll_interval"`
}

type AnnouncementConfig struct {
	Enabled   bool            `yaml:"enabled"`
	Etherscan EtherscanConfig `yaml:"etherscan"`
	PythForum PythForumConfig `yaml:"pyth_forum"`
	GitHub    GitHubConfig    `yaml:"github"`
}

type EtherscanConfig struct {
	Enabled      bool     `yaml:"enabled"`
	APIKey       string   `yaml:"api_key"`
	PollInterval Duration `yaml:"poll_interval"`
}

type PythForumConfig struct {
	Enabled      bool     `yaml:"enabled"`
	URL          string   `yaml:"url"`
	PollInterval Duration `yaml:"poll_interval"`
	Keywords     []string `yaml:"keywords"`
}

type GitHubConfig struct {
	Enabled      bool     `yaml:"enabled"`
	Repo         string   `yaml:"repo"`
	PollInterval Duration `yaml:"poll_interval"`
}

type NetworkConfig struct {
	Name           string          `yaml:"name"`
	AkashAPI       string          `yaml:"akash_api"`
	HermesRelayers []RelayerConfig `yaml:"hermes_relayers"`
}

type RelayerConfig struct {
	Name             string `yaml:"name"`
	HealthEndpoint   string `yaml:"health_endpoint"`
	Wallet           string `yaml:"wallet"`
	MinWalletBalance int64  `yaml:"min_wallet_balance"`
	// Optional: if set, the health monitor will alert if the relayer reports
	// a different value (detects accidental misconfiguration).
	ExpectedPriceFeedID     string `yaml:"expected_price_feed_id"`
	ExpectedContractAddress string `yaml:"expected_contract_address"`
}

// Duration wraps time.Duration to support YAML unmarshaling of strings like "60s", "5m".
type Duration struct {
	time.Duration
}

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	dur, err := time.ParseDuration(value.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", value.Value, err)
	}
	d.Duration = dur
	return nil
}

// EnvConfigPath is the environment variable that overrides the config file path.
// Useful in K8s where the config is mounted as a ConfigMap at a fixed path.
const EnvConfigPath = "PRICE_FEED_MONITOR_CONFIG"

// Load reads and validates the config file at the given path.
// Environment variables take precedence over config.yaml values for secrets:
//
//	SLACK_WEBHOOK_URL  — overrides slack.webhook_url
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config %q: %w", path, err)
	}
	defer f.Close()

	var cfg Config
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Apply env var overrides — these win over config.yaml values.
	// In K8s, project secrets as env vars (envFrom: secretKeyRef).
	if v := os.Getenv("SLACK_WEBHOOK_URL"); v != "" {
		cfg.Slack.WebhookURL = v
	}

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

func (c *Config) validate() error {
	if c.Slack.WebhookURL == "" {
		return fmt.Errorf("slack.webhook_url is required")
	}
	for i, n := range c.Networks {
		if n.Name == "" {
			return fmt.Errorf("networks[%d].name is required", i)
		}
		if n.AkashAPI == "" {
			return fmt.Errorf("networks[%d].akash_api is required", i)
		}
	}
	return nil
}
