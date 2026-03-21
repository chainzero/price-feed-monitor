package bme

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/akash-network/price-feed-monitor/internal/alerting"
	"github.com/akash-network/price-feed-monitor/internal/config"
	"github.com/akash-network/price-feed-monitor/internal/types"
)

// StatusMonitor implements Component 6: BME (Burn Mint Equilibrium) Status Monitor.
//
// The BME module governs the mint/burn mechanics of the Akash network token economy.
// Its health is determined by the collateral ratio — the ratio of collateral backing
// to outstanding minted tokens. The chain itself defines two threshold levels:
//
//	warn_threshold:  ratio below this triggers a warning state (default 0.95)
//	halt_threshold:  ratio below this halts minting entirely (default 0.90)
//
// Thresholds are read directly from the chain on each poll cycle, so the monitor
// automatically adjusts if governance changes them — no config update required.
//
// Alert conditions monitored:
//   - collateral_ratio < warn_threshold  → Warning (approaching halt)
//   - collateral_ratio < halt_threshold  → Critical (minting may be halted)
//   - mints_allowed = false              → Critical (minting is halted)
//   - refunds_allowed = false            → Critical (refunds are halted)
//
// All four conditions are tracked independently with their own alert keys so
// that each can resolve separately as the system recovers.
type StatusMonitor struct {
	network                config.NetworkConfig
	cfg                    config.BMEConfig
	alerter                *alerting.Slack
	logger                 *slog.Logger
	client                 *http.Client
	consecutiveFailures    int // API unreachable
	consecutiveHaltedPolls int // mints or refunds halted
}

func NewStatusMonitor(
	network config.NetworkConfig,
	cfg config.BMEConfig,
	alerter *alerting.Slack,
	logger *slog.Logger,
) *StatusMonitor {
	return &StatusMonitor{
		network: network,
		cfg:     cfg,
		alerter: alerter,
		logger:  logger.With("component", "bme_monitor", "network", network.Name),
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

// Run starts the polling loop. Blocks until ctx is cancelled.
func (m *StatusMonitor) Run(ctx context.Context) {
	m.logger.Info("BME status monitor started",
		"poll_interval", m.cfg.PollInterval.Duration,
		"endpoint", m.network.AkashAPI+"/akash/bme/v1/status",
	)

	m.check(ctx)

	ticker := time.NewTicker(m.cfg.PollInterval.Duration)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.check(ctx)
		}
	}
}

// bmeStatusResponse mirrors the JSON returned by GET /akash/bme/v1/status.
//
// All numeric fields are returned as decimal strings by the Cosmos SDK REST layer.
// collateral_ratio is the live ratio and will be very large when healthy (e.g. "210.5")
// since the system is overcollateralised; it approaches the thresholds only under stress.
type bmeStatusResponse struct {
	Status          string `json:"status"`
	CollateralRatio string `json:"collateral_ratio"`
	WarnThreshold   string `json:"warn_threshold"`
	HaltThreshold   string `json:"halt_threshold"`
	MintsAllowed    bool   `json:"mints_allowed"`
	RefundsAllowed  bool   `json:"refunds_allowed"`
}

func (m *StatusMonitor) check(ctx context.Context) {
	url := m.network.AkashAPI + "/akash/bme/v1/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		m.logger.Error("failed to create BME status request", "error", err)
		return
	}

	resp, err := m.client.Do(req)
	if err != nil {
		m.consecutiveFailures++
		m.logger.Error("BME status endpoint unreachable",
			"url", url,
			"consecutive_failures", m.consecutiveFailures,
			"error", err,
		)

		var sev types.Severity
		var title string
		switch m.consecutiveFailures {
		case 1:
			sev = types.SeverityWarning
			title = fmt.Sprintf("BME STATUS UNREACHABLE — WARNING — %s", m.network.Name)
		case 2:
			sev = types.SeverityCritical
			title = fmt.Sprintf("BME STATUS UNREACHABLE — CRITICAL — %s", m.network.Name)
		case 3:
			sev = types.SeverityEmergency
			title = fmt.Sprintf("BME STATUS UNREACHABLE — EMERGENCY — %s (final alert)", m.network.Name)
		default:
			return
		}

		m.alerter.Send(types.Alert{
			Key:      fmt.Sprintf("bme_unreachable_%s", m.network.Name),
			Severity: sev,
			Title:    title,
			Body: fmt.Sprintf(
				"Network: %s\n"+
					"Cannot reach BME status endpoint.\n"+
					"URL: %s\n"+
					"Consecutive failures: %d\n"+
					"Error: %s\n\n"+
					"BME health cannot be verified while this endpoint is down.\n"+
					"No further alerts will be sent until the endpoint recovers.",
				m.network.Name, url, m.consecutiveFailures, err.Error(),
			),
		})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		m.logger.Error("unexpected BME status response", "status_code", resp.StatusCode)
		return
	}

	var s bmeStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		m.logger.Error("failed to decode BME status response", "error", err)
		return
	}

	// Endpoint is responding — reset failure count and clear any outstanding alert.
	if m.consecutiveFailures > 0 {
		m.consecutiveFailures = 0
		m.alerter.Resolve(
			fmt.Sprintf("bme_unreachable_%s", m.network.Name),
			fmt.Sprintf("BME STATUS REACHABLE — %s", m.network.Name),
			fmt.Sprintf("Network: %s\nBME status endpoint is responding again.", m.network.Name),
		)
	}

	// --- Parse numeric fields ---
	// Thresholds are read from the chain response so the monitor automatically
	// respects any changes made via governance without requiring a config update.
	ratio, err := strconv.ParseFloat(s.CollateralRatio, 64)
	if err != nil {
		m.logger.Error("failed to parse collateral_ratio", "raw", s.CollateralRatio, "error", err)
		return
	}
	warnThreshold, err := strconv.ParseFloat(s.WarnThreshold, 64)
	if err != nil {
		m.logger.Error("failed to parse warn_threshold", "raw", s.WarnThreshold, "error", err)
		return
	}
	haltThreshold, err := strconv.ParseFloat(s.HaltThreshold, 64)
	if err != nil {
		m.logger.Error("failed to parse halt_threshold", "raw", s.HaltThreshold, "error", err)
		return
	}

	m.logger.Info("BME status fetched",
		"status", s.Status,
		"collateral_ratio", ratio,
		"warn_threshold", warnThreshold,
		"halt_threshold", haltThreshold,
		"mints_allowed", s.MintsAllowed,
		"refunds_allowed", s.RefundsAllowed,
	)

	// --- Check 1: Collateral ratio thresholds ---
	//
	// A healthy system has a very large collateral ratio (e.g. 210x). The ratio only
	// approaches the thresholds under stress. Two severity levels:
	//   - Below warn_threshold: Warning — approaching halt, team should investigate
	//   - Below halt_threshold: Critical — at or past the point where minting halts
	//
	// A single alert key is used for both levels. The Slack alerter will bypass the
	// cooldown automatically if severity escalates from Warning to Critical.
	collateralKey := fmt.Sprintf("bme_collateral_%s", m.network.Name)
	if ratio < haltThreshold {
		m.alerter.Send(types.Alert{
			Key:      collateralKey,
			Severity: types.SeverityCritical,
			Title:    fmt.Sprintf("BME COLLATERAL RATIO CRITICAL — %s", m.network.Name),
			Body: fmt.Sprintf(
				"Network: %s\n"+
					"BME Status: %s\n\n"+
					"Collateral Ratio: %.6f\n"+
					"Halt Threshold:   %.6f  ← BREACHED\n"+
					"Warn Threshold:   %.6f\n\n"+
					"The collateral ratio has fallen below the halt threshold.\n"+
					"Minting may be halted or imminent. Immediate action required.\n\n"+
					"Mints Allowed:   %v\n"+
					"Refunds Allowed: %v",
				m.network.Name, s.Status,
				ratio, haltThreshold, warnThreshold,
				s.MintsAllowed, s.RefundsAllowed,
			),
		})
	} else if ratio < warnThreshold {
		m.alerter.Send(types.Alert{
			Key:      collateralKey,
			Severity: types.SeverityWarning,
			Title:    fmt.Sprintf("BME COLLATERAL RATIO WARNING — %s", m.network.Name),
			Body: fmt.Sprintf(
				"Network: %s\n"+
					"BME Status: %s\n\n"+
					"Collateral Ratio: %.6f\n"+
					"Warn Threshold:   %.6f  ← BREACHED\n"+
					"Halt Threshold:   %.6f\n\n"+
					"The collateral ratio is below the warning threshold.\n"+
					"System is approaching the halt threshold — investigate now.\n\n"+
					"Mints Allowed:   %v\n"+
					"Refunds Allowed: %v",
				m.network.Name, s.Status,
				ratio, warnThreshold, haltThreshold,
				s.MintsAllowed, s.RefundsAllowed,
			),
		})
	} else {
		// Ratio is healthy — resolve any outstanding collateral alert.
		m.alerter.Resolve(
			collateralKey,
			fmt.Sprintf("BME COLLATERAL RATIO HEALTHY — %s", m.network.Name),
			fmt.Sprintf(
				"Network: %s\n"+
					"Collateral Ratio: %.6f (above warn threshold %.6f)\n"+
					"BME Status: %s",
				m.network.Name, ratio, warnThreshold, s.Status,
			),
		)
	}

	// --- Check 2: Minting or refunds halted (combined) ---
	//
	// Both flags are checked together and reported in a single alert because they
	// almost always flip together (e.g. mint_status_halt_oracle sets both false
	// simultaneously). Separate alerts for each flag would cause duplicate pages.
	//
	// Escalation mirrors the bme_unreachable pattern:
	//   1st consecutive poll halted → Warning
	//   2nd consecutive poll halted → Critical
	//   3rd+ consecutive poll halted → Emergency (final — suppressed until resolved)
	haltKey := fmt.Sprintf("bme_halted_%s", m.network.Name)
	if !s.MintsAllowed || !s.RefundsAllowed {
		m.consecutiveHaltedPolls++
		m.logger.Warn("BME halted",
			"mints_allowed", s.MintsAllowed,
			"refunds_allowed", s.RefundsAllowed,
			"status", s.Status,
			"consecutive_halted_polls", m.consecutiveHaltedPolls,
		)

		// After 3 consecutive halted polls the Emergency has been sent — suppress
		// further alerts until the condition resolves to avoid notification spam.
		if m.consecutiveHaltedPolls > 3 {
			return
		}

		var sev types.Severity
		var title string
		switch m.consecutiveHaltedPolls {
		case 1:
			sev = types.SeverityWarning
			title = fmt.Sprintf("BME HALTED — WARNING — %s", m.network.Name)
		case 2:
			sev = types.SeverityCritical
			title = fmt.Sprintf("BME HALTED — CRITICAL — %s", m.network.Name)
		default:
			sev = types.SeverityEmergency
			title = fmt.Sprintf("BME HALTED — EMERGENCY — %s (final alert)", m.network.Name)
		}

		// Describe which operations are halted.
		var halted []string
		if !s.MintsAllowed {
			halted = append(halted, "mints_allowed = FALSE  (new minting halted)")
		}
		if !s.RefundsAllowed {
			halted = append(halted, "refunds_allowed = FALSE  (collateral refunds halted)")
		}

		m.alerter.Send(types.Alert{
			Key:      haltKey,
			Severity: sev,
			Title:    title,
			Body: fmt.Sprintf(
				"Network: %s\n"+
					"BME Status: %s\n"+
					"Halt Reason: %s\n\n"+
					"Halted:\n  • %s\n\n"+
					"Collateral Ratio: %.6f\n"+
					"Warn Threshold:   %.6f\n"+
					"Halt Threshold:   %.6f\n\n"+
					"Consecutive halted polls: %d\n\n"+
					"%s",
				m.network.Name, s.Status, formatHaltReason(s.Status),
				strings.Join(halted, "\n  • "),
				ratio, warnThreshold, haltThreshold,
				m.consecutiveHaltedPolls,
				haltGuidance(s.Status),
			),
		})
		return
	}

	// Both flags are true — BME operational.
	if m.consecutiveHaltedPolls > 0 {
		m.consecutiveHaltedPolls = 0
		m.alerter.Resolve(
			haltKey,
			fmt.Sprintf("BME OPERATIONAL — %s", m.network.Name),
			fmt.Sprintf(
				"Network: %s\n"+
					"mints_allowed and refunds_allowed are both TRUE — BME is operational.\n"+
					"Collateral Ratio: %.6f\n"+
					"BME Status: %s",
				m.network.Name, ratio, s.Status,
			),
		)
	}
}

// formatHaltReason converts a BME status string to a human-readable halt reason.
// The status field encodes why the chain halted operations, not just that it did.
func formatHaltReason(status string) string {
	switch status {
	case "mint_status_halt_oracle":
		return "Oracle price staleness — oracle data was unavailable at this block"
	case "mint_status_halt_collateral":
		return "Collateral ratio breach — ratio fell below the halt threshold"
	case "mint_status_warn":
		return "Collateral ratio warning — ratio is below the warn threshold"
	case "mint_status_healthy":
		return "Healthy"
	default:
		return status
	}
}

// haltGuidance returns context-appropriate guidance based on halt reason.
func haltGuidance(status string) string {
	switch status {
	case "mint_status_halt_oracle":
		return "This is typically transient — the oracle price was stale for one or more\n" +
			"blocks. If this alert persists across multiple polls, investigate the\n" +
			"oracle price feed and hermes relayer health."
	case "mint_status_halt_collateral":
		return "The collateral ratio has fallen below the halt threshold. The AKT\n" +
			"burn/mint mechanism is non-functional until the ratio recovers.\n" +
			"This directly impacts BME provider revenue and lease pricing.\n" +
			"Immediate investigation required."
	default:
		return "Investigate the BME module status on the chain."
	}
}
