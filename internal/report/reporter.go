package report

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/akash-network/price-feed-monitor/internal/alerting"
	"github.com/akash-network/price-feed-monitor/internal/config"
	"github.com/akash-network/price-feed-monitor/internal/guardian"
	"github.com/akash-network/price-feed-monitor/internal/types"
)

// Reporter posts startup and scheduled health summaries to Slack.
// It makes its own HTTP calls so it can report status independently
// of the running monitor goroutines.
type Reporter struct {
	cfg     *config.Config
	alerter *alerting.Slack
	logger  *slog.Logger
	client  *http.Client
}

func New(cfg *config.Config, alerter *alerting.Slack, logger *slog.Logger) *Reporter {
	return &Reporter{
		cfg:     cfg,
		alerter: alerter,
		logger:  logger.With("component", "reporter"),
		client:  &http.Client{Timeout: 10 * time.Second},
	}
}

// PostStartup sends the startup summary immediately. Called once from main
// after all monitor goroutines have been launched.
func (r *Reporter) PostStartup(ctx context.Context) {
	r.post(ctx, "🔄 BME Price Feed Monitor Restarted")
}

// RunDailySchedule blocks until ctx is cancelled, posting health summaries
// at each time configured in report.schedule_times (default: ["08:00"]).
func (r *Reporter) RunDailySchedule(ctx context.Context) {
	tz := r.cfg.Report.Timezone
	if tz == "" {
		tz = "America/Chicago"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		r.logger.Error("failed to load timezone", "timezone", tz, "error", err)
		return
	}

	times := r.cfg.Report.ScheduleTimes
	if len(times) == 0 {
		times = []string{"08:00"}
	}

	var wg sync.WaitGroup
	for _, t := range times {
		hour, minute, err := parseHHMM(t)
		if err != nil {
			r.logger.Error("invalid schedule time, skipping", "time", t, "error", err)
			continue
		}
		wg.Add(1)
		go func(label string, h, m int) {
			defer wg.Done()
			for {
				delay := durationUntilNext(h, m, loc)
				r.logger.Info("daily health check scheduled", "time", label, "in", delay.Round(time.Minute))
				select {
				case <-ctx.Done():
					return
				case <-time.After(delay):
					r.post(ctx, "📊 BME Price Feed Monitor — Daily Health Check")
				}
			}
		}(t, hour, minute)
	}
	wg.Wait()
}

// post fetches live status and sends the formatted summary to Slack.
func (r *Reporter) post(ctx context.Context, header string) {
	var b strings.Builder
	fmt.Fprintf(&b, "Time: %s UTC\n\n", time.Now().UTC().Format("2006-01-02 15:04:05"))

	// Fetch the Wormholescan global guardian set once — shared across all networks.
	var globalGuardianIndex uint32
	var globalGuardianAddresses []string
	var globalGuardianErr error
	if r.cfg.WormholescanMonitor.Enabled {
		wsClient := guardian.NewWormholescanClient(r.cfg.WormholescanMonitor.APIBaseURL)
		globalGuardianIndex, globalGuardianAddresses, globalGuardianErr = wsClient.GetCurrentGuardianSet(ctx)
	}

	for _, network := range r.cfg.Networks {
		fmt.Fprintf(&b, "━━━ Network: %s ━━━\n\n", network.Name)

		// Oracle price
		price, priceAge, err := r.fetchOraclePrice(ctx, network.AkashAPI)
		if err != nil {
			fmt.Fprintf(&b, "Oracle Price: ❌ unreachable (%s)\n\n", err)
		} else {
			priceStatus := "✅"
			if priceAge >= 15*time.Minute {
				priceStatus = "🚨"
			} else if priceAge >= 5*time.Minute {
				priceStatus = "⚠️"
			}
			fmt.Fprintf(&b, "Oracle Price: %s $%.4f AKT/USD  (age: %s)\n\n",
				priceStatus, price, formatAge(priceAge))
		}

		// Hermes relayers
		fmt.Fprintf(&b, "Hermes Relayers:\n")
		for _, relayer := range network.HermesRelayers {
			r.appendRelayerStatus(ctx, &b, network, relayer)
		}
		fmt.Fprintln(&b)

		// BME status
		if r.cfg.BMEMonitor.Enabled {
			r.appendBMEStatus(ctx, &b, network.AkashAPI)
		}

		// Guardian set status
		r.appendGuardianStatus(ctx, &b, network,
			globalGuardianIndex, globalGuardianAddresses, globalGuardianErr)
	}

	// Pyth forum monitoring note
	if r.cfg.AnnouncementMonitor.Enabled && r.cfg.AnnouncementMonitor.PythForum.Enabled {
		fmt.Fprintf(&b, "\nPyth Forum: ✅ monitoring active\n")
	}

	r.alerter.Post(header, b.String())
}

func (r *Reporter) appendRelayerStatus(
	ctx context.Context,
	b *strings.Builder,
	network config.NetworkConfig,
	relayer config.RelayerConfig,
) {
	health, err := r.fetchHealth(ctx, relayer.HealthEndpoint)
	if err != nil {
		fmt.Fprintf(b, "  • %s: ❌ unreachable (%s)\n", relayer.Name, err)
		return
	}

	runningIcon := "✅"
	runningLabel := "running"
	if !health.IsRunning {
		runningIcon = "🔴"
		runningLabel = "stopped"
	}

	walletStr := ""
	if relayer.Wallet != "" {
		balanceUAKT, err := r.fetchWalletBalance(ctx, network.AkashAPI, relayer.Wallet)
		if err != nil {
			walletStr = "wallet: ❌ unavailable"
		} else {
			balanceAKT := float64(balanceUAKT) / 1_000_000
			balanceIcon := "✅"
			switch {
			case relayer.MinWalletBalance > 0 && balanceUAKT < relayer.MinWalletBalance:
				balanceIcon = "🔴"
			case relayer.WarnWalletBalance > 0 && balanceUAKT < relayer.WarnWalletBalance:
				balanceIcon = "⚠️"
			case relayer.InfoWalletBalance > 0 && balanceUAKT < relayer.InfoWalletBalance:
				balanceIcon = "ℹ️"
			}
			walletStr = fmt.Sprintf("wallet: %s %.0f AKT", balanceIcon, balanceAKT)
		}
	}

	fmt.Fprintf(b, "  • %s: %s %s  |  %s\n", relayer.Name, runningIcon, runningLabel, walletStr)
	fmt.Fprintf(b, "    addr: %s  |  feed: %s\n",
		truncate(health.Address, 16, 6),
		truncate(health.PriceFeedID, 10, 5),
	)
}

// reporterBMEStatus is a local mirror of the BME API response fields we need.
type reporterBMEStatus struct {
	Status         string `json:"status"`
	CollateralRatio string `json:"collateral_ratio"`
	WarnThreshold  string `json:"warn_threshold"`
	HaltThreshold  string `json:"halt_threshold"`
	MintsAllowed   bool   `json:"mints_allowed"`
	RefundsAllowed bool   `json:"refunds_allowed"`
}

func (r *Reporter) appendBMEStatus(ctx context.Context, b *strings.Builder, akashAPI string) {
	url := akashAPI + "/akash/bme/v1/status"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(b, "BME Status: ❌ request error (%s)\n\n", err)
		return
	}

	resp, err := r.client.Do(req)
	if err != nil {
		fmt.Fprintf(b, "BME Status: ❌ unreachable (%s)\n\n", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(b, "BME Status: ❌ HTTP %d\n\n", resp.StatusCode)
		return
	}

	var s reporterBMEStatus
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		fmt.Fprintf(b, "BME Status: ❌ decode error (%s)\n\n", err)
		return
	}

	ratio, err := strconv.ParseFloat(s.CollateralRatio, 64)
	if err != nil {
		fmt.Fprintf(b, "BME Status: ❌ parse error (%s)\n\n", err)
		return
	}

	warnThreshold, _ := strconv.ParseFloat(s.WarnThreshold, 64)
	haltThreshold, _ := strconv.ParseFloat(s.HaltThreshold, 64)

	// Choose icon based on ratio vs thresholds and operational flags
	bmeIcon := "✅"
	switch {
	case !s.MintsAllowed || !s.RefundsAllowed || ratio < haltThreshold:
		bmeIcon = "🔴"
	case ratio < warnThreshold:
		bmeIcon = "⚠️"
	}

	mintsIcon := "✅"
	if !s.MintsAllowed {
		mintsIcon = "🔴"
	}
	refundsIcon := "✅"
	if !s.RefundsAllowed {
		refundsIcon = "🔴"
	}

	fmt.Fprintf(b, "BME: %s %s  |  collateral: %.0fx  |  mints: %s  refunds: %s  (warn: %.2f  halt: %.2f)\n\n",
		bmeIcon, formatBMEStatus(s.Status), ratio, mintsIcon, refundsIcon, warnThreshold, haltThreshold)
}

func (r *Reporter) appendGuardianStatus(
	ctx context.Context,
	b *strings.Builder,
	network config.NetworkConfig,
	globalIndex uint32,
	globalAddresses []string,
	globalErr error,
) {
	if globalErr != nil {
		fmt.Fprintf(b, "Guardian Set: ❌ unreachable (%s)\n\n", globalErr)
		return
	}

	// Fetch Akash on-chain guardian addresses for this network.
	akashClient := guardian.NewAkashOracleClient(network.AkashAPI, network.Name, network.WormholeContract)
	akashAddresses, err := akashClient.GetGuardianAddresses(ctx)
	if err != nil {
		fmt.Fprintf(b, "Guardian Set: global index %d (%d guardians)  |  Akash: ❌ params unreachable (%s)\n\n",
			globalIndex, len(globalAddresses), err)
		return
	}

	// Check sync: compare address counts and all addresses.
	inSync := len(globalAddresses) == len(akashAddresses)
	if inSync {
		for i, addr := range globalAddresses {
			if i >= len(akashAddresses) || addr != strings.ToLower(akashAddresses[i]) {
				inSync = false
				break
			}
		}
	}

	syncIcon := "✅"
	syncLabel := "in sync with Akash"
	if !inSync {
		syncIcon = "🔴"
		syncLabel = fmt.Sprintf("OUT OF SYNC — Akash has %d guardians, global has %d",
			len(akashAddresses), len(globalAddresses))
	}

	fmt.Fprintf(b, "Guardian Set: %s index %d  |  %d guardians  |  %s\n\n",
		syncIcon, globalIndex, len(globalAddresses), syncLabel)
}

func (r *Reporter) fetchOraclePrice(ctx context.Context, akashAPI string) (price float64, age time.Duration, err error) {
	url := fmt.Sprintf("%s/akash/oracle/v1/prices?pagination.limit=1", akashAPI)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, err
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("status %d", resp.StatusCode)
	}

	var result types.OraclePriceResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, 0, err
	}
	if len(result.Prices) == 0 {
		return 0, 0, fmt.Errorf("no prices returned")
	}

	p := result.Prices[0]
	ts, err := time.Parse(time.RFC3339, p.State.Timestamp)
	if err != nil {
		return 0, 0, fmt.Errorf("parse timestamp: %w", err)
	}

	priceF, err := strconv.ParseFloat(p.State.Price, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse price: %w", err)
	}

	return priceF, time.Since(ts.UTC()), nil
}

func (r *Reporter) fetchHealth(ctx context.Context, endpoint string) (*types.HermesHealthResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var h types.HermesHealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return nil, err
	}
	return &h, nil
}

func (r *Reporter) fetchWalletBalance(ctx context.Context, akashAPI, address string) (int64, error) {
	url := fmt.Sprintf("%s/cosmos/bank/v1beta1/balances/%s", akashAPI, address)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("status %d", resp.StatusCode)
	}

	var result types.WalletBalanceResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}

	for _, b := range result.Balances {
		if b.Denom == "uakt" {
			return strconv.ParseInt(b.Amount, 10, 64)
		}
	}
	return 0, nil
}

// durationUntilNext returns the duration until the next occurrence of hour:minute
// in the given location.
func durationUntilNext(hour, minute int, loc *time.Location) time.Duration {
	now := time.Now().In(loc)
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, loc)
	if !now.Before(next) {
		next = next.Add(24 * time.Hour)
	}
	return time.Until(next)
}

// parseHHMM parses a "HH:MM" string into hour and minute integers.
func parseHHMM(s string) (hour, minute int, err error) {
	var h, m int
	if _, err = fmt.Sscanf(s, "%d:%d", &h, &m); err != nil {
		return 0, 0, fmt.Errorf("expected HH:MM, got %q", s)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("time %q out of range", s)
	}
	return h, m, nil
}

// truncate shortens a string to prefix+...+suffix chars for display.
func truncate(s string, prefix, suffix int) string {
	if len(s) <= prefix+suffix+3 {
		return s
	}
	return s[:prefix] + "..." + s[len(s)-suffix:]
}

// formatBMEStatus converts internal status strings like "mint_status_healthy"
// to human-readable labels like "Healthy".
func formatBMEStatus(s string) string {
	s = strings.TrimPrefix(s, "mint_status_")
	if len(s) == 0 {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func formatAge(d time.Duration) string {
	d = d.Round(time.Second)
	minutes := int(d.Minutes())
	seconds := int(d.Seconds()) % 60
	if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}
