package bme

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
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
	network config.NetworkConfig
	cfg     config.BMEConfig
	alerter *alerting.Slack
	logger  *slog.Logger
	client  *http.Client
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
		m.logger.Error("BME status endpoint unreachable", "url", url, "error", err)
		m.alerter.Send(types.Alert{
			Key:      fmt.Sprintf("bme_unreachable_%s", m.network.Name),
			Severity: types.SeverityWarning,
			Title:    fmt.Sprintf("BME STATUS UNREACHABLE — %s", m.network.Name),
			Body: fmt.Sprintf(
				"Network: %s\n"+
					"Cannot reach BME status endpoint.\n"+
					"URL: %s\n"+
					"Error: %s\n\n"+
					"BME health cannot be verified while this endpoint is down.",
				m.network.Name, url, err.Error(),
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

	// Endpoint is responding — clear any outstanding unreachable alert.
	m.alerter.Resolve(
		fmt.Sprintf("bme_unreachable_%s", m.network.Name),
		fmt.Sprintf("BME STATUS REACHABLE — %s", m.network.Name),
		fmt.Sprintf("Network: %s\nBME status endpoint is responding again.", m.network.Name),
	)

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

	// --- Check 2: Minting halted ---
	mintsKey := fmt.Sprintf("bme_mints_halted_%s", m.network.Name)
	if !s.MintsAllowed {
		m.alerter.Send(types.Alert{
			Key:      mintsKey,
			Severity: types.SeverityCritical,
			Title:    fmt.Sprintf("BME MINTING HALTED — %s", m.network.Name),
			Body: fmt.Sprintf(
				"Network: %s\n"+
					"BME Status: %s\n\n"+
					"mints_allowed is FALSE — the BME module has halted new minting.\n\n"+
					"Collateral Ratio: %.6f\n"+
					"Halt Threshold:   %.6f\n\n"+
					"The AKT burn/mint mechanism is non-functional until the collateral\n"+
					"ratio recovers above the halt threshold. This directly impacts\n"+
					"BME provider revenue and lease pricing.",
				m.network.Name, s.Status,
				ratio, haltThreshold,
			),
		})
	} else {
		m.alerter.Resolve(
			mintsKey,
			fmt.Sprintf("BME MINTING RESUMED — %s", m.network.Name),
			fmt.Sprintf(
				"Network: %s\n"+
					"mints_allowed is TRUE — minting has resumed.\n"+
					"Collateral Ratio: %.6f",
				m.network.Name, ratio,
			),
		)
	}

	// --- Check 3: Refunds halted ---
	refundsKey := fmt.Sprintf("bme_refunds_halted_%s", m.network.Name)
	if !s.RefundsAllowed {
		m.alerter.Send(types.Alert{
			Key:      refundsKey,
			Severity: types.SeverityCritical,
			Title:    fmt.Sprintf("BME REFUNDS HALTED — %s", m.network.Name),
			Body: fmt.Sprintf(
				"Network: %s\n"+
					"BME Status: %s\n\n"+
					"refunds_allowed is FALSE — the BME module has halted refunds.\n\n"+
					"Collateral Ratio: %.6f\n"+
					"Warn Threshold:   %.6f\n\n"+
					"Tenants closing deployments will not receive collateral refunds\n"+
					"until this condition clears.",
				m.network.Name, s.Status,
				ratio, warnThreshold,
			),
		})
	} else {
		m.alerter.Resolve(
			refundsKey,
			fmt.Sprintf("BME REFUNDS RESUMED — %s", m.network.Name),
			fmt.Sprintf(
				"Network: %s\n"+
					"refunds_allowed is TRUE — refunds have resumed.\n"+
					"Collateral Ratio: %.6f",
				m.network.Name, ratio,
			),
		)
	}
}
