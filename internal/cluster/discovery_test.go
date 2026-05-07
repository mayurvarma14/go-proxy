package cluster

import (
	"github.com/mayurvarma14/go-proxy/internal/config"
	"testing"
)

func epSet(eps []Endpoint) map[string]struct{} {
	m := make(map[string]struct{}, len(eps))
	for _, e := range eps {
		m[e.Addr] = struct{}{}
	}
	return m
}

func TestApplyEndpointSet_AddAndRemove(t *testing.T) {
	m, err := NewManager([]config.ClusterSpec{{Name: "c1", Endpoints: []string{"127.0.0.1:1"}}})
	if err != nil {
		t.Fatal(err)
	}
	c := m.byName["c1"]

	// Add a new endpoint (simulate DNS adding 127.0.0.1:2)
	newSet := map[string]struct{}{"127.0.0.1:1": {}, "127.0.0.1:2": {}}
	c.applyEndpointSet(newSet)
	got := epSet(c.snapshotEndpoints())
	if _, ok := got["127.0.0.1:1"]; !ok {
		t.Fatalf("missing endpoint 127.0.0.1:1 after add")
	}
	if _, ok := got["127.0.0.1:2"]; !ok {
		t.Fatalf("missing endpoint 127.0.0.1:2 after add")
	}
	// State entries must exist for new endpoints
	c.mu.RLock()
	if _, ok := c.state["127.0.0.1:2"]; !ok {
		c.mu.RUnlock()
		t.Fatalf("state not created for new endpoint")
	}
	c.mu.RUnlock()

	// Remove the first endpoint
	newSet = map[string]struct{}{"127.0.0.1:2": {}}
	c.applyEndpointSet(newSet)
	got = epSet(c.snapshotEndpoints())
	if _, ok := got["127.0.0.1:2"]; !ok || len(got) != 1 {
		t.Fatalf("expected only 127.0.0.1:2 after remove, got %#v", got)
	}
}

func TestDebugSnapshot_Basic(t *testing.T) {
	m, err := NewManager([]config.ClusterSpec{{Name: "c1", Endpoints: []string{"127.0.0.1:1"}, LBPolicy: "round_robin"}})
	if err != nil {
		t.Fatal(err)
	}
	snap := m.DebugSnapshot()
	if len(snap) != 1 {
		t.Fatalf("want 1 cluster in snapshot, got %d", len(snap))
	}
	cd := snap[0]
	if cd.Name != "c1" {
		t.Fatalf("want name c1, got %q", cd.Name)
	}
	if cd.LBPolicy == "" {
		t.Fatalf("expected lb policy set")
	}
	if len(cd.Endpoints) != 1 || cd.Endpoints[0].Address != "127.0.0.1:1" {
		t.Fatalf("unexpected endpoints snapshot: %#v", cd.Endpoints)
	}
}
