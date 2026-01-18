package httpcm

import (
    "bytes"
    "context"
    "net/http"
    "testing"
    "time"

    "github.com/mayurvarma14/go-proxy/internal/cluster"
    "github.com/mayurvarma14/go-proxy/internal/config"
)

func makeReqCtx(t *testing.T, method, url string) *RequestCtx {
    t.Helper()
    req, err := http.NewRequest(method, url, nil)
    if err != nil { t.Fatalf("new request: %v", err) }
    return &RequestCtx{Context: context.Background(), Req: req, W: &bytes.Buffer{}}
}

func TestUpstreamProxy_ClusterAtCapacity_FastFail503(t *testing.T) {
    specs := []config.ClusterSpec{{
        Name: "c1",
        Endpoints: []string{"127.0.0.1:1"},
        CircuitBreaker: &config.CircuitBreakerSpec{MaxRequests: 1, MaxPending: 0},
    }}
    cm, err := cluster.NewManager(specs)
    if err != nil { t.Fatal(err) }
    up := NewUpstreamProxy(cm, specs)

    // Saturate cluster capacity
    if !cm.TryAcquire("c1") { t.Fatal("expected to acquire capacity") }

    rc := makeReqCtx(t, http.MethodGet, "http://example/svc")
    rc.UpstreamCluster = "c1"
    if err := up.OnRequest(rc); err != nil { t.Fatalf("OnRequest: %v", err) }
    if !rc.Responded || rc.StatusCode != http.StatusServiceUnavailable {
        t.Fatalf("want 503 fast-fail, got responded=%v code=%d", rc.Responded, rc.StatusCode)
    }
}

func TestUpstreamProxy_ClusterAtCapacity_PendingTimeout504(t *testing.T) {
    specs := []config.ClusterSpec{{
        Name: "c1",
        Endpoints: []string{"127.0.0.1:1"},
        CircuitBreaker: &config.CircuitBreakerSpec{MaxRequests: 1, MaxPending: 1},
    }}
    cm, err := cluster.NewManager(specs)
    if err != nil { t.Fatal(err) }
    up := NewUpstreamProxy(cm, specs)

    // Saturate cluster capacity
    if !cm.TryAcquire("c1") { t.Fatal("expected to acquire capacity") }

    req, _ := http.NewRequest(http.MethodGet, "http://example/svc", nil)
    // Short request context timeout to trigger pending wait timeout
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
    defer cancel()
    rc := &RequestCtx{Context: ctx, Req: req, W: &bytes.Buffer{}, UpstreamCluster: "c1"}
    if err := up.OnRequest(rc); err != nil { t.Fatalf("OnRequest: %v", err) }
    if !rc.Responded || rc.StatusCode != http.StatusGatewayTimeout {
        t.Fatalf("want 504 from pending timeout, got responded=%v code=%d", rc.Responded, rc.StatusCode)
    }
}

