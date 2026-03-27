package types

import "testing"

func TestSeverityString(t *testing.T) {
	cases := []struct {
		sev  Severity
		want string
	}{
		{SeverityNone, "NONE"},
		{SeverityInfo, "INFO"},
		{SeverityWarning, "WARNING"},
		{SeverityCritical, "CRITICAL"},
		{SeverityEmergency, "EMERGENCY"},
		{SeverityResolved, "RESOLVED"},
		{Severity(99), "NONE"}, // unknown value falls through to default
	}

	for _, tc := range cases {
		if got := tc.sev.String(); got != tc.want {
			t.Errorf("Severity(%d).String() = %q, want %q", tc.sev, got, tc.want)
		}
	}
}

func TestSeverityEmoji(t *testing.T) {
	cases := []struct {
		sev  Severity
		want string
	}{
		{SeverityNone, ""},
		{SeverityInfo, "ℹ️"},
		{SeverityWarning, "⚠️"},
		{SeverityCritical, "🔴"},
		{SeverityEmergency, "🚨"},
		{SeverityResolved, "✅"},
		{Severity(99), ""}, // unknown value
	}

	for _, tc := range cases {
		if got := tc.sev.Emoji(); got != tc.want {
			t.Errorf("Severity(%d).Emoji() = %q, want %q", tc.sev, got, tc.want)
		}
	}
}

func TestSeverityOrdering(t *testing.T) {
	// Severity levels must be strictly increasing so escalation logic works.
	levels := []Severity{
		SeverityNone,
		SeverityInfo,
		SeverityWarning,
		SeverityCritical,
		SeverityEmergency,
		SeverityResolved,
	}

	for i := 1; i < len(levels); i++ {
		if levels[i] <= levels[i-1] {
			t.Errorf("severity ordering broken: %s (%d) is not greater than %s (%d)",
				levels[i].String(), levels[i],
				levels[i-1].String(), levels[i-1],
			)
		}
	}
}
