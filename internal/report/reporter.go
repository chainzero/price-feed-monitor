package report

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

// dailyCheckHour is the hour (24h, America/Chicago) at which the daily
// health check is posted to Slack.
const dailyCheckHour = 8

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

// RunDailySchedule blocks until ctx is cancelled, posting a health summary
// at 08:00 America/Chicago each day.
func (r *Reporter) RunDailySchedule(ctx context.Context) {
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		r.logger.Error("failed to load America/Chicago timezone", "error", err)
		return
	}

	for {
		delay := durationUntilNext(dailyCheckHour, loc)
		r.logger.Info("daily health check scheduled", "in", delay.Round(time.Minute))

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
			r.post(ctx, "📊 BME Price Feed Monitor — Daily Health Check")
		}
	}
}

// post fetches live status and sends the formatted summary to Slack.
func (r *Reporter) post(ctx context.Context, header string) {
	var b strings.Builder
	fmt.Fprintf(&b, "Time: %s UTC\n\n", time.Now().UTC().Format("2006-01-02 15:04:05"))

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
			fmt.Fprintf(&b, "Oracle Price: %s $%s AKT/USD  (age: %s)\n\n",
				priceStatus, price, formatAge(priceAge))
		}

		// Hermes relayers
		fmt.Fprintf(&b, "Hermes Relayers:\n")
		for _, relayer := range network.HermesRelayers {
			r.appendRelayerStatus(ctx, &b, network, relayer)
		}
		fmt.Fprintln(&b)
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
	if !health.IsRunning {
		runningIcon = "🔴"
	}

	fmt.Fprintf(b, "  • %s: %s isRunning=%v\n", relayer.Name, runningIcon, health.IsRunning)
	fmt.Fprintf(b, "    Address:  %s\n", health.Address)
	fmt.Fprintf(b, "    PriceFeed: %s\n", health.PriceFeedID)
	fmt.Fprintf(b, "    Contract:  %s\n", health.ContractAddress)

	// Wallet balance
	if relayer.Wallet != "" {
		balanceUAKT, err := r.fetchWalletBalance(ctx, network.AkashAPI, relayer.Wallet)
		if err != nil {
			fmt.Fprintf(b, "    Wallet:    ❌ balance unavailable (%s)\n", err)
		} else {
			balanceAKT := float64(balanceUAKT) / 1_000_000
			minAKT := float64(relayer.MinWalletBalance) / 1_000_000
			balanceIcon := "✅"
			if relayer.MinWalletBalance > 0 && balanceUAKT < relayer.MinWalletBalance {
				balanceIcon = "⚠️"
			}
			fmt.Fprintf(b, "    Wallet:    %s %.2f AKT (min: %.2f AKT)\n",
				balanceIcon, balanceAKT, minAKT)
		}
	}
}

func (r *Reporter) fetchOraclePrice(ctx context.Context, akashAPI string) (price string, age time.Duration, err error) {
	url := fmt.Sprintf("%s/akash/oracle/v1/prices?pagination.limit=1", akashAPI)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", 0, err
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("status %d", resp.StatusCode)
	}

	var result types.OraclePriceResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", 0, err
	}
	if len(result.Prices) == 0 {
		return "", 0, fmt.Errorf("no prices returned")
	}

	p := result.Prices[0]
	ts, err := time.Parse(time.RFC3339, p.State.Timestamp)
	if err != nil {
		return "", 0, fmt.Errorf("parse timestamp: %w", err)
	}

	return p.State.Price, time.Since(ts.UTC()), nil
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

// durationUntilNext returns the duration until the next occurrence of hour:00
// in the given location.
func durationUntilNext(hour int, loc *time.Location) time.Duration {
	now := time.Now().In(loc)
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, loc)
	if !now.Before(next) {
		next = next.Add(24 * time.Hour)
	}
	return time.Until(next)
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
