package guardian

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/akash-network/price-feed-monitor/internal/alerting"
	"github.com/akash-network/price-feed-monitor/internal/config"
	"github.com/akash-network/price-feed-monitor/internal/types"
)

// WormholescanMonitor implements Component 5: Reactive Wormhole Guardian Set Monitor.
//
// This monitor runs alongside Component 3 (Ethereum RPC) to provide defense-in-depth
// detection of guardian set rotations. The two components are complementary:
//
//	Component 3 (Ethereum RPC)  — Authoritative address-level verification.
//	                               Confirms the exact 19 guardian addresses match.
//	                               Requires Ethereum RPC connectivity.
//
//	Component 5 (Wormholescan) — Reactive index change detection + VAA retrieval.
//	                               Provides the signed governance VAA needed to
//	                               update the Akash contract. No Ethereum RPC needed.
//
// Rotation response sequence:
//  1. Wormhole governance executes a guardian set upgrade on Ethereum.
//  2. A governance VAA is published and indexed by Wormholescan.
//  3. This monitor detects the global index increase on the next poll.
//  4. Retrieves the governance VAA and includes it in the Slack alert.
//  5. The team submits the VAA to the Akash contract via submit_v_a_a.
//  6. Once Akash oracle params update, this monitor sends a resolved message.
//
// Grace period: The Akash Wormhole contract's guardian_set_expiry is 86400 seconds
// (24 hours). Polling every 15–60 minutes ensures detection well within this window.
type WormholescanMonitor struct {
	cfg                 config.WormholescanConfig
	networks            []config.NetworkConfig
	wormholescan        *WormholescanClient
	alerter             alerting.Alerter
	logger              *slog.Logger
	lastKnownIndex      uint32
	initialized         bool
	consecutiveFailures int
}

func NewWormholescanMonitor(
	cfg config.WormholescanConfig,
	networks []config.NetworkConfig,
	alerter alerting.Alerter,
	logger *slog.Logger,
) *WormholescanMonitor {
	return &WormholescanMonitor{
		cfg:          cfg,
		networks:     networks,
		wormholescan: NewWormholescanClient(cfg.APIBaseURL),
		alerter:      alerter,
		logger:       logger.With("component", "wormholescan_monitor"),
	}
}

// Run starts the polling loop. Blocks until ctx is cancelled.
func (m *WormholescanMonitor) Run(ctx context.Context) {
	m.logger.Info("wormholescan guardian set monitor started",
		"poll_interval", m.cfg.PollInterval.Duration,
		"api_base_url", m.cfg.APIBaseURL,
		"governance_emitter", m.cfg.GovernanceEmitter,
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

func (m *WormholescanMonitor) check(ctx context.Context) {
	// --- Step 1: Get the current global guardian set index and addresses ---
	//
	// This is the authoritative global state from Wormholescan. A single REST call
	// gives us both the index (for change detection) and the addresses (for comparing
	// against what Akash currently has configured).
	globalIndex, globalAddresses, err := m.wormholescan.GetCurrentGuardianSet(ctx)
	if err != nil {
		m.consecutiveFailures++
		m.logger.Error("failed to fetch current guardian set from Wormholescan",
			"consecutive_failures", m.consecutiveFailures,
			"error", err,
		)

		var sev types.Severity
		var title string
		switch m.consecutiveFailures {
		case 1:
			sev = types.SeverityWarning
			title = "WORMHOLESCAN API UNREACHABLE — WARNING"
		case 2:
			sev = types.SeverityCritical
			title = "WORMHOLESCAN API UNREACHABLE — CRITICAL"
		case 3:
			sev = types.SeverityEmergency
			title = "WORMHOLESCAN API UNREACHABLE — EMERGENCY (final alert)"
		default:
			return
		}

		m.alerter.Send(types.Alert{
			Key:      "wormholescan_unreachable",
			Severity: sev,
			Title:    title,
			Body: fmt.Sprintf(
				"Cannot reach Wormholescan to verify guardian set index.\n"+
					"API: %s\n"+
					"Consecutive failures: %d\n"+
					"Error: %s\n\n"+
					"Risk: A guardian rotation may go undetected while this endpoint is down.\n"+
					"Component 3 (Ethereum RPC) continues to provide address-level monitoring.\n"+
					"No further alerts will be sent until the endpoint recovers.",
				m.cfg.APIBaseURL, m.consecutiveFailures, err.Error(),
			),
		})
		return
	}

	// API is responding — reset failure count and clear any outstanding alert.
	if m.consecutiveFailures > 0 {
		m.consecutiveFailures = 0
		m.alerter.Resolve(
			"wormholescan_unreachable",
			"WORMHOLESCAN API REACHABLE",
			fmt.Sprintf("API endpoint is responding again.\nAPI: %s", m.cfg.APIBaseURL),
		)
	}

	m.logger.Info("wormholescan guardian set fetched",
		"global_index", globalIndex,
		"address_count", len(globalAddresses),
	)

	// --- Step 2: Detect guardian set index increase (rotation event) ---
	//
	// Wormhole guardian set indices are always monotonically increasing. An increase
	// means a governance vote has completed and the new guardian set is globally active.
	// The old guardian set expires after the guardian_set_expiry grace period (24h).
	if m.initialized && globalIndex > m.lastKnownIndex {
		m.logger.Warn("guardian set rotation detected via Wormholescan",
			"previous_index", m.lastKnownIndex,
			"new_index", globalIndex,
		)
		m.sendRotationAlert(ctx, m.lastKnownIndex, globalIndex)
	} else if !m.initialized {
		m.logger.Info("wormholescan guardian set baseline established", "index", globalIndex)
	}

	m.lastKnownIndex = globalIndex
	m.initialized = true

	// --- Step 3: Verify each Akash network is using the current guardian set ---
	//
	// Even without a fresh rotation event, compare Wormholescan's current addresses
	// against each network's Akash oracle params. This catches the case where the
	// monitor restarted after a rotation or the team partially applied an update.
	for _, network := range m.networks {
		m.compareWithNetwork(ctx, network, globalIndex, globalAddresses)
	}
}

// sendRotationAlert fetches the governance VAA for the new index and posts a CRITICAL
// alert containing the truncated VAA and a retrieval link.
func (m *WormholescanMonitor) sendRotationAlert(ctx context.Context, previousIndex, newIndex uint32) {
	vaaBase64, vaaTimestamp, vaaErr := m.wormholescan.GetUpgradeVAA(ctx, m.cfg.GovernanceEmitter, newIndex)

	var body strings.Builder

	fmt.Fprintf(&body,
		"The Wormhole global guardian set has rotated.\n\n"+
			"Previous Index: %d\n"+
			"New Index:      %d\n\n",
		previousIndex, newIndex,
	)

	// Include VAA information in the alert. The VAA is the exact signed payload
	// required by the Akash Wormhole contract's submit_v_a_a execution message.
	// We truncate to 64 characters here to keep the alert readable; the full VAA
	// is available via the Wormholescan retrieval link below.
	if vaaErr != nil {
		m.logger.Warn("could not retrieve upgrade VAA", "error", vaaErr)
		fmt.Fprintf(&body,
			"⚠️  Could not retrieve upgrade VAA: %s\n"+
				"Retrieve manually from:\n"+
				"  https://wormholescan.io/guardianset\n\n",
			vaaErr.Error(),
		)
	} else {
		// Truncate VAA to first 64 base64 characters (≈48 raw bytes) for readability.
		// The full VAA is ~1–2 KB of base64; showing just the header is sufficient
		// to confirm which VAA this is without flooding the Slack message.
		truncated := vaaBase64
		if len(truncated) > 64 {
			truncated = truncated[:64] + "..."
		}
		fmt.Fprintf(&body, "Governance VAA (truncated):\n  %s\n\n", truncated)
		if vaaTimestamp != "" {
			fmt.Fprintf(&body, "VAA Published: %s\n", vaaTimestamp)
		}
	}

	// Wormholescan VAA retrieval link — anyone on the team can use this to get
	// the full base64 VAA without needing any tooling:
	//   curl https://api.wormholescan.io/api/v1/vaas/1/{emitter} | jq '.data[0].vaa'
	fmt.Fprintf(&body,
		"Full VAA retrieval:\n"+
			"  https://wormholescan.io/guardianset\n"+
			"  curl -s \"https://api.wormholescan.io/api/v1/vaas/1/%s?pageSize=5\" | jq -r '.data[0].vaa'\n\n",
		m.cfg.GovernanceEmitter,
	)

	fmt.Fprintf(&body,
		"Action Required:\n"+
			"1. Retrieve the full VAA using the curl command above.\n"+
			"2. Submit to the Akash Wormhole contract:\n"+
			"     akash tx wasm execute <contract> '{\"submit_v_a_a\":{\"vaa\":\"<base64>\"}}' \\\n"+
			"       --from <key> --chain-id akashnet-2 --gas auto\n"+
			"3. Confirm oracle params update via /akash/oracle/v1/params.\n\n"+
			"Grace Period: 86400 seconds (24 hours) from VAA publication.\n"+
			"Price feed verification WILL FAIL once the old guardian set expires.",
	)

	m.alerter.Send(types.Alert{
		Key:      "wormholescan_guardian_rotation",
		Severity: types.SeverityCritical,
		Title:    fmt.Sprintf("GUARDIAN SET ROTATION: INDEX %d → %d", previousIndex, newIndex),
		Body:     body.String(),
	})
}

// compareWithNetwork checks whether a specific Akash network's oracle params are using
// the current globally active guardian set addresses.
//
// This runs on every poll cycle (not just on rotation events), so it will catch drift
// even after a monitor restart or if a rotation was missed during downtime.
func (m *WormholescanMonitor) compareWithNetwork(
	ctx context.Context,
	network config.NetworkConfig,
	globalIndex uint32,
	globalAddresses []string,
) {
	alertKey := fmt.Sprintf("wormholescan_sync_%s", network.Name)

	akashClient := NewAkashOracleClient(network.AkashAPI, network.Name, network.WormholeContract)
	akashAddresses, err := akashClient.GetGuardianAddresses(ctx)
	if err != nil {
		// Akash oracle params unreachable — Component 3 also handles this alert,
		// so we only log here to avoid duplicate Slack notifications.
		m.logger.Warn("could not fetch Akash oracle params for Wormholescan comparison",
			"network", network.Name,
			"error", err,
		)
		return
	}

	// Compare Wormholescan's current global addresses against Akash's stored addresses.
	// Both sets are normalized to lowercase without "0x" prefix before comparison.
	mismatches := findMismatches(globalAddresses, akashAddresses)
	countMismatch := len(globalAddresses) != len(akashAddresses)

	m.logger.Info("wormholescan vs akash guardian set comparison",
		"network", network.Name,
		"global_index", globalIndex,
		"global_count", len(globalAddresses),
		"akash_count", len(akashAddresses),
		"positional_mismatches", len(mismatches),
	)

	if len(mismatches) == 0 && !countMismatch {
		// Akash oracle params match the current global guardian set — all good.
		m.logger.Info("Akash guardian set matches Wormholescan global set",
			"network", network.Name,
			"index", globalIndex,
		)
		m.alerter.Resolve(
			alertKey,
			"GUARDIAN SET SYNCED (Wormholescan confirmed)",
			fmt.Sprintf(
				"Network: %s\n"+
					"Akash oracle params match the current Wormhole global guardian set.\n"+
					"Guardian Set Index: %d — all %d addresses verified.",
				network.Name, globalIndex, len(globalAddresses),
			),
		)
		return
	}

	// Addresses don't match — Akash is behind the global guardian set.
	// This alert fires even if the index hasn't changed on this poll cycle,
	// providing a persistent reminder until the update is applied.
	var body strings.Builder
	fmt.Fprintf(&body,
		"Network: %s\n"+
			"Wormholescan Global Index: %d\n\n"+
			"Akash oracle params are OUT OF SYNC with the current Wormhole guardian set.\n"+
			"Price feed signature verification WILL FAIL once the old guardian set expires.\n\n",
		network.Name, globalIndex,
	)

	if countMismatch {
		fmt.Fprintf(&body, "Count mismatch: Global=%d  Akash=%d\n\n",
			len(globalAddresses), len(akashAddresses))
	}
	if len(mismatches) > 0 {
		fmt.Fprintf(&body, "Changed Addresses:\n")
		for _, mm := range mismatches {
			fmt.Fprintf(&body, "%s\n", mm)
		}
		fmt.Fprintf(&body, "\n")
	}

	fmt.Fprintf(&body,
		"Retrieve the upgrade VAA:\n"+
			"  curl -s \"https://api.wormholescan.io/api/v1/vaas/1/%s?pageSize=5\" | jq -r '.data[0].vaa'\n\n"+
			"Then apply via submit_v_a_a — see rotation alert for full instructions.",
		m.cfg.GovernanceEmitter,
	)

	m.alerter.Send(types.Alert{
		Key:      alertKey,
		Severity: types.SeverityCritical,
		Title:    fmt.Sprintf("GUARDIAN SET OUT OF SYNC (Wormholescan) — %s", network.Name),
		Body:     body.String(),
	})
}
