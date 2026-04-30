package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/akash-network/price-feed-monitor/internal/alerting"
	"github.com/akash-network/price-feed-monitor/internal/announcements"
	"github.com/akash-network/price-feed-monitor/internal/bme"
	"github.com/akash-network/price-feed-monitor/internal/config"
	"github.com/akash-network/price-feed-monitor/internal/guardian"
	"github.com/akash-network/price-feed-monitor/internal/hermes"
	"github.com/akash-network/price-feed-monitor/internal/oracle"
	"github.com/akash-network/price-feed-monitor/internal/report"
	"github.com/akash-network/price-feed-monitor/internal/types"
)

func main() {
	defaultConfig := "config.yaml"
	if v := os.Getenv(config.EnvConfigPath); v != "" {
		defaultConfig = v
	}
	configPath := flag.String("config", defaultConfig, "path to config file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Build alerter — Slack always active; SendGrid added when email is enabled and API key present.
	slackAlerter := alerting.NewSlack(cfg.Slack.WebhookURL)
	var alerter alerting.Alerter = slackAlerter
	if cfg.Email.Enabled {
		if cfg.Email.APIKey == "" {
			slog.Warn("email alerting enabled but SENDGRID_API_KEY is not set — email alerts disabled")
		} else {
			minSev := parseMinSeverity(cfg.Email.MinSeverity)
			emailAlerter := alerting.NewSendGrid(cfg.Email.APIKey, cfg.Email.From, cfg.Email.To, minSev)
			alerter = alerting.NewMulti(slackAlerter, emailAlerter)
			slog.Info("email alerting enabled via SendGrid",
				"from", cfg.Email.From,
				"recipients", len(cfg.Email.To),
				"min_severity", minSev.String(),
			)
		}
	}

	// Component 1: Oracle Price Health
	if cfg.OraclePriceMonitor.Enabled {
		for _, network := range cfg.Networks {
			pm := oracle.NewPriceMonitor(network, cfg.OraclePriceMonitor, alerter, logger)
			go pm.Run(ctx)
		}
		slog.Info("oracle price monitor enabled", "networks", len(cfg.Networks))
	}

	// Component 2: Hermes Relayer Health
	if cfg.HermesHealthMonitor.Enabled {
		for _, network := range cfg.Networks {
			hm := hermes.NewHealthMonitor(network, cfg.HermesHealthMonitor, alerter, logger)
			go hm.Run(ctx)
		}
		slog.Info("hermes health monitor enabled", "networks", len(cfg.Networks))
	}

	// Component 3: Guardian Set Currency
	if cfg.GuardianSetMonitor.Enabled {
		gm := guardian.NewSyncMonitor(cfg.GuardianSetMonitor, cfg.Networks, alerter, logger)
		go gm.Run(ctx)
		slog.Info("guardian set monitor enabled")
	}

	// Component 6: BME Status Monitor
	if cfg.BMEMonitor.Enabled {
		for _, network := range cfg.Networks {
			bm := bme.NewStatusMonitor(network, cfg.BMEMonitor, alerter, logger)
			go bm.Run(ctx)
		}
		slog.Info("BME status monitor enabled", "networks", len(cfg.Networks))
	}

	// Component 5: Wormholescan Guardian Set Monitor
	if cfg.WormholescanMonitor.Enabled {
		wm := guardian.NewWormholescanMonitor(cfg.WormholescanMonitor, cfg.Networks, cfg.GuardianSetMonitor.EtherscanAPIKey, alerter, logger)
		go wm.Run(ctx)
		slog.Info("wormholescan guardian set monitor enabled")
	}

	// Component 4: Guardian Update Announcements
	if cfg.AnnouncementMonitor.Enabled {
		if cfg.AnnouncementMonitor.PythForum.Enabled {
			fm := announcements.NewPythForumMonitor(cfg.AnnouncementMonitor.PythForum, alerter, logger)
			go fm.Run(ctx)
			slog.Info("pyth forum monitor enabled")
		}
		if cfg.AnnouncementMonitor.GitHub.Enabled {
			gm := announcements.NewGitHubGuardianMonitor(cfg.AnnouncementMonitor.GitHub, alerter, logger)
			go gm.Run(ctx)
			slog.Info("github guardian monitor enabled", "repo", cfg.AnnouncementMonitor.GitHub.Repo)
		}
	}

	// Startup summary + daily health check
	reporter := report.New(cfg, alerter, logger)
	reporter.PostStartup(ctx)
	go reporter.RunDailySchedule(ctx)

	slog.Info("price-feed-monitor started")
	<-ctx.Done()
	slog.Info("shutting down")
}

// parseMinSeverity converts a config string to a Severity level.
// Defaults to Warning if unrecognised.
func parseMinSeverity(s string) types.Severity {
	switch strings.ToLower(s) {
	case "info":
		return types.SeverityInfo
	case "warning":
		return types.SeverityWarning
	case "critical":
		return types.SeverityCritical
	case "emergency":
		return types.SeverityEmergency
	default:
		return types.SeverityWarning
	}
}
