package httpcm

import (
    "bytes"
    "io"
    "net"
    "net/http"
    "strings"
    "sync"
    "time"

    "github.com/mayurvarma14/go-proxy/internal/config"
    "github.com/mayurvarma14/go-proxy/internal/obs/metrics"
)

// RateLimiter is a simple token-bucket local rate limiter.
type RateLimiter struct {
    scope string // "global" or "ip"

    mu     sync.Mutex
    global *bucket
    perIP  map[string]*bucket
    defCap float64
    defRate float64
}

type bucket struct {
    cap   float64
    rate  float64 // tokens per second
    tok   float64
    last  time.Time
}

// NewRateLimiter builds a limiter from config. If cfg is nil or invalid, returns nil (no-op).
func NewRateLimiter(cfg *config.RateLimitSpec) *RateLimiter {
    if cfg == nil || cfg.RequestsPerSecond <= 0 {
        return nil
    }
    scope := strings.ToLower(cfg.Scope)
    if scope == "" { scope = "global" }

    // defaults for buckets
    defRate := float64(cfg.RequestsPerSecond)
    defCap := float64(cfg.Burst)
    if defCap <= 0 { defCap = defRate }
    if defCap < 1 { defCap = 1 }

    mk := func() *bucket {
        b := &bucket{cap: defCap, rate: defRate}
        b.tok = b.cap // start full to allow initial burst
        b.last = time.Now()
        return b
    }

    rl := &RateLimiter{scope: scope, defCap: defCap, defRate: defRate}
    if scope == "ip" {
        rl.perIP = make(map[string]*bucket)
    } else {
        rl.global = mk()
    }
    // store factory in global field via nil marker; perIP buckets created on demand using cfg
    if rl.global != nil {
        // ok
    }
    // Keep cfg defaults in closures by capturing values in mk.
    return rl
}

func (rl *RateLimiter) getBucket(ip string) *bucket {
    if rl == nil { return nil }
    if rl.scope != "ip" {
        return rl.global
    }
    b := rl.perIP[ip]
    if b == nil {
        cap := rl.defCap
        rate := rl.defRate
        b = &bucket{cap: cap, rate: rate, tok: cap, last: time.Now()}
        rl.perIP[ip] = b
    }
    return b
}

func (b *bucket) allow(now time.Time) bool {
    // Refill tokens
    if now.After(b.last) {
        dt := now.Sub(b.last).Seconds()
        b.tok += dt * b.rate
        if b.tok > b.cap { b.tok = b.cap }
        b.last = now
    }
    if b.tok >= 1.0 {
        b.tok -= 1.0
        return true
    }
    return false
}

// OnRequest enforces the rate limit. When limited, writes 429 and marks Responded.
func (rl *RateLimiter) OnRequest(rc *RequestCtx) error {
    if rl == nil { return nil }

    ip := "global"
    if rl.scope == "ip" {
        // extract IP from remote address
        if ta, ok := rc.Remote.(*net.TCPAddr); ok {
            ip = ta.IP.String()
        } else {
            // best-effort parse: split host:port
            parts := strings.Split(rc.Remote.String(), ":")
            if len(parts) > 0 { ip = parts[0] }
        }
    }
    rl.mu.Lock()
    b := rl.getBucket(ip)
    now := time.Now()
    allowed := b.allow(now)
    rl.mu.Unlock()

    if allowed {
        return nil
    }

    // Deny with 429
    body := "rate limited\n"
    resp := http.Response{
        StatusCode:    http.StatusTooManyRequests,
        ProtoMajor:    rc.Req.ProtoMajor,
        ProtoMinor:    rc.Req.ProtoMinor,
        Header:        make(http.Header),
        ContentLength: int64(len(body)),
        Body:          io.NopCloser(bytes.NewReader(nil)),
        Close:         true,
        Request:       rc.Req,
    }
    resp.Header.Set("Content-Type", "text/plain; charset=utf-8")
    // Write manually to rc.W to avoid allocating a new reader for Body content.
    // But http.Response requires Body; we can just write via Response with Body set to a reader.
    resp.Body = io.NopCloser(bytes.NewReader([]byte(body)))
    _ = resp.Write(rc.W)
    rc.StatusCode = http.StatusTooManyRequests
    rc.Responded = true
    metrics.Inc("rate_limit_dropped_total")
    return nil
}
