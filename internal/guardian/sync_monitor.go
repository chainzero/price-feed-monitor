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

// networkState tracks per-network failure state for the Akash oracle params query.
type networkState struct {
	consecutiveFailures int
}

// SyncMonitor implements Component 3: Guardian Set Currency.
//
// On each cycle it:
//  1. Queries the Ethereum Wormhole contract for the authoritative guardian set
//  2. Queries each configured Akash network's oracle params
//  3. Alerts if the index has changed (rotation detected) or addresses don't match
//
// Data source reliability:
//   - Tracks consecutive failures reaching the Ethereum RPC and alerts operators
//     so a silent RPC outage doesn't mask a real guardian rotation.
//   - Same tracking for each Akash oracle params endpoint.
type SyncMonitor struct {
	cfg            config.GuardianSetConfig
	eth            *EthereumClient
	networks       []config.NetworkConfig
	netStates      map[string]*networkState
	alerter        alerting.Alerter
	logger         *slog.Logger
	lastKnownIndex uint32
	initialized    bool
	ethFailures    int
}

func NewSyncMonitor(
	cfg config.GuardianSetConfig,
	networks []config.NetworkConfig,
	alerter alerting.Alerter,
	logger *slog.Logger,
) *SyncMonitor {
	states := make(map[string]*networkState, len(networks))
	for _, n := range networks {
		states[n.Name] = &networkState{}
	}
	return &SyncMonitor{
		cfg:       cfg,
		eth:       NewEthereumClient(cfg.EthereumRPC, cfg.WormholeContract),
		networks:  networks,
		netStates: states,
		alerter:   alerter,
		logger:    logger.With("component", "guardian_sync"),
	}
}

// Run starts the polling loop. Blocks until ctx is cancelled.
func (m *SyncMonitor) Run(ctx context.Context) {
	m.logger.Info("guardian set monitor started",
		"poll_interval", m.cfg.PollInterval.Duration,
		"ethereum_rpc", m.cfg.EthereumRPC,
		"wormhole_contract", m.cfg.WormholeContract,
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

func (m *SyncMonitor) check(ctx context.Context) {
	// --- Step 1: Query Ethereum (source of truth) ---
	currentIndex, err := m.eth.GetGuardianSetIndex(ctx)
	if err != nil {
		m.ethFailures++
		m.logger.Error("failed to fetch Ethereum guardian set index",
			"consecutive_failures", m.ethFailures,
			"error", err,
		)

		var sev types.Severity
		var title string
		switch m.ethFailures {
		case 1:
			sev = types.SeverityWarning
			title = "ETHEREUM RPC UNREACHABLE — WARNING"
		case 2:
			sev = types.SeverityCritical
			title = "ETHEREUM RPC UNREACHABLE — CRITICAL"
		case 3:
			sev = types.SeverityEmergency
			title = "ETHEREUM RPC UNREACHABLE — EMERGENCY (final alert)"
		default:
			return
		}

		m.alerter.Send(types.Alert{
			Key:      "ethereum_rpc_unreachable",
			Severity: sev,
			Title:    title,
			Body: fmt.Sprintf(
				"Cannot reach Ethereum RPC to verify guardian set currency.\n"+
					"RPC: %s\n"+
					"Consecutive failures: %d\n"+
					"Error: %s\n\n"+
					"Risk: A guardian rotation may go undetected while this endpoint is down.\n"+
					"Action required: Check RPC endpoint configuration.\n"+
					"No further alerts will be sent until the endpoint recovers.",
				m.cfg.EthereumRPC, m.ethFailures, err.Error(),
			),
		})
		return
	}

	// RPC is back up — clear any outstanding alert.
	if m.ethFailures > 0 {
		m.ethFailures = 0
		m.alerter.Resolve(
			"ethereum_rpc_unreachable",
			"ETHEREUM RPC REACHABLE",
			fmt.Sprintf("RPC endpoint is responding again.\nRPC: %s", m.cfg.EthereumRPC),
		)
	}

	// --- Step 2: Detect index change (rotation) ---
	if m.initialized && currentIndex > m.lastKnownIndex {
		m.logger.Warn("guardian set index changed — rotation detected",
			"previous_index", m.lastKnownIndex,
			"new_index", currentIndex,
		)
		m.alerter.Send(types.Alert{
			Key:      "guardian_set_rotation",
			Severity: types.SeverityCritical,
			Title:    "WORMHOLE GUARDIAN ROTATION DETECTED",
			Body: fmt.Sprintf(
				"The Wormhole mainnet guardian set has rotated.\n\n"+
					"Previous Index: %d\n"+
					"New Index: %d\n\n"+
					"Action Required:\n"+
					"Submit governance proposal to update guardian addresses.\n"+
					"Fetching new addresses now — sync check will follow.",
				m.lastKnownIndex, currentIndex,
			),
		})
	} else if !m.initialized {
		m.logger.Info("guardian set baseline established", "index", currentIndex)
	}

	m.lastKnownIndex = currentIndex
	m.initialized = true

	// --- Step 3: Fetch the authoritative address list ---
	ethAddresses, err := m.eth.GetGuardianSet(ctx, currentIndex)
	if err != nil {
		m.logger.Error("failed to fetch Ethereum guardian set addresses",
			"index", currentIndex, "error", err)
		return
	}

	m.logger.Info("Ethereum guardian set fetched",
		"index", currentIndex,
		"address_count", len(ethAddresses),
	)

	// --- Step 4: Compare against each network's Akash oracle params ---
	for _, network := range m.networks {
		m.compareWithNetwork(ctx, network, currentIndex, ethAddresses)
	}
}

func (m *SyncMonitor) compareWithNetwork(
	ctx context.Context,
	network config.NetworkConfig,
	ethIndex uint32,
	ethAddresses []string,
) {
	state := m.netStates[network.Name]
	syncAlertKey := fmt.Sprintf("guardian_sync_%s", network.Name)
	akashRPCKey := fmt.Sprintf("akash_params_unreachable_%s", network.Name)

	akashClient := NewAkashOracleClient(network.AkashAPINodes, network.Name, network.WormholeContract)
	akashAddresses, err := akashClient.GetGuardianAddresses(ctx)
	if err != nil {
		state.consecutiveFailures++
		m.logger.Error("failed to fetch Akash oracle params",
			"network", network.Name,
			"consecutive_failures", state.consecutiveFailures,
			"error", err,
		)

		// Alert only on the first 3 consecutive failures with escalating severity.
		// After that, stay silent until the endpoint recovers — no channel flooding.
		var sev types.Severity
		var title string
		switch state.consecutiveFailures {
		case 1:
			sev = types.SeverityWarning
			title = "AKASH ORACLE PARAMS UNREACHABLE — WARNING"
		case 2:
			sev = types.SeverityCritical
			title = "AKASH ORACLE PARAMS UNREACHABLE — CRITICAL"
		case 3:
			sev = types.SeverityEmergency
			title = "AKASH ORACLE PARAMS UNREACHABLE — EMERGENCY (final alert)"
		default:
			// Silent beyond 3 failures — already alerted, waiting for recovery.
			return
		}

		m.alerter.Send(types.Alert{
			Key:      akashRPCKey,
			Severity: sev,
			Title:    title,
			Body: fmt.Sprintf(
				"Network: %s\n"+
					"Cannot fetch oracle params to verify guardian set sync.\n"+
					"Nodes tried: %s\n"+
					"Consecutive failures: %d\n"+
					"Error: %s\n\n"+
					"Guardian set sync cannot be verified while all nodes are down.\n"+
					"No further alerts will be sent until an endpoint recovers.",
				network.Name, strings.Join(network.AkashAPINodes, ", "),
				state.consecutiveFailures, err.Error(),
			),
		})
		return
	}

	if state.consecutiveFailures > 0 {
		state.consecutiveFailures = 0
		m.alerter.Resolve(
			akashRPCKey,
			"AKASH ORACLE PARAMS REACHABLE",
			fmt.Sprintf("Network: %s\nOracle params endpoint is responding again.", network.Name),
		)
	}

	// --- Compare addresses positionally ---
	mismatches := findMismatches(ethAddresses, akashAddresses)
	countMismatch := len(ethAddresses) != len(akashAddresses)

	m.logger.Info("guardian set comparison",
		"network", network.Name,
		"ethereum_index", ethIndex,
		"ethereum_count", len(ethAddresses),
		"akash_count", len(akashAddresses),
		"positional_mismatches", len(mismatches),
	)

	if len(mismatches) > 0 || countMismatch {
		var body strings.Builder
		fmt.Fprintf(&body, "Network: %s\nEthereum Guardian Index: %d\n\n", network.Name, ethIndex)
		fmt.Fprintf(&body, "⚠️  Akash oracle params are OUT OF SYNC with Wormhole mainnet.\n")
		fmt.Fprintf(&body, "Price feed signature verification WILL FAIL until params are updated.\n\n")

		if countMismatch {
			fmt.Fprintf(&body, "Count mismatch: Ethereum=%d  Akash=%d\n\n",
				len(ethAddresses), len(akashAddresses))
		}
		if len(mismatches) > 0 {
			fmt.Fprintf(&body, "Changed Addresses:\n")
			for _, mm := range mismatches {
				fmt.Fprintf(&body, "%s\n", mm)
			}
			fmt.Fprintf(&body, "\n")
		}

		fmt.Fprintf(&body, "Current Ethereum Guardian Set (Index %d):\n", ethIndex)
		for _, addr := range ethAddresses {
			fmt.Fprintf(&body, "  %s\n", addr)
		}

		fmt.Fprintf(&body, "\nAction Required:\n")
		fmt.Fprintf(&body, "Submit governance proposal to update guardian addresses.\n")
		fmt.Fprintf(&body, "See: hermes-relayer-setup-guide.md#wormhole-guardian-set-management")

		m.alerter.Send(types.Alert{
			Key:      syncAlertKey,
			Severity: types.SeverityEmergency,
			Title:    "GUARDIAN SET OUT OF SYNC",
			Body:     body.String(),
		})
	} else {
		m.logger.Info("guardian set in sync", "network", network.Name, "index", ethIndex)
		m.alerter.Resolve(
			syncAlertKey,
			"GUARDIAN SET IN SYNC",
			fmt.Sprintf(
				"Network: %s\n\n"+
					"Akash oracle params match Wormhole mainnet.\n"+
					"Guardian Set Index: %d — all %d addresses verified.",
				network.Name, ethIndex, len(ethAddresses),
			),
		)
	}
}

// findMismatches compares two address lists positionally and returns
// human-readable descriptions of any differences.
func findMismatches(eth, akash []string) []string {
	var mismatches []string
	maxLen := len(eth)
	if len(akash) > maxLen {
		maxLen = len(akash)
	}
	for i := 0; i < maxLen; i++ {
		var ethAddr, akashAddr string
		if i < len(eth) {
			ethAddr = eth[i]
		}
		if i < len(akash) {
			akashAddr = strings.ToLower(akash[i])
		}
		if ethAddr != akashAddr {
			mismatches = append(mismatches, fmt.Sprintf(
				"  Position %d:\n    was:  %s\n    now:  %s",
				i, orMissing(akashAddr), orMissing(ethAddr),
			))
		}
	}
	return mismatches
}

func orMissing(s string) string {
	if s == "" {
		return "(missing)"
	}
	return s
}
