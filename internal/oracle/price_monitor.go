package oracle

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

// PriceMonitor implements Component 1: Oracle Price Health.
//
// It polls the Akash oracle REST API for the latest AKT/USD price timestamp
// and raises Slack alerts when prices become stale. It also monitors the
// Hermes wallet balance for each configured relayer on the network.
type PriceMonitor struct {
	network config.NetworkConfig
	cfg     config.OraclePriceConfig
	alerter *alerting.Slack
	logger  *slog.Logger
	client  *http.Client
}

func NewPriceMonitor(
	network config.NetworkConfig,
	cfg config.OraclePriceConfig,
	alerter *alerting.Slack,
	logger *slog.Logger,
) *PriceMonitor {
	return &PriceMonitor{
		network: network,
		cfg:     cfg,
		alerter: alerter,
		logger:  logger.With("network", network.Name, "component", "oracle_price"),
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

// Run starts the polling loop. It performs an immediate check on startup,
// then polls at the configured interval. Blocks until ctx is cancelled.
func (m *PriceMonitor) Run(ctx context.Context) {
	m.logger.Info("oracle price monitor started",
		"poll_interval", m.cfg.PollInterval.Duration,
		"warning_age", m.cfg.Thresholds.WarningAge.Duration,
		"critical_age", m.cfg.Thresholds.CriticalAge.Duration,
		"emergency_age", m.cfg.Thresholds.EmergencyAge.Duration,
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

func (m *PriceMonitor) check(ctx context.Context) {
	m.checkPrice(ctx)
	for _, relayer := range m.network.HermesRelayers {
		m.checkWalletBalance(ctx, relayer)
	}
}

// checkPrice fetches the latest oracle price, calculates its age, and fires
// alerts at the Warning / Critical / Emergency thresholds defined in config.
func (m *PriceMonitor) checkPrice(ctx context.Context) {
	price, err := m.fetchLatestPrice(ctx)
	if err != nil {
		m.logger.Error("failed to fetch oracle price", "error", err)
		return
	}

	ts, err := time.Parse(time.RFC3339, price.State.Timestamp)
	if err != nil {
		m.logger.Error("failed to parse price timestamp",
			"timestamp", price.State.Timestamp, "error", err)
		return
	}

	age := time.Since(ts.UTC())
	alertKey := fmt.Sprintf("oracle_price_stale_%s", m.network.Name)

	m.logger.Info("price check",
		"price_usd", price.State.Price,
		"timestamp", price.State.Timestamp,
		"age", age.Round(time.Second),
		"height", price.ID.Height,
	)

	switch {
	case age >= m.cfg.Thresholds.EmergencyAge.Duration:
		m.alerter.Send(types.Alert{
			Key:      alertKey,
			Severity: types.SeverityEmergency,
			Title:    "ORACLE PRICE STALE — EMERGENCY",
			Body: fmt.Sprintf(
				"Network: %s\nSeverity: EMERGENCY\n\n"+
					"Last price update: %s (%s ago)\n"+
					"Last price: $%s AKT/USD\n"+
					"Block height: %s\n\n"+
					"Action required: Price submissions have stalled for >%s. "+
					"The oracle module will start rejecting stale prices.\n"+
					"Check Hermes relayer status immediately:\n"+
					"  docker logs hermes-client\n"+
					"  docker restart hermes-client",
				m.network.Name,
				price.State.Timestamp, formatAge(age),
				price.State.Price,
				price.ID.Height,
				m.cfg.Thresholds.EmergencyAge.Duration,
			),
		})

	case age >= m.cfg.Thresholds.CriticalAge.Duration:
		m.alerter.Send(types.Alert{
			Key:      alertKey,
			Severity: types.SeverityCritical,
			Title:    "ORACLE PRICE STALE — CRITICAL",
			Body: fmt.Sprintf(
				"Network: %s\nSeverity: CRITICAL\n\n"+
					"Last price update: %s (%s ago)\n"+
					"Last price: $%s AKT/USD\n"+
					"Block height: %s\n\n"+
					"Action required: Staleness threshold approaching. "+
					"Check Hermes relayer logs.\n"+
					"  docker logs --tail 50 hermes-client",
				m.network.Name,
				price.State.Timestamp, formatAge(age),
				price.State.Price,
				price.ID.Height,
			),
		})

	case age >= m.cfg.Thresholds.WarningAge.Duration:
		m.alerter.Send(types.Alert{
			Key:      alertKey,
			Severity: types.SeverityWarning,
			Title:    "ORACLE PRICE STALE — WARNING",
			Body: fmt.Sprintf(
				"Network: %s\nSeverity: WARNING\n\n"+
					"Last price update: %s (%s ago)\n"+
					"Last price: $%s AKT/USD\n"+
					"Block height: %s\n\n"+
					"Action required: Check Hermes relayer status.",
				m.network.Name,
				price.State.Timestamp, formatAge(age),
				price.State.Price,
				price.ID.Height,
			),
		})

	default:
		// Prices are healthy — send resolved if we were previously alerting.
		m.alerter.Resolve(
			alertKey,
			"ORACLE PRICE HEALTHY",
			fmt.Sprintf(
				"Network: %s\n\n"+
					"Prices are flowing normally.\n"+
					"Last update: %s (%s ago)\n"+
					"Last price: $%s AKT/USD",
				m.network.Name,
				price.State.Timestamp, formatAge(age),
				price.State.Price,
			),
		)
	}
}

func (m *PriceMonitor) fetchLatestPrice(ctx context.Context) (*types.OraclePrice, error) {
	url := fmt.Sprintf("%s/akash/oracle/v1/prices?pagination.limit=1", m.network.AkashAPI)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d from oracle API", resp.StatusCode)
	}

	var result types.OraclePriceResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode oracle response: %w", err)
	}

	if len(result.Prices) == 0 {
		return nil, fmt.Errorf("oracle API returned no prices")
	}

	return &result.Prices[0], nil
}

// checkWalletBalance verifies the Hermes relayer wallet has enough AKT for gas.
// Three alert tiers (all use the same alert key so severity escalation/de-escalation works):
//
//	< min_wallet_balance  (default 100 AKT)  → Critical
//	< warn_wallet_balance (default 500 AKT)  → Warning
//	< info_wallet_balance (default 1000 AKT) → Info
func (m *PriceMonitor) checkWalletBalance(ctx context.Context, relayer config.RelayerConfig) {
	if relayer.Wallet == "" || relayer.MinWalletBalance == 0 {
		return
	}

	balanceUAKT, err := m.fetchWalletBalance(ctx, relayer.Wallet)
	if err != nil {
		m.logger.Error("failed to fetch wallet balance",
			"relayer", relayer.Name, "wallet", relayer.Wallet, "error", err)
		return
	}

	alertKey := fmt.Sprintf("wallet_balance_low_%s_%s", m.network.Name, relayer.Name)
	balanceAKT := float64(balanceUAKT) / 1_000_000
	minAKT := float64(relayer.MinWalletBalance) / 1_000_000
	warnAKT := float64(relayer.WarnWalletBalance) / 1_000_000
	infoAKT := float64(relayer.InfoWalletBalance) / 1_000_000

	m.logger.Info("wallet balance check",
		"relayer", relayer.Name,
		"balance_akt", fmt.Sprintf("%.2f", balanceAKT),
		"min_akt", fmt.Sprintf("%.2f", minAKT),
	)

	fundCmd := fmt.Sprintf("akash tx bank send default %s 10000000000uakt --from default -y", relayer.Wallet)

	switch {
	case balanceUAKT < relayer.MinWalletBalance:
		m.alerter.Send(types.Alert{
			Key:      alertKey,
			Severity: types.SeverityCritical,
			Title:    fmt.Sprintf("HERMES WALLET CRITICAL — %s", relayer.Name),
			Body: fmt.Sprintf(
				"Network: %s\nRelayer: %s\nWallet: %s\n\n"+
					"Current balance: %.2f AKT\n"+
					"Critical threshold: %.2f AKT  ← BREACHED\n\n"+
					"Action required immediately: Fund the wallet to prevent gas failures.\n"+
					"  %s",
				m.network.Name, relayer.Name, relayer.Wallet,
				balanceAKT, minAKT, fundCmd,
			),
		})

	case relayer.WarnWalletBalance > 0 && balanceUAKT < relayer.WarnWalletBalance:
		m.alerter.Send(types.Alert{
			Key:      alertKey,
			Severity: types.SeverityWarning,
			Title:    fmt.Sprintf("HERMES WALLET LOW — %s", relayer.Name),
			Body: fmt.Sprintf(
				"Network: %s\nRelayer: %s\nWallet: %s\n\n"+
					"Current balance: %.2f AKT\n"+
					"Warning threshold: %.2f AKT  ← BREACHED\n"+
					"Critical threshold: %.2f AKT\n\n"+
					"Fund the wallet soon to avoid hitting the critical threshold.\n"+
					"  %s",
				m.network.Name, relayer.Name, relayer.Wallet,
				balanceAKT, warnAKT, minAKT, fundCmd,
			),
		})

	case relayer.InfoWalletBalance > 0 && balanceUAKT < relayer.InfoWalletBalance:
		m.alerter.Send(types.Alert{
			Key:      alertKey,
			Severity: types.SeverityInfo,
			Title:    fmt.Sprintf("HERMES WALLET NOTICE — %s", relayer.Name),
			Body: fmt.Sprintf(
				"Network: %s\nRelayer: %s\nWallet: %s\n\n"+
					"Current balance: %.2f AKT\n"+
					"Info threshold: %.2f AKT  ← BREACHED\n"+
					"Warning threshold: %.2f AKT\n"+
					"Critical threshold: %.2f AKT\n\n"+
					"Balance is declining — consider funding soon.\n"+
					"  %s",
				m.network.Name, relayer.Name, relayer.Wallet,
				balanceAKT, infoAKT, warnAKT, minAKT, fundCmd,
			),
		})

	default:
		m.alerter.Resolve(
			alertKey,
			fmt.Sprintf("HERMES WALLET HEALTHY — %s", relayer.Name),
			fmt.Sprintf(
				"Network: %s\nRelayer: %s\n\n"+
					"Balance: %.2f AKT (critical threshold: %.2f AKT)",
				m.network.Name, relayer.Name, balanceAKT, minAKT,
			),
		)
	}
}

func (m *PriceMonitor) fetchWalletBalance(ctx context.Context, address string) (int64, error) {
	url := fmt.Sprintf("%s/cosmos/bank/v1beta1/balances/%s", m.network.AkashAPI, address)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}

	resp, err := m.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("unexpected status %d from bank API", resp.StatusCode)
	}

	var result types.WalletBalanceResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, fmt.Errorf("decode bank response: %w", err)
	}

	for _, b := range result.Balances {
		if b.Denom == "uakt" {
			return strconv.ParseInt(b.Amount, 10, 64)
		}
	}

	return 0, nil // wallet has no uakt balance
}

// formatAge renders a duration as a human-readable string, e.g. "7m 23s".
func formatAge(d time.Duration) string {
	d = d.Round(time.Second)
	minutes := int(d.Minutes())
	seconds := int(d.Seconds()) % 60
	if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}
