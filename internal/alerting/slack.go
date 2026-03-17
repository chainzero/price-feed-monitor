package alerting

import (
	"log/slog"
	"sync"
	"time"

	"github.com/akash-network/price-feed-monitor/internal/types"
	"github.com/slack-go/slack"
)

const defaultCooldown = 10 * time.Minute

type cooldownEntry struct {
	severity types.Severity
	sentAt   time.Time
}

// Slack posts alerts to a Slack Incoming Webhook with per-key rate limiting.
// Severity escalations always bypass the cooldown; same-or-lower severity
// alerts are suppressed until the cooldown expires.
type Slack struct {
	webhookURL string
	mu         sync.Mutex
	cooldowns  map[string]cooldownEntry
}

func NewSlack(webhookURL string) *Slack {
	return &Slack{
		webhookURL: webhookURL,
		cooldowns:  make(map[string]cooldownEntry),
	}
}

// Send posts an alert unless suppressed by the cooldown policy.
func (s *Slack) Send(alert types.Alert) {
	if alert.Time.IsZero() {
		alert.Time = time.Now()
	}

	if !s.shouldSend(alert) {
		slog.Debug("alert suppressed by cooldown", "key", alert.Key, "severity", alert.Severity)
		return
	}

	text := alert.Severity.Emoji() + " *" + alert.Title + "*\n\n" + alert.Body
	msg := &slack.WebhookMessage{Text: text}

	if err := slack.PostWebhook(s.webhookURL, msg); err != nil {
		slog.Error("failed to post Slack alert", "error", err, "key", alert.Key)
		return
	}

	s.mu.Lock()
	s.cooldowns[alert.Key] = cooldownEntry{severity: alert.Severity, sentAt: alert.Time}
	s.mu.Unlock()

	slog.Info("slack alert sent", "key", alert.Key, "severity", alert.Severity.String())
}

// Resolve sends a ✅ resolved message and clears the cooldown entry for key.
// If no alert was previously sent for key, this is a no-op.
func (s *Slack) Resolve(key, title, body string) {
	s.mu.Lock()
	_, hadAlert := s.cooldowns[key]
	delete(s.cooldowns, key)
	s.mu.Unlock()

	if !hadAlert {
		return
	}

	text := types.SeverityResolved.Emoji() + " *" + title + "*\n\n" + body
	msg := &slack.WebhookMessage{Text: text}

	if err := slack.PostWebhook(s.webhookURL, msg); err != nil {
		slog.Error("failed to post Slack resolved message", "error", err, "key", key)
		return
	}

	slog.Info("slack resolved message sent", "key", key)
}

// Post sends a message directly without cooldown tracking. Use for informational
// messages (startup summaries, daily health checks) that should always be delivered.
func (s *Slack) Post(title, body string) {
	text := "ℹ️ *" + title + "*\n\n" + body
	msg := &slack.WebhookMessage{Text: text}
	if err := slack.PostWebhook(s.webhookURL, msg); err != nil {
		slog.Error("failed to post Slack message", "error", err, "title", title)
		return
	}
	slog.Info("slack message sent", "title", title)
}

func (s *Slack) shouldSend(alert types.Alert) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry, exists := s.cooldowns[alert.Key]
	if !exists {
		return true
	}
	// Always send if severity has escalated.
	if alert.Severity > entry.severity {
		return true
	}
	// Resend if cooldown has expired.
	return time.Since(entry.sentAt) >= defaultCooldown
}
