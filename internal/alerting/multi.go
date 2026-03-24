package alerting

import "github.com/akash-network/price-feed-monitor/internal/types"

// Multi fans out Send, Resolve, and Post calls to multiple Alerter backends.
// All backends receive every call; a failure in one does not prevent others from running.
type Multi struct {
	backends []Alerter
}

// NewMulti returns a Multi alerter that delegates to all provided backends.
func NewMulti(backends ...Alerter) *Multi {
	return &Multi{backends: backends}
}

func (m *Multi) Send(alert types.Alert) {
	for _, b := range m.backends {
		b.Send(alert)
	}
}

func (m *Multi) Resolve(key, title, body string) {
	for _, b := range m.backends {
		b.Resolve(key, title, body)
	}
}

func (m *Multi) Post(title, body string) {
	for _, b := range m.backends {
		b.Post(title, body)
	}
}
