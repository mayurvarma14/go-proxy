package cluster

import (
    "context"
    "errors"
    "fmt"
    "net"
    "net/http"
    "math/rand"
    "strings"
    "sync"
    "sync/atomic"
    "time"

    "github.com/mayurvarma14/go-proxy/internal/config"
    "github.com/mayurvarma14/go-proxy/internal/obs/metrics"
)

// Endpoint is a simple upstream target (host:port).
type Endpoint struct{ Addr string }

type outlierCfg struct {
	consecutive int
	ejectFor    time.Duration
}

type epState struct {
    failures       int
    unhealthyUntil time.Time
    // Active health check state
    activeUnhealthy bool
    consecSucc      int
    consecFail      int
    inFlight        int64 // per-endpoint in-flight requests (for least-requests)
}

// Cluster holds endpoints, outlier config, and round-robin state.
type Cluster struct {
    Name      string
    Endpoints []Endpoint
    rr        uint64 // round-robin index

    outlier outlierCfg
    mu      sync.RWMutex
    state   map[string]*epState // key: endpoint addr
    // Active health check config (nil means disabled)
    hc *config.HealthCheckSpec
    // Per-endpoint cancel for active health checks (for dynamic add/remove)
    hcCancel map[string]context.CancelFunc

    // Circuit breaker
    cbMaxReqs int
    inflight  int64 // in-flight requests

    lbPolicy string // "round_robin" (default), "random", "least_requests"

    // Service discovery
    discovery    string        // "strict_dns" or ""
    dnsRefresh   time.Duration // refresh interval
    seeds        []string      // original configured endpoints (host:port)
    staticSeeds  map[string]struct{} // IP-literal endpoints from seeds
    // For strict_dns: latest resolved ip:port set per seed host:port
    resolvedBySeed map[string]map[string]struct{}

    // Runtime base context set when Manager.Start is called; used for dynamic goroutines
    baseCtx context.Context

    // Per-endpoint breaker cap
    epMaxReqs int
}

// Manager indexes clusters by name and selects endpoints.
type Manager struct{
    byName map[string]*Cluster
    rnd    *rand.Rand
}

func NewManager(specs []config.ClusterSpec) (*Manager, error) {
    m := &Manager{byName: make(map[string]*Cluster), rnd: rand.New(rand.NewSource(time.Now().UnixNano()))}
	for _, s := range specs {
		if s.Name == "" || len(s.Endpoints) == 0 {
			return nil, fmt.Errorf("invalid cluster %q: empty name or endpoints", s.Name)
		}
		c := &Cluster{
            Name: s.Name,
            state: make(map[string]*epState),
            hcCancel: make(map[string]context.CancelFunc),
            seeds: append([]string(nil), s.Endpoints...),
            staticSeeds: make(map[string]struct{}),
            resolvedBySeed: make(map[string]map[string]struct{}),
        }
		// outlier defaults
		c.outlier.consecutive = 3
		c.outlier.ejectFor = 30 * time.Second
		if s.Outlier != nil {
			if s.Outlier.ConsecutiveFailures > 0 {
				c.outlier.consecutive = s.Outlier.ConsecutiveFailures
			}
			if s.Outlier.EjectionSeconds > 0 {
				c.outlier.ejectFor = time.Duration(s.Outlier.EjectionSeconds) * time.Second
			}
		}
        // Discovery mode
        c.discovery = strings.ToLower(strings.TrimSpace(s.Discovery))
        if s.DnsRefreshSeconds > 0 {
            c.dnsRefresh = time.Duration(s.DnsRefreshSeconds) * time.Second
        } else {
            c.dnsRefresh = 5 * time.Second
        }
        // Seed endpoints: keep IP-literals as static; hostnames will be resolved if strict_dns
        for _, e := range s.Endpoints {
            host, port, err := net.SplitHostPort(e)
            if err != nil {
                // If malformed, treat as opaque address
                c.Endpoints = append(c.Endpoints, Endpoint{Addr: e})
                if _, ok := c.state[e]; !ok { c.state[e] = &epState{} }
                continue
            }
            if ip := net.ParseIP(host); ip != nil {
                // static IP seed
                addr := net.JoinHostPort(ip.String(), port)
                c.Endpoints = append(c.Endpoints, Endpoint{Addr: addr})
                c.staticSeeds[addr] = struct{}{}
                if _, ok := c.state[addr]; !ok { c.state[addr] = &epState{} }
            } else {
                // hostname seed; if not strict_dns, treat as static
                if c.discovery != "strict_dns" {
                    c.Endpoints = append(c.Endpoints, Endpoint{Addr: e})
                    if _, ok := c.state[e]; !ok { c.state[e] = &epState{} }
                }
            }
        }
        // attach health check config
        if s.HealthCheck != nil {
            hc := *s.HealthCheck
            if hc.Type == "" { hc.Type = "tcp" }
            if hc.IntervalMillis <= 0 { hc.IntervalMillis = 2000 }
            if hc.TimeoutMillis <= 0 { hc.TimeoutMillis = 1000 }
            if hc.HTTPPath == "" { hc.HTTPPath = "/" }
            if hc.HealthyThreshold <= 0 { hc.HealthyThreshold = 1 }
            if hc.UnhealthyThreshold <= 0 { hc.UnhealthyThreshold = 3 }
            c.hc = &hc
        }
        // circuit breaker
        if s.CircuitBreaker != nil {
            if s.CircuitBreaker.MaxRequests > 0 {
                c.cbMaxReqs = s.CircuitBreaker.MaxRequests
            }
            if s.CircuitBreaker.PerEndpointMaxRequests > 0 {
                c.epMaxReqs = s.CircuitBreaker.PerEndpointMaxRequests
            }
        }
        // lb policy
        switch strings.ToLower(s.LBPolicy) {
        case "", "round_robin":
            c.lbPolicy = "round_robin"
        case "random":
            c.lbPolicy = "random"
        case "least_requests":
            c.lbPolicy = "least_requests"
        default:
            c.lbPolicy = "round_robin"
        }
        m.byName[c.Name] = c
    }
    return m, nil
}

// ClusterDebug summarizes cluster and endpoint runtime state for admin/debug.
type ClusterDebug struct {
    Name                     string           `json:"name"`
    LBPolicy                 string           `json:"lb_policy"`
    MaxRequests              int              `json:"max_requests"`
    PerEndpointMaxRequests   int              `json:"per_endpoint_max_requests"`
    InFlight                 int64            `json:"inflight"`
    Endpoints                []EndpointDebug  `json:"endpoints"`
}

// EndpointDebug summarizes endpoint state.
type EndpointDebug struct {
    Address        string    `json:"address"`
    ActiveHealthy  bool      `json:"active_healthy"`
    PassiveEjected bool      `json:"passive_ejected"`
    EjectedUntil   time.Time `json:"ejected_until,omitempty"`
    InFlight       int64     `json:"inflight"`
}

// DebugSnapshot returns a stable snapshot of all clusters and endpoint states.
func (m *Manager) DebugSnapshot() []ClusterDebug {
    out := make([]ClusterDebug, 0, len(m.byName))
    now := time.Now()
    for _, c := range m.byName {
        cd := ClusterDebug{
            Name:                   c.Name,
            LBPolicy:               c.lbPolicy,
            MaxRequests:            c.cbMaxReqs,
            PerEndpointMaxRequests: c.epMaxReqs,
            InFlight:               atomic.LoadInt64(&c.inflight),
        }
        eps := c.snapshotEndpoints()
        for _, ep := range eps {
            c.mu.RLock()
            st := c.state[ep.Addr]
            c.mu.RUnlock()
            var infl int64
            var activeUnhealthy bool
            var ejectedUntil time.Time
            if st != nil {
                infl = atomic.LoadInt64(&st.inFlight)
                activeUnhealthy = st.activeUnhealthy
                ejectedUntil = st.unhealthyUntil
            }
            ed := EndpointDebug{
                Address:        ep.Addr,
                ActiveHealthy:  !activeUnhealthy,
                PassiveEjected: now.Before(ejectedUntil),
                EjectedUntil:   ejectedUntil,
                InFlight:       infl,
            }
            cd.Endpoints = append(cd.Endpoints, ed)
        }
        out = append(out, cd)
    }
    return out
}

// ErrNoEndpointCapacity indicates all eligible endpoints are at per-endpoint max.
var ErrNoEndpointCapacity = errors.New("no endpoint capacity")

// isEjected returns whether the endpoint is currently unhealthy.
func (c *Cluster) isEjected(addr string, now time.Time) bool {
    c.mu.RLock()
    st := c.state[addr]
    c.mu.RUnlock()
    if st == nil {
        return false
    }
    return now.Before(st.unhealthyUntil)
}

func (c *Cluster) isAvailable(addr string, now time.Time) bool {
    c.mu.RLock()
    st := c.state[addr]
    c.mu.RUnlock()
    if st == nil { return true }
    if st.activeUnhealthy { return false }
    if now.Before(st.unhealthyUntil) { return false }
    return true
}

// markFailure increments failure count and may eject the endpoint.
func (c *Cluster) markFailure(addr string, now time.Time) {
    c.mu.Lock()
    st := c.state[addr]
    if st == nil {
        st = &epState{}
        c.state[addr] = st
    }
    st.failures++
    if st.failures >= c.outlier.consecutive {
        st.unhealthyUntil = now.Add(c.outlier.ejectFor)
        st.failures = 0
        metrics.Inc("endpoints_ejected_total")
    }
    c.mu.Unlock()
}

// markSuccess resets failure count and clears ejection if expired.
func (c *Cluster) markSuccess(addr string) {
	c.mu.Lock()
	st := c.state[addr]
	if st == nil {
		st = &epState{}
		c.state[addr] = st
	}
	st.failures = 0
	c.mu.Unlock()
}

// Pick returns a healthy endpoint if available, otherwise any endpoint.
func (m *Manager) Pick(clusterName string) (Endpoint, error) {
    c := m.byName[clusterName]
    if c == nil {
        return Endpoint{}, errors.New("unknown or empty cluster: " + clusterName)
    }
    eps := c.snapshotEndpoints()
    if len(eps) == 0 {
        return Endpoint{}, errors.New("unknown or empty cluster: " + clusterName)
    }
    n := len(eps)
    now := time.Now()

    switch c.lbPolicy {
    case "random":
        // Build list of healthy endpoints
        healthy := make([]Endpoint, 0, n)
        healthyUnderCap := make([]Endpoint, 0, n)
        for _, ep := range eps {
            if c.isAvailable(ep.Addr, now) {
                healthy = append(healthy, ep)
                if c.underEndpointCap(ep.Addr) {
                    healthyUnderCap = append(healthyUnderCap, ep)
                }
            }
        }
        if len(healthyUnderCap) > 0 {
            return healthyUnderCap[m.rnd.Intn(len(healthyUnderCap))], nil
        }
        if len(healthy) == 0 {
            // fall back: pick any random
            return eps[m.rnd.Intn(n)], nil
        }
        // healthy exist but all at per-endpoint cap
        return Endpoint{}, ErrNoEndpointCapacity
    case "least_requests":
        var sel Endpoint
        var min int64 = -1
        for _, ep := range eps {
            if !c.isAvailable(ep.Addr, now) {
                continue
            }
            c.mu.RLock()
            st := c.state[ep.Addr]
            c.mu.RUnlock()
            var cur int64
            if st != nil { cur = atomic.LoadInt64(&st.inFlight) }
            if !c.underEndpointCap(ep.Addr) {
                continue
            }
            if min == -1 || cur < min {
                min = cur
                sel = ep
            }
        }
        if min != -1 {
            return sel, nil
        }
        // Either none healthy or all at cap; fall back to round robin logic next
        fallthrough
    default: // round_robin
        start := int(atomic.AddUint64(&c.rr, 1)-1) % n
        // try to find a healthy endpoint across the ring
        var anyHealthy bool
        for i := 0; i < n; i++ {
            idx := (start + i) % n
            ep := eps[idx]
            if c.isAvailable(ep.Addr, now) {
                anyHealthy = true
                if c.underEndpointCap(ep.Addr) {
                    return ep, nil
                }
            }
        }
        if anyHealthy {
            // healthy exist but all at per-endpoint cap
            return Endpoint{}, ErrNoEndpointCapacity
        }
        // if all are ejected, return round-robin anyway
        return eps[start], nil
    }
}

func (c *Cluster) underEndpointCap(addr string) bool {
    if c.epMaxReqs <= 0 { return true }
    c.mu.RLock()
    st := c.state[addr]
    c.mu.RUnlock()
    if st == nil { return true }
    cur := atomic.LoadInt64(&st.inFlight)
    return cur < int64(c.epMaxReqs)
}

// Endpoint in-flight accounting (for least-requests)
func (m *Manager) IncEndpointInFlight(clusterName, addr string) {
    c := m.byName[clusterName]
    if c == nil { return }
    c.mu.RLock()
    st := c.state[addr]
    c.mu.RUnlock()
    if st == nil { return }
    v := atomic.AddInt64(&st.inFlight, 1)
    metrics.SetGauge("endpoint."+c.Name+"."+addr+".inflight_requests", v)
}

func (m *Manager) DecEndpointInFlight(clusterName, addr string) {
    c := m.byName[clusterName]
    if c == nil { return }
    c.mu.RLock()
    st := c.state[addr]
    c.mu.RUnlock()
    if st == nil { return }
    v := atomic.AddInt64(&st.inFlight, -1)
    if v < 0 {
        atomic.StoreInt64(&st.inFlight, 0)
        v = 0
    }
    metrics.SetGauge("endpoint."+c.Name+"."+addr+".inflight_requests", v)
}

// ReportFailure should be called when a request to addr fails.
func (m *Manager) ReportFailure(clusterName, addr string) {
	if c := m.byName[clusterName]; c != nil {
		c.markFailure(addr, time.Now())
	}
}

// ReportSuccess should be called on a successful response.
func (m *Manager) ReportSuccess(clusterName, addr string) {
    if c := m.byName[clusterName]; c != nil {
        c.markSuccess(addr)
    }
}

// Start active health checks for all endpoints that have health check config.
func (m *Manager) Start(ctx context.Context) {
    for _, c := range m.byName {
        c.baseCtx = ctx
        // Start per-endpoint health checks for current endpoints
        if c.hc != nil {
            for _, ep := range c.snapshotEndpoints() {
                c.startHealthCheck(ctx, ep.Addr)
            }
        }
        // Start discovery loops for strict_dns
        if c.discovery == "strict_dns" {
            c.startDiscovery(ctx)
        }
    }
}

// snapshotEndpoints returns a copy of current endpoints under read lock.
func (c *Cluster) snapshotEndpoints() []Endpoint {
    c.mu.RLock()
    defer c.mu.RUnlock()
    out := make([]Endpoint, len(c.Endpoints))
    copy(out, c.Endpoints)
    return out
}

// startHealthCheck ensures an active health-check goroutine for addr.
func (c *Cluster) startHealthCheck(ctx context.Context, addr string) {
    if c.hc == nil { return }
    c.mu.Lock()
    if _, exists := c.hcCancel[addr]; exists {
        c.mu.Unlock()
        return
    }
    epCtx, cancel := context.WithCancel(ctx)
    c.hcCancel[addr] = cancel
    c.mu.Unlock()
    go c.runHealthCheck(epCtx, addr)
}

// stopHealthCheck cancels the active health check for addr if running.
func (c *Cluster) stopHealthCheck(addr string) {
    c.mu.Lock()
    cancel, ok := c.hcCancel[addr]
    if ok {
        delete(c.hcCancel, addr)
    }
    c.mu.Unlock()
    if ok { cancel() }
}

// startDiscovery launches resolver goroutines for each hostname seed.
func (c *Cluster) startDiscovery(ctx context.Context) {
    // For each seed, if host is a hostname (not IP), run a resolver loop
    for _, seed := range c.seeds {
        host, port, err := net.SplitHostPort(seed)
        if err != nil { continue }
        if ip := net.ParseIP(host); ip != nil {
            continue // static seed already added
        }
        go c.resolveLoop(ctx, seed, host, port)
    }
}

func (c *Cluster) resolveLoop(ctx context.Context, seed string, host, port string) {
    refresh := c.dnsRefresh
    if refresh <= 0 { refresh = 5 * time.Second }
    ticker := time.NewTicker(refresh)
    defer ticker.Stop()

    // Run immediately, then on each tick
    c.resolveOnce(ctx, seed, host, port)
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            c.resolveOnce(ctx, seed, host, port)
        }
    }
}

func (c *Cluster) resolveOnce(ctx context.Context, seed, host, port string) {
    // Perform DNS lookup (A/AAAA)
    metrics.Inc("dns_resolve_total")
    // Bound the DNS call to avoid long hangs
    lctx, cancel := context.WithTimeout(ctx, 2*time.Second)
    defer cancel()
    addrs, err := net.DefaultResolver.LookupIPAddr(lctx, host)
    if err != nil {
        metrics.Inc("dns_resolve_errors_total")
        return
    }
    // Build set of ip:port strings
    resolved := make(map[string]struct{}, len(addrs))
    for _, ipa := range addrs {
        addr := net.JoinHostPort(ipa.IP.String(), port)
        resolved[addr] = struct{}{}
    }
    // Store per-seed and update effective endpoints
    c.mu.Lock()
    // replace map for this seed
    cp := make(map[string]struct{}, len(resolved))
    for k := range resolved { cp[k] = struct{}{} }
    c.resolvedBySeed[seed] = cp
    // Compute union: staticSeeds ∪ all resolvedBySeed
    effective := make(map[string]struct{}, len(c.staticSeeds)+len(resolved))
    for addr := range c.staticSeeds { effective[addr] = struct{}{} }
    for _, set := range c.resolvedBySeed {
        for addr := range set { effective[addr] = struct{}{} }
    }
    c.mu.Unlock()
    c.applyEndpointSet(effective)
}

// applyEndpointSet atomically applies the given set of endpoint addresses.
func (c *Cluster) applyEndpointSet(newSet map[string]struct{}) {
    c.mu.Lock()
    // current set
    curSet := make(map[string]struct{}, len(c.Endpoints))
    for _, ep := range c.Endpoints { curSet[ep.Addr] = struct{}{} }

    // additions
    var toAdd []string
    for addr := range newSet {
        if _, ok := curSet[addr]; !ok {
            toAdd = append(toAdd, addr)
        }
    }
    // removals
    var toRemove []string
    for addr := range curSet {
        if _, ok := newSet[addr]; !ok {
            toRemove = append(toRemove, addr)
        }
    }

    if len(toAdd) == 0 && len(toRemove) == 0 {
        c.mu.Unlock()
        return
    }

    // Build new slice
    next := make([]Endpoint, 0, len(newSet))
    for addr := range newSet { next = append(next, Endpoint{Addr: addr}) }
    c.Endpoints = next
    // ensure state entries exist
    for _, addr := range toAdd {
        if _, ok := c.state[addr]; !ok { c.state[addr] = &epState{} }
    }
    c.mu.Unlock()

    // Start/stop health checks outside lock
    for _, addr := range toAdd {
        if c.hc != nil && c.baseCtx != nil {
            c.startHealthCheck(c.baseCtx, addr)
        }
        metrics.Inc("endpoints_added_total")
    }
    for _, addr := range toRemove {
        c.stopHealthCheck(addr)
        // Zero the in-flight gauge for removed endpoint if no in-flight remains
        // Note: we keep epState so in-flight counters can drain if any still active.
        metrics.SetGauge("endpoint."+c.Name+"."+addr+".inflight_requests", 0)
        metrics.Inc("endpoints_removed_total")
    }
    metrics.SetGauge("cluster."+c.Name+".endpoints", int64(len(newSet)))
}

func (c *Cluster) runHealthCheck(ctx context.Context, addr string) {
    interval := time.Duration(c.hc.IntervalMillis) * time.Millisecond
    timeout := time.Duration(c.hc.TimeoutMillis) * time.Millisecond
    ticker := time.NewTicker(interval)
    defer ticker.Stop()
    httpClient := &http.Client{Timeout: timeout}
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            healthy := false
            switch strings.ToLower(c.hc.Type) {
            case "http":
                url := "http://" + addr + c.hc.HTTPPath
                resp, err := httpClient.Get(url)
                if err == nil {
                    if resp.StatusCode < 500 { healthy = true }
                    _ = resp.Body.Close()
                }
            default: // tcp
                d := net.Dialer{Timeout: timeout}
                conn, err := d.DialContext(ctx, "tcp", addr)
                if err == nil {
                    healthy = true
                    _ = conn.Close()
                }
            }
            c.updateActiveHealth(addr, healthy)
        }
    }
}

func (c *Cluster) updateActiveHealth(addr string, healthy bool) {
    c.mu.Lock()
    st := c.state[addr]
    if st == nil { st = &epState{}; c.state[addr] = st }
    if healthy {
        st.consecSucc++
        st.consecFail = 0
        if st.activeUnhealthy && st.consecSucc >= c.hc.HealthyThreshold {
            st.activeUnhealthy = false
        }
    } else {
        st.consecFail++
        st.consecSucc = 0
        if !st.activeUnhealthy && st.consecFail >= c.hc.UnhealthyThreshold {
            st.activeUnhealthy = true
        }
    }
    c.mu.Unlock()
}

// TryAcquire increments cluster in-flight if below limit. Returns true if acquired.
func (m *Manager) TryAcquire(clusterName string) bool {
    c := m.byName[clusterName]
    if c == nil {
        return true
    }
    if c.cbMaxReqs <= 0 {
        v := atomic.AddInt64(&c.inflight, 1)
        metrics.SetGauge("cluster."+c.Name+".inflight_requests", v)
        return true
    }
    v := atomic.AddInt64(&c.inflight, 1)
    if v > int64(c.cbMaxReqs) {
        // roll back and deny
        atomic.AddInt64(&c.inflight, -1)
        return false
    }
    metrics.SetGauge("cluster."+c.Name+".inflight_requests", v)
    return true
}

// Release decrements cluster in-flight counter.
func (m *Manager) Release(clusterName string) {
    c := m.byName[clusterName]
    if c == nil {
        return
    }
    v := atomic.AddInt64(&c.inflight, -1)
    if v < 0 {
        atomic.StoreInt64(&c.inflight, 0)
        v = 0
    }
    metrics.SetGauge("cluster."+c.Name+".inflight_requests", v)
}
