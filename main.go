package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/akash-network/price-feed-monitor/internal/alerting"
	"github.com/akash-network/price-feed-monitor/internal/announcements"
	"github.com/akash-network/price-feed-monitor/internal/bme"
	"github.com/akash-network/price-feed-monitor/internal/config"
	"github.com/akash-network/price-feed-monitor/internal/guardian"
	"github.com/akash-network/price-feed-monitor/internal/hermes"
	"github.com/akash-network/price-feed-monitor/internal/oracle"
	"github.com/akash-network/price-feed-monitor/internal/report"
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

	alerter := alerting.NewSlack(cfg.Slack.WebhookURL)

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
		wm := guardian.NewWormholescanMonitor(cfg.WormholescanMonitor, cfg.Networks, alerter, logger)
		go wm.Run(ctx)
		slog.Info("wormholescan guardian set monitor enabled")
	}

	// Component 4: Guardian Update Announcements
	if cfg.AnnouncementMonitor.Enabled && cfg.AnnouncementMonitor.PythForum.Enabled {
		fm := announcements.NewPythForumMonitor(cfg.AnnouncementMonitor.PythForum, alerter, logger)
		go fm.Run(ctx)
		slog.Info("pyth forum monitor enabled")
	}

	// Startup summary + daily 8 AM CT health check
	reporter := report.New(cfg, alerter, logger)
	reporter.PostStartup(ctx)
	go reporter.RunDailySchedule(ctx)

	slog.Info("price-feed-monitor started")
	<-ctx.Done()
	slog.Info("shutting down")
}
