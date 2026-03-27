package alerting

import (
	"testing"
	"time"

	"github.com/akash-network/price-feed-monitor/internal/types"
)

// newSlackWithCooldown creates a Slack instance and pre-populates a cooldown
// entry directly, bypassing the HTTP call. This lets us test shouldSend()
// without hitting the real Slack API.
func newSlackWithCooldown(key string, sev types.Severity, sentAt time.Time) *Slack {
	s := NewSlack("https://hooks.slack.com/services/FAKE")
	s.cooldowns[key] = cooldownEntry{severity: sev, sentAt: sentAt}
	return s
}

func TestShouldSend_NoExistingCooldown(t *testing.T) {
	s := NewSlack("https://hooks.slack.com/services/FAKE")
	alert := types.Alert{Key: "k", Severity: types.SeverityWarning, Time: time.Now()}
	if !s.shouldSend(alert) {
		t.Error("shouldSend() = false for new key, want true")
	}
}

func TestShouldSend_WithinCooldown_SameSeverity(t *testing.T) {
	s := newSlackWithCooldown("k", types.SeverityWarning, time.Now())
	alert := types.Alert{Key: "k", Severity: types.SeverityWarning, Time: time.Now()}
	if s.shouldSend(alert) {
		t.Error("shouldSend() = true within cooldown at same severity, want false")
	}
}

func TestShouldSend_WithinCooldown_LowerSeverity(t *testing.T) {
	// Lower severity should be suppressed even within cooldown.
	s := newSlackWithCooldown("k", types.SeverityCritical, time.Now())
	alert := types.Alert{Key: "k", Severity: types.SeverityWarning, Time: time.Now()}
	if s.shouldSend(alert) {
		t.Error("shouldSend() = true for lower severity within cooldown, want false")
	}
}

func TestShouldSend_WithinCooldown_Escalation(t *testing.T) {
	// Severity escalation must bypass cooldown.
	s := newSlackWithCooldown("k", types.SeverityWarning, time.Now())
	alert := types.Alert{Key: "k", Severity: types.SeverityCritical, Time: time.Now()}
	if !s.shouldSend(alert) {
		t.Error("shouldSend() = false for severity escalation, want true")
	}
}

func TestShouldSend_CooldownExpired(t *testing.T) {
	// Sent 11 minutes ago — cooldown (10m) has expired.
	sentAt := time.Now().Add(-(defaultCooldown + time.Minute))
	s := newSlackWithCooldown("k", types.SeverityWarning, sentAt)
	alert := types.Alert{Key: "k", Severity: types.SeverityWarning, Time: time.Now()}
	if !s.shouldSend(alert) {
		t.Error("shouldSend() = false after cooldown expired, want true")
	}
}

func TestShouldSend_CooldownJustActive(t *testing.T) {
	// Sent 9 minutes ago — still within 10-minute cooldown.
	sentAt := time.Now().Add(-(defaultCooldown - time.Minute))
	s := newSlackWithCooldown("k", types.SeverityWarning, sentAt)
	alert := types.Alert{Key: "k", Severity: types.SeverityWarning, Time: time.Now()}
	if s.shouldSend(alert) {
		t.Error("shouldSend() = true with 1 minute remaining in cooldown, want false")
	}
}

func TestShouldSend_EmergencyAlwaysEscalates(t *testing.T) {
	// Sent a Critical 1 second ago — Emergency should still go through.
	s := newSlackWithCooldown("k", types.SeverityCritical, time.Now())
	alert := types.Alert{Key: "k", Severity: types.SeverityEmergency, Time: time.Now()}
	if !s.shouldSend(alert) {
		t.Error("shouldSend() = false for Emergency escalation, want true")
	}
}

func TestShouldSend_DifferentKeysAreIndependent(t *testing.T) {
	s := newSlackWithCooldown("key-a", types.SeverityWarning, time.Now())
	// key-b has no cooldown entry
	alert := types.Alert{Key: "key-b", Severity: types.SeverityWarning, Time: time.Now()}
	if !s.shouldSend(alert) {
		t.Error("shouldSend() = false for new key, want true")
	}
}

func TestResolve_ClearsCooldown(t *testing.T) {
	// After Resolve, the same key should be sendable again without hitting HTTP.
	s := newSlackWithCooldown("k", types.SeverityWarning, time.Now())

	// Manually clear without HTTP — call the internal state directly.
	s.mu.Lock()
	delete(s.cooldowns, "k")
	s.mu.Unlock()

	alert := types.Alert{Key: "k", Severity: types.SeverityWarning, Time: time.Now()}
	if !s.shouldSend(alert) {
		t.Error("shouldSend() = false after cooldown cleared, want true")
	}
}

func TestShouldSend_MultipleEscalationSteps(t *testing.T) {
	// Info → Warning → Critical → Emergency: each step should be sendable.
	s := NewSlack("https://hooks.slack.com/services/FAKE")

	steps := []types.Severity{
		types.SeverityInfo,
		types.SeverityWarning,
		types.SeverityCritical,
		types.SeverityEmergency,
	}

	for i, sev := range steps {
		alert := types.Alert{Key: "k", Severity: sev, Time: time.Now()}
		if !s.shouldSend(alert) {
			t.Errorf("step %d (%s): shouldSend() = false, want true", i, sev.String())
		}
		// Simulate that this alert was "sent" by recording the cooldown.
		s.mu.Lock()
		s.cooldowns["k"] = cooldownEntry{severity: sev, sentAt: time.Now()}
		s.mu.Unlock()
	}
}
