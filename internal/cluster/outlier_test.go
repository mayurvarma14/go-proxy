package cluster

import (
    "testing"
    "time"
    "github.com/mayurvarma14/go-proxy/internal/config"
)

func TestOutlier_EjectAfterConsecutiveFailures(t *testing.T) {
    m, err := NewManager([]config.ClusterSpec{{
        Name: "c1", Endpoints: []string{"127.0.0.1:1"},
        Outlier: &config.OutlierSpec{ConsecutiveFailures: 2, EjectionSeconds: 1},
    }})
    if err != nil { t.Fatal(err) }
    // Two failures should eject
    m.ReportFailure("c1", "127.0.0.1:1")
    m.ReportFailure("c1", "127.0.0.1:1")
    c := m.byName["c1"]
    if !c.isEjected("127.0.0.1:1", time.Now()) {
        t.Fatalf("expected endpoint ejected after consecutive failures")
    }
}

func TestActiveHealth_updateActiveHealth(t *testing.T) {
    // Verify threshold logic toggles activeUnhealthy
    m, err := NewManager([]config.ClusterSpec{{
        Name: "c1", Endpoints: []string{"127.0.0.1:1"},
        HealthCheck: &config.HealthCheckSpec{Type: "tcp", IntervalMillis: 100, TimeoutMillis: 50, HealthyThreshold: 1, UnhealthyThreshold: 2},
    }})
    if err != nil { t.Fatal(err) }
    c := m.byName["c1"]
    addr := c.Endpoints[0].Addr
    // Two consecutive unhealthy flips activeUnhealthy
    c.updateActiveHealth(addr, false)
    c.updateActiveHealth(addr, false)
    c.mu.RLock(); st := c.state[addr]; c.mu.RUnlock()
    if st == nil || !st.activeUnhealthy { t.Fatalf("expected activeUnhealthy after failures") }
    // One healthy (threshold 1) should mark healthy
    c.updateActiveHealth(addr, true)
    c.mu.RLock(); st = c.state[addr]; c.mu.RUnlock()
    if st == nil || st.activeUnhealthy { t.Fatalf("expected healthy after success threshold") }
}

