package alerting

import (
	"testing"

	"github.com/akash-network/price-feed-monitor/internal/types"
)

// mockAlerter records every call made to it.
type mockAlerter struct {
	sends    []types.Alert
	resolves []resolveCall
	posts    []postCall
}

type resolveCall struct{ key, title, body string }
type postCall struct{ title, body string }

func (m *mockAlerter) Send(alert types.Alert)              { m.sends = append(m.sends, alert) }
func (m *mockAlerter) Resolve(key, title, body string)    { m.resolves = append(m.resolves, resolveCall{key, title, body}) }
func (m *mockAlerter) Post(title, body string)             { m.posts = append(m.posts, postCall{title, body}) }

func TestMulti_SendReachesAllBackends(t *testing.T) {
	a, b := &mockAlerter{}, &mockAlerter{}
	multi := NewMulti(a, b)

	alert := types.Alert{Key: "test", Severity: types.SeverityWarning, Title: "T", Body: "B"}
	multi.Send(alert)

	if len(a.sends) != 1 {
		t.Errorf("backend a: expected 1 Send call, got %d", len(a.sends))
	}
	if len(b.sends) != 1 {
		t.Errorf("backend b: expected 1 Send call, got %d", len(b.sends))
	}
	if a.sends[0].Key != "test" {
		t.Errorf("backend a received wrong alert key: %q", a.sends[0].Key)
	}
}

func TestMulti_ResolveReachesAllBackends(t *testing.T) {
	a, b := &mockAlerter{}, &mockAlerter{}
	multi := NewMulti(a, b)

	multi.Resolve("key1", "Title", "Body")

	if len(a.resolves) != 1 {
		t.Errorf("backend a: expected 1 Resolve call, got %d", len(a.resolves))
	}
	if len(b.resolves) != 1 {
		t.Errorf("backend b: expected 1 Resolve call, got %d", len(b.resolves))
	}
	if a.resolves[0].key != "key1" {
		t.Errorf("backend a received wrong resolve key: %q", a.resolves[0].key)
	}
}

func TestMulti_PostReachesAllBackends(t *testing.T) {
	a, b := &mockAlerter{}, &mockAlerter{}
	multi := NewMulti(a, b)

	multi.Post("Daily Report", "All systems go")

	if len(a.posts) != 1 {
		t.Errorf("backend a: expected 1 Post call, got %d", len(a.posts))
	}
	if len(b.posts) != 1 {
		t.Errorf("backend b: expected 1 Post call, got %d", len(b.posts))
	}
	if b.posts[0].title != "Daily Report" {
		t.Errorf("backend b received wrong post title: %q", b.posts[0].title)
	}
}

func TestMulti_SingleBackend(t *testing.T) {
	a := &mockAlerter{}
	multi := NewMulti(a)

	multi.Send(types.Alert{Key: "k"})
	multi.Resolve("k", "t", "b")
	multi.Post("t", "b")

	if len(a.sends) != 1 || len(a.resolves) != 1 || len(a.posts) != 1 {
		t.Errorf("single backend: sends=%d resolves=%d posts=%d, want 1 each",
			len(a.sends), len(a.resolves), len(a.posts))
	}
}

func TestMulti_NoBackends(t *testing.T) {
	// Should not panic with zero backends.
	multi := NewMulti()
	multi.Send(types.Alert{Key: "k"})
	multi.Resolve("k", "t", "b")
	multi.Post("t", "b")
}

func TestMulti_AlertFieldsPreserved(t *testing.T) {
	a := &mockAlerter{}
	multi := NewMulti(a)

	alert := types.Alert{
		Key:      "oracle_stale",
		Severity: types.SeverityCritical,
		Title:    "Price stale",
		Body:     "Last update 20 minutes ago",
	}
	multi.Send(alert)

	got := a.sends[0]
	if got.Key != alert.Key || got.Severity != alert.Severity ||
		got.Title != alert.Title || got.Body != alert.Body {
		t.Errorf("alert not preserved: got %+v, want %+v", got, alert)
	}
}

func TestMulti_ThreeBackends(t *testing.T) {
	a, b, c := &mockAlerter{}, &mockAlerter{}, &mockAlerter{}
	multi := NewMulti(a, b, c)

	multi.Send(types.Alert{Key: "k"})

	for i, m := range []*mockAlerter{a, b, c} {
		if len(m.sends) != 1 {
			t.Errorf("backend %d: expected 1 Send, got %d", i, len(m.sends))
		}
	}
}
