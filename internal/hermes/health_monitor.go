package hermes

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/akash-network/price-feed-monitor/internal/alerting"
	"github.com/akash-network/price-feed-monitor/internal/config"
	"github.com/akash-network/price-feed-monitor/internal/types"
)

// relayerState tracks per-relayer failure state across polling cycles.
type relayerState struct {
	consecutiveFailures int
	lastSeen            time.Time // last successful health check
}

// HealthMonitor implements Component 2: Hermes Relayer Health.
//
// It polls the /health endpoint on each configured relayer, checks isRunning,
// optionally validates priceFeedId and contractAddress, and tracks consecutive
// failures before escalating alerts.
type HealthMonitor struct {
	network config.NetworkConfig
	cfg     config.HermesHealthConfig
	alerter *alerting.Slack
	logger  *slog.Logger
	client  *http.Client
	states  map[string]*relayerState // keyed by relayer name
}

func NewHealthMonitor(
	network config.NetworkConfig,
	cfg config.HermesHealthConfig,
	alerter *alerting.Slack,
	logger *slog.Logger,
) *HealthMonitor {
	states := make(map[string]*relayerState, len(network.HermesRelayers))
	for _, r := range network.HermesRelayers {
		states[r.Name] = &relayerState{}
	}

	return &HealthMonitor{
		network: network,
		cfg:     cfg,
		alerter: alerter,
		logger:  logger.With("network", network.Name, "component", "hermes_health"),
		client:  &http.Client{Timeout: 10 * time.Second},
		states:  states,
	}
}

// Run starts the polling loop. Performs an immediate check on startup,
// then polls at the configured interval. Blocks until ctx is cancelled.
func (m *HealthMonitor) Run(ctx context.Context) {
	m.logger.Info("hermes health monitor started",
		"poll_interval", m.cfg.PollInterval.Duration,
		"failure_threshold", m.cfg.ConsecutiveFailuresThreshold,
		"relayers", len(m.network.HermesRelayers),
	)

	m.checkAll(ctx)

	ticker := time.NewTicker(m.cfg.PollInterval.Duration)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkAll(ctx)
		}
	}
}

func (m *HealthMonitor) checkAll(ctx context.Context) {
	for _, relayer := range m.network.HermesRelayers {
		m.checkRelayer(ctx, relayer)
	}
}

func (m *HealthMonitor) checkRelayer(ctx context.Context, relayer config.RelayerConfig) {
	state := m.states[relayer.Name]
	alertKey := fmt.Sprintf("hermes_health_%s_%s", m.network.Name, relayer.Name)

	health, err := m.fetchHealth(ctx, relayer.HealthEndpoint)
	if err != nil {
		state.consecutiveFailures++
		m.logger.Error("hermes health check failed",
			"relayer", relayer.Name,
			"endpoint", relayer.HealthEndpoint,
			"consecutive_failures", state.consecutiveFailures,
			"error", err,
		)

		lastSeenStr := "never"
		if !state.lastSeen.IsZero() {
			lastSeenStr = state.lastSeen.UTC().Format(time.RFC3339)
		}

		m.alerter.Send(types.Alert{
			Key:      alertKey,
			Severity: types.SeverityCritical,
			Title:    "HERMES RELAYER UNREACHABLE",
			Body: fmt.Sprintf(
				"Network: %s\nRelayer: %s\nEndpoint: %s\n\n"+
					"Status: Unreachable (%d consecutive failure(s))\n"+
					"Last seen: %s\nError: %s\n\n"+
					"Action required: Check container status\n"+
					"  docker logs hermes-client\n"+
					"  docker restart hermes-client",
				m.network.Name, relayer.Name, relayer.HealthEndpoint,
				state.consecutiveFailures,
				lastSeenStr,
				err.Error(),
			),
		})
		return
	}

	// Endpoint responded — check isRunning.
	if !health.IsRunning {
		state.consecutiveFailures++
		m.logger.Warn("hermes relayer not running",
			"relayer", relayer.Name,
			"address", health.Address,
			"consecutive_failures", state.consecutiveFailures,
		)

		m.alerter.Send(types.Alert{
			Key:      alertKey,
			Severity: types.SeverityCritical,
			Title:    "HERMES RELAYER STOPPED",
			Body: fmt.Sprintf(
				"Network: %s\nRelayer: %s\nEndpoint: %s\n\n"+
					"Status: isRunning=false (%d consecutive failure(s))\n"+
					"Address: %s\n\n"+
					"Action required: Restart the Hermes container\n"+
					"  docker restart hermes-client",
				m.network.Name, relayer.Name, relayer.HealthEndpoint,
				state.consecutiveFailures,
				health.Address,
			),
		})
		return
	}

	// Relayer is running — check for config mismatches.
	mismatches := m.detectMismatches(relayer, health)
	if len(mismatches) > 0 {
		m.logger.Warn("hermes relayer config mismatch",
			"relayer", relayer.Name,
			"mismatches", mismatches,
		)

		mismatchKey := fmt.Sprintf("hermes_config_mismatch_%s_%s", m.network.Name, relayer.Name)
		m.alerter.Send(types.Alert{
			Key:      mismatchKey,
			Severity: types.SeverityWarning,
			Title:    "HERMES RELAYER CONFIG MISMATCH",
			Body: fmt.Sprintf(
				"Network: %s\nRelayer: %s\nEndpoint: %s\n\n"+
					"Unexpected configuration values detected:\n%s\n\n"+
					"Action required: Verify relayer is pointed at the correct contract and price feed.",
				m.network.Name, relayer.Name, relayer.HealthEndpoint,
				strings.Join(mismatches, "\n"),
			),
		})
	} else {
		// Clear config mismatch alert if it was previously firing.
		mismatchKey := fmt.Sprintf("hermes_config_mismatch_%s_%s", m.network.Name, relayer.Name)
		m.alerter.Resolve(
			mismatchKey,
			"HERMES RELAYER CONFIG HEALTHY",
			fmt.Sprintf("Network: %s\nRelayer: %s\nConfig values are correct.", m.network.Name, relayer.Name),
		)
	}

	// Healthy — record and resolve any outstanding unreachable/stopped alert.
	state.consecutiveFailures = 0
	state.lastSeen = time.Now()

	m.logger.Info("hermes health check OK",
		"relayer", relayer.Name,
		"address", health.Address,
		"price_feed_id", health.PriceFeedID,
		"contract_address", health.ContractAddress,
	)

	m.alerter.Resolve(
		alertKey,
		"HERMES RELAYER HEALTHY",
		fmt.Sprintf(
			"Network: %s\nRelayer: %s\n\n"+
				"Status: Running\n"+
				"Address: %s\n"+
				"Price Feed ID: %s\n"+
				"Contract: %s",
			m.network.Name, relayer.Name,
			health.Address,
			health.PriceFeedID,
			health.ContractAddress,
		),
	)
}

func (m *HealthMonitor) fetchHealth(ctx context.Context, endpoint string) (*types.HermesHealthResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var health types.HermesHealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &health, nil
}

// detectMismatches compares the live health response against any expected
// values configured for the relayer. Returns a slice of human-readable
// mismatch descriptions; empty slice means all checks passed.
func (m *HealthMonitor) detectMismatches(relayer config.RelayerConfig, health *types.HermesHealthResponse) []string {
	var mismatches []string

	if relayer.ExpectedPriceFeedID != "" &&
		!strings.EqualFold(health.PriceFeedID, relayer.ExpectedPriceFeedID) {
		mismatches = append(mismatches, fmt.Sprintf(
			"  priceFeedId:\n    expected: %s\n    got:      %s",
			relayer.ExpectedPriceFeedID, health.PriceFeedID,
		))
	}

	if relayer.ExpectedContractAddress != "" &&
		!strings.EqualFold(health.ContractAddress, relayer.ExpectedContractAddress) {
		mismatches = append(mismatches, fmt.Sprintf(
			"  contractAddress:\n    expected: %s\n    got:      %s",
			relayer.ExpectedContractAddress, health.ContractAddress,
		))
	}

	return mismatches
}
