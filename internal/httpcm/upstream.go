package httpcm

import (
    "context"
    "io"
    "net/http"
    "net/url"
    "strings"
    "time"

    "github.com/mayurvarma14/go-proxy/internal/cluster"
    "github.com/mayurvarma14/go-proxy/internal/obs/metrics"
    "github.com/mayurvarma14/go-proxy/internal/config"
    "crypto/tls"
    "errors"
    "math/rand"
)

// UpstreamProxy forwards the HTTP request to a selected upstream endpoint
// (based on rc.UpstreamCluster) and streams the response back.
type UpstreamProxy struct {
    cm  *cluster.Manager
    cli *http.Client                 // default http client
    clients map[string]*http.Client  // per-cluster client (e.g., TLS options)
    tlsEnabled map[string]bool       // cluster uses https
    retry map[string]retryCfg        // per-cluster retry policy
    pending map[string]chan struct{} // bounded pending queues per cluster
}

func NewUpstreamProxy(cm *cluster.Manager, clusters []config.ClusterSpec) UpstreamProxy {
    tr := &http.Transport{
        Proxy:               http.ProxyFromEnvironment,
        MaxIdleConns:        100,
        IdleConnTimeout:     90 * time.Second,
        DisableCompression:  false,
        ForceAttemptHTTP2:   false, // default http client remains h1
        ResponseHeaderTimeout: 10 * time.Second,
    }
    up := UpstreamProxy{
        cm:  cm,
        cli: &http.Client{Transport: tr, Timeout: 0}, // timeouts via transport + ctx
        clients: make(map[string]*http.Client),
        tlsEnabled: make(map[string]bool),
        retry: make(map[string]retryCfg),
        pending: make(map[string]chan struct{}),
    }
    for _, cs := range clusters {
        if cs.UpstreamTLS != nil && cs.UpstreamTLS.Enabled {
            up.tlsEnabled[cs.Name] = true
            tlsConf := &tls.Config{InsecureSkipVerify: cs.UpstreamTLS.InsecureSkipVerify}
            if cs.UpstreamTLS.ServerName != "" { tlsConf.ServerName = cs.UpstreamTLS.ServerName }
            tr2 := &http.Transport{
                Proxy: http.ProxyFromEnvironment,
                MaxIdleConns: 100,
                IdleConnTimeout: 90 * time.Second,
                DisableCompression: false,
                // Enable HTTP/2 to upstreams when using TLS and ALPN allows it
                ForceAttemptHTTP2: true,
                TLSClientConfig: tlsConf,
                ResponseHeaderTimeout: 10 * time.Second,
            }
            up.clients[cs.Name] = &http.Client{Transport: tr2, Timeout: 0}
        }
        // Build retry config
        rc := retryCfg{
            requestTimeout: 0,
            perTryTimeout:  0,
            maxRetries:     0,
            idempotentOnly: true,
            backoffBase:    50 * time.Millisecond,
            backoffMax:     500 * time.Millisecond,
        }
        if cs.RetryPolicy != nil {
            if cs.RetryPolicy.RequestTimeoutMs > 0 { rc.requestTimeout = time.Duration(cs.RetryPolicy.RequestTimeoutMs) * time.Millisecond }
            if cs.RetryPolicy.PerTryTimeoutMs > 0 { rc.perTryTimeout = time.Duration(cs.RetryPolicy.PerTryTimeoutMs) * time.Millisecond }
            if cs.RetryPolicy.MaxRetries > 0 { rc.maxRetries = cs.RetryPolicy.MaxRetries }
            // default idempotentOnly=true even if field not set
            if !cs.RetryPolicy.IdempotentOnly { rc.idempotentOnly = false }
            if cs.RetryPolicy.BackoffBaseMs > 0 { rc.backoffBase = time.Duration(cs.RetryPolicy.BackoffBaseMs) * time.Millisecond }
            if cs.RetryPolicy.BackoffMaxMs > 0 { rc.backoffMax = time.Duration(cs.RetryPolicy.BackoffMaxMs) * time.Millisecond }
        }
        up.retry[cs.Name] = rc
        if cs.CircuitBreaker != nil && cs.CircuitBreaker.MaxPending > 0 {
            up.pending[cs.Name] = make(chan struct{}, cs.CircuitBreaker.MaxPending)
        }
    }
    return up
}

type retryCfg struct {
    requestTimeout time.Duration
    perTryTimeout  time.Duration
    maxRetries     int
    idempotentOnly bool
    backoffBase    time.Duration
    backoffMax     time.Duration
}

func (u UpstreamProxy) OnRequest(rc *RequestCtx) error {
    if rc.UpstreamCluster == "" { // router didn't set a target
        return nil
    }
    
    // Determine retries/timeouts from cluster policy
    pol := u.retry[rc.UpstreamCluster]
    if rc.RetryOverride != nil {
        // Merge: non-zero durations and maxRetries from override replace base
        o := *rc.RetryOverride
        if o.requestTimeout > 0 { pol.requestTimeout = o.requestTimeout }
        if o.perTryTimeout  > 0 { pol.perTryTimeout  = o.perTryTimeout }
        if o.maxRetries     > 0 { pol.maxRetries     = o.maxRetries }
        if o.backoffBase    > 0 { pol.backoffBase    = o.backoffBase }
        if o.backoffMax     > 0 { pol.backoffMax     = o.backoffMax }
    }
    method := strings.ToUpper(rc.Req.Method)
    maxAttempts := 1 + pol.maxRetries
    if pol.idempotentOnly && !(method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions) {
        maxAttempts = 1 // no retries for non-idempotent
    }

    // Overall request timeout wrapper (if configured)
    reqCtx := rc.Context
    var cancelReq context.CancelFunc
    if pol.requestTimeout > 0 {
        reqCtx, cancelReq = context.WithTimeout(rc.Context, pol.requestTimeout)
        defer cancelReq()
    }

    var lastErr error
    for attempt := 0; attempt < maxAttempts; attempt++ {
        // Circuit breaker: try to acquire capacity for this cluster, else optionally wait using pending queue.
        if !u.cm.TryAcquire(rc.UpstreamCluster) {
            if u.waitForCapacity(rc) {
                // we wrote a 503 response
                metrics.Inc("circuit_breaker_trips_total")
                return nil
            }
            // capacity acquired, continue
        }

        // Ensure release after this attempt path exits.
        released := false
        release := func() {
            if !released {
                u.cm.Release(rc.UpstreamCluster)
                released = true
            }
        }
        ep, err := u.cm.Pick(rc.UpstreamCluster)
        if err != nil {
            if errors.Is(err, cluster.ErrNoEndpointCapacity) {
                // release cluster slot then wait until endpoint capacity likely frees
                u.cm.Release(rc.UpstreamCluster)
                if u.waitForCapacity(rc) {
                    metrics.Inc("circuit_breaker_trips_total")
                    return nil
                }
                // acquired capacity; retry from top of loop
                continue
            }
            return err
        }
        rc.UpstreamAddr = ep.Addr
        metrics.Inc("upstream_attempts_total")
        // track per-endpoint inflight for LB policies like least-requests
        u.cm.IncEndpointInFlight(rc.UpstreamCluster, ep.Addr)

        // Per-try timeout (fall back to request ctx if not set)
        tctx := reqCtx
        cancel := func() {}
        if pol.perTryTimeout > 0 {
            tctx, cancel = context.WithTimeout(reqCtx, pol.perTryTimeout)
        }
        // Build upstream URL, honoring rewrite path if set
        fwdPath := rc.Req.URL.Path
        if rc.RewritePath != "" { fwdPath = rc.RewritePath }
        scheme := "http"
        if u.tlsEnabled[rc.UpstreamCluster] { scheme = "https" }
        upURL := &url.URL{Scheme: scheme, Host: ep.Addr, Path: fwdPath, RawQuery: rc.Req.URL.RawQuery}
        req, err := http.NewRequestWithContext(tctx, rc.Req.Method, upURL.String(), rc.Req.Body)
        if err != nil { cancel(); return err }

        copyHeadersExcluding(req.Header, rc.Req.Header,
            "Connection", "Proxy-Connection", "Keep-Alive", "Transfer-Encoding", "Upgrade", "Trailer", "TE",
        )
        req.Host = rc.Req.Host

        // Choose client based on cluster TLS settings
        cli := u.cli
        if ccli, ok := u.clients[rc.UpstreamCluster]; ok { cli = ccli }
        attemptStart := time.Now()
        resp, err := cli.Do(req)
        if err != nil {
            lastErr = err
            cancel()
            u.cm.ReportFailure(rc.UpstreamCluster, ep.Addr)
            metrics.Inc("upstream_failures_total")
            // Classify outcome
            if errors.Is(err, context.DeadlineExceeded) {
                metrics.IncUpstreamOutcome(rc.UpstreamCluster, "timeout")
            } else if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
                metrics.IncUpstreamOutcome(rc.UpstreamCluster, "timeout")
            } else {
                metrics.IncUpstreamOutcome(rc.UpstreamCluster, "error")
            }
            u.cm.DecEndpointInFlight(rc.UpstreamCluster, ep.Addr)
            release()
            // On final attempt, bubble error; else retry next endpoint.
            // Backoff with jitter before next attempt
            if attempt < maxAttempts-1 {
                back := backoffWithJitter(pol.backoffBase, pol.backoffMax, attempt)
                select {
                case <-time.After(back):
                case <-reqCtx.Done():
                    lastErr = reqCtx.Err()
                    attempt = maxAttempts // break outer
                }
            }
            continue
        }

        // Write upstream response downstream.
        dr := &http.Response{
            StatusCode:    resp.StatusCode,
            ProtoMajor:    rc.Req.ProtoMajor,
            ProtoMinor:    rc.Req.ProtoMinor,
            Header:        make(http.Header),
            ContentLength: resp.ContentLength,
            Body:          io.NopCloser(resp.Body),
            Close:         rc.Req.Close,
            Request:       rc.Req,
        }
        copyHeadersExcluding(dr.Header, resp.Header, "Transfer-Encoding", "Connection", "Proxy-Connection", "Keep-Alive", "Upgrade", "Trailer", "TE")

        if err := dr.Write(rc.W); err != nil { _ = resp.Body.Close(); cancel(); u.cm.DecEndpointInFlight(rc.UpstreamCluster, ep.Addr); release(); return err }
        _ = resp.Body.Close()
        cancel()
        u.cm.ReportSuccess(rc.UpstreamCluster, ep.Addr)
        u.cm.DecEndpointInFlight(rc.UpstreamCluster, ep.Addr)
        // Record upstream latency (ms) per cluster for histograms
        metrics.ObserveUpstreamLatencyMs(rc.UpstreamCluster, time.Since(attemptStart).Milliseconds())
        metrics.IncUpstreamOutcome(rc.UpstreamCluster, "success")
        rc.StatusCode = resp.StatusCode
        rc.Responded = true
        release()
        return nil
    }
    // Classify timeouts so HCM can return 504 vs 502
    if lastErr == nil {
        return errors.New("upstream failed without error")
    }
    if errors.Is(lastErr, context.DeadlineExceeded) {
        return context.DeadlineExceeded
    }
    if ne, ok := lastErr.(interface{ Timeout() bool }); ok && ne.Timeout() {
        return context.DeadlineExceeded
    }
    return lastErr
}

// waitForCapacity handles bounded waiting when a cluster is at capacity or all endpoints are at per-endpoint cap.
// Returns true if it wrote a 503 response (fast-fail), false if capacity was acquired and caller should retry.
func (u UpstreamProxy) waitForCapacity(rc *RequestCtx) bool {
    sem, ok := u.pending[rc.UpstreamCluster]
    if !ok {
        // No pending configured: fast fail
        body := "circuit breaker open\n"
        dr := &http.Response{
            StatusCode:    http.StatusServiceUnavailable,
            ProtoMajor:    rc.Req.ProtoMajor,
            ProtoMinor:    rc.Req.ProtoMinor,
            Header:        make(http.Header),
            ContentLength: int64(len(body)),
            Body:          io.NopCloser(strings.NewReader(body)),
            Close:         true,
            Request:       rc.Req,
        }
        dr.Header.Set("Content-Type", "text/plain; charset=utf-8")
        _ = dr.Write(rc.W)
        rc.StatusCode = http.StatusServiceUnavailable
        rc.Responded = true
        metrics.Inc("circuit_breaker_open_total")
        return true
    }
    // Try to enter pending queue (non-blocking)
    select {
    case sem <- struct{}{}:
        metrics.AddGauge("cluster."+rc.UpstreamCluster+".pending_current", 1)
    default:
        // queue full: fast fail
        body := "circuit breaker open\n"
        dr := &http.Response{
            StatusCode:    http.StatusServiceUnavailable,
            ProtoMajor:    rc.Req.ProtoMajor,
            ProtoMinor:    rc.Req.ProtoMinor,
            Header:        make(http.Header),
            ContentLength: int64(len(body)),
            Body:          io.NopCloser(strings.NewReader(body)),
            Close:         true,
            Request:       rc.Req,
        }
        dr.Header.Set("Content-Type", "text/plain; charset=utf-8")
        _ = dr.Write(rc.W)
        rc.StatusCode = http.StatusServiceUnavailable
        rc.Responded = true
        metrics.Inc("circuit_breaker_open_total")
        return true
    }
    // Poll for capacity
    ticker := time.NewTicker(10 * time.Millisecond)
    defer ticker.Stop()
    for {
        if u.cm.TryAcquire(rc.UpstreamCluster) {
            <-sem
            metrics.AddGauge("cluster."+rc.UpstreamCluster+".pending_current", -1)
            return false
        }
        select {
        case <-rc.Context.Done():
            <-sem
            metrics.AddGauge("cluster."+rc.UpstreamCluster+".pending_current", -1)
            metrics.Inc("pending_dropped_total")
            // Write 504 Gateway Timeout
            body := "gateway timeout\n"
            dr := &http.Response{
                StatusCode:    http.StatusGatewayTimeout,
                ProtoMajor:    rc.Req.ProtoMajor,
                ProtoMinor:    rc.Req.ProtoMinor,
                Header:        make(http.Header),
                ContentLength: int64(len(body)),
                Body:          io.NopCloser(strings.NewReader(body)),
                Close:         true,
                Request:       rc.Req,
            }
            dr.Header.Set("Content-Type", "text/plain; charset=utf-8")
            _ = dr.Write(rc.W)
            rc.StatusCode = http.StatusGatewayTimeout
            rc.Responded = true
            return true
        case <-ticker.C:
        }
    }
}

func copyHeadersExcluding(dst, src http.Header, exclude ...string) {
    skip := make(map[string]struct{}, len(exclude))
    for _, k := range exclude { skip[strings.ToLower(k)] = struct{}{} }
    for k, vv := range src {
        if _, ok := skip[strings.ToLower(k)]; ok { continue }
        for _, v := range vv { dst.Add(k, v) }
    }
}

func backoffWithJitter(base, max time.Duration, attempt int) time.Duration {
    if base <= 0 { base = 50 * time.Millisecond }
    if max <= 0 { max = 500 * time.Millisecond }
    // exponential: base * 2^(attempt)
    d := base << uint(attempt)
    if d > max { d = max }
    // add jitter up to 25%
    jitter := time.Duration(rand.Int63n(int64(d) / 4 + 1))
    return d + jitter
}
