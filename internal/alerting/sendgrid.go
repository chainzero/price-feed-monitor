package alerting

import (
	"log/slog"

	"github.com/akash-network/price-feed-monitor/internal/types"
	sendgridgo "github.com/sendgrid/sendgrid-go"
	"github.com/sendgrid/sendgrid-go/helpers/mail"
)

// SendGrid sends alert emails via the SendGrid API.
//
// Only alerts at or above MinSeverity are emailed — lower-severity events are
// suppressed to avoid inbox noise. Resolve and Post are always sent (they carry
// important status information regardless of severity).
//
// Unlike the Slack alerter, SendGrid does not apply cooldown logic — the Slack
// alerter's cooldown is sufficient to prevent duplicate emails since Multi fans
// out to both backends simultaneously.
type SendGrid struct {
	apiKey      string
	from        string
	to          []string
	minSeverity types.Severity
}

// NewSendGrid returns a SendGrid alerter. apiKey is the SendGrid API key,
// from is the sender address, to is the list of recipient addresses, and
// minSeverity is the minimum severity level that triggers an email.
func NewSendGrid(apiKey, from string, to []string, minSeverity types.Severity) *SendGrid {
	return &SendGrid{
		apiKey:      apiKey,
		from:        from,
		to:          to,
		minSeverity: minSeverity,
	}
}

// Send emails the alert if its severity meets the minimum threshold.
func (s *SendGrid) Send(alert types.Alert) {
	if alert.Severity < s.minSeverity {
		return
	}
	subject := "[" + alert.Severity.String() + "] " + alert.Title
	s.send(subject, alert.Body)
}

// Resolve always sends a resolution email.
func (s *SendGrid) Resolve(key, title, body string) {
	s.send("[RESOLVED] "+title, body)
}

// Post always sends an informational email (startup/daily health reports).
func (s *SendGrid) Post(title, body string) {
	s.send("[INFO] "+title, body)
}

func (s *SendGrid) send(subject, body string) {
	from := mail.NewEmail("Akash BME Monitor", s.from)

	message := mail.NewV3Mail()
	message.SetFrom(from)
	message.Subject = subject

	p := mail.NewPersonalization()
	for _, addr := range s.to {
		p.AddTos(mail.NewEmail("", addr))
	}
	message.AddPersonalizations(p)
	message.AddContent(mail.NewContent("text/plain", body))

	client := sendgridgo.NewSendClient(s.apiKey)
	resp, err := client.Send(message)
	if err != nil {
		slog.Error("failed to send email via SendGrid", "subject", subject, "error", err)
		return
	}
	if resp.StatusCode >= 300 {
		slog.Error("SendGrid rejected email",
			"subject", subject,
			"status_code", resp.StatusCode,
			"response_body", resp.Body,
		)
		return
	}

	slog.Info("email sent via SendGrid", "subject", subject, "recipients", len(s.to))
}
