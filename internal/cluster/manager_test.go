package cluster

import (
    "sync/atomic"
    "testing"

    "github.com/mayurvarma14/go-proxy/internal/config"
)

func TestPick_RoundRobin(t *testing.T) {
    m, err := NewManager([]config.ClusterSpec{{
        Name: "c1", Endpoints: []string{"127.0.0.1:1", "127.0.0.1:2"},
    }})
    if err != nil { t.Fatal(err) }
    a, _ := m.Pick("c1")
    b, _ := m.Pick("c1")
    if a.Addr == b.Addr {
        t.Fatalf("expected different endpoints in round-robin, got %q == %q", a.Addr, b.Addr)
    }
}

func TestPick_PerEndpointCap_AllCapped(t *testing.T) {
    m, err := NewManager([]config.ClusterSpec{{
        Name: "c1", Endpoints: []string{"127.0.0.1:1", "127.0.0.1:2"},
        CircuitBreaker: &config.CircuitBreakerSpec{PerEndpointMaxRequests: 1},
    }})
    if err != nil { t.Fatal(err) }
    c := m.byName["c1"]
    // Set both endpoints to in-flight == cap
    for _, ep := range c.Endpoints {
        c.mu.Lock()
        st := c.state[ep.Addr]
        c.mu.Unlock()
        atomic.StoreInt64(&st.inFlight, 1)
    }
    if _, err := m.Pick("c1"); err == nil {
        t.Fatalf("expected ErrNoEndpointCapacity when all at cap")
    }
}

func TestPick_LeastRequests_ChoosesLowerInflight(t *testing.T) {
    m, err := NewManager([]config.ClusterSpec{{
        Name: "c1", Endpoints: []string{"127.0.0.1:1", "127.0.0.1:2"}, LBPolicy: "least_requests",
    }})
    if err != nil { t.Fatal(err) }
    c := m.byName["c1"]
    // Set inflight: ep[0]=5, ep[1]=1
    c.mu.Lock()
    st0 := c.state[c.Endpoints[0].Addr]
    st1 := c.state[c.Endpoints[1].Addr]
    c.mu.Unlock()
    atomic.StoreInt64(&st0.inFlight, 5)
    atomic.StoreInt64(&st1.inFlight, 1)
    ep, err := m.Pick("c1")
    if err != nil { t.Fatal(err) }
    if ep.Addr != c.Endpoints[1].Addr {
        t.Fatalf("expected ep %q, got %q", c.Endpoints[1].Addr, ep.Addr)
    }
}

