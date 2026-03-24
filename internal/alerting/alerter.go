package alerting

import "github.com/akash-network/price-feed-monitor/internal/types"

// Alerter is the interface implemented by all alert backends (Slack, SendGrid, etc.).
// Monitors accept an Alerter so additional backends can be added without modifying them.
type Alerter interface {
	// Send posts an alert. Implementations may apply cooldown/dedup logic.
	Send(alert types.Alert)

	// Resolve sends a resolution message and clears any outstanding alert state for key.
	// No-ops if no alert was previously sent for key.
	Resolve(key, title, body string)

	// Post sends an informational message without cooldown tracking.
	// Used for startup summaries and scheduled health reports.
	Post(title, body string)
}
