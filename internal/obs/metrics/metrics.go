package metrics

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// A minimal metrics registry with counters and gauges.

type counter struct{ v int64 }
type gauge struct{ v int64 }

var (
	muCounters sync.Mutex
	muGauges   sync.Mutex
	counters   = map[string]*counter{}
	gauges     = map[string]*gauge{}
)

func Inc(name string) { Add(name, 1) }

func Add(name string, delta int64) {
	muCounters.Lock()
	c := counters[name]
	if c == nil {
		c = &counter{}
		counters[name] = c
	}
	muCounters.Unlock()
	atomic.AddInt64(&c.v, delta)
}

// AddGauge adjusts a gauge up or down.
func AddGauge(name string, delta int64) {
	muGauges.Lock()
	g := gauges[name]
	if g == nil {
		g = &gauge{}
		gauges[name] = g
	}
	muGauges.Unlock()
	atomic.AddInt64(&g.v, delta)
}

func SetGauge(name string, v int64) {
	muGauges.Lock()
	g := gauges[name]
	if g == nil {
		g = &gauge{}
		gauges[name] = g
	}
	muGauges.Unlock()
	atomic.StoreInt64(&g.v, v)
}

// ObserveUpstreamLatencyMs records a latency sample (in ms) for the given cluster
// into a fixed-bucket histogram. Buckets are cumulative and follow Prometheus
// conventions when rendered.
func ObserveUpstreamLatencyMs(cluster string, ms int64) {
    if ms < 0 { ms = 0 }
    // Fixed buckets (ms)
    buckets := []int64{5, 10, 25, 50, 100, 200, 500, 1000, 2000, 5000}
    for _, b := range buckets {
        if ms <= b {
            Add("hist.upstream_request_duration_ms."+cluster+".bucket."+strconv.FormatInt(b, 10), 1)
        }
    }
    // +Inf bucket (always increment)
    Add("hist.upstream_request_duration_ms."+cluster+".bucket.Inf", 1)
    // Sum and count
    Add("hist.upstream_request_duration_ms."+cluster+".count", 1)
    Add("hist.upstream_request_duration_ms."+cluster+".sum", ms)
}

// IncDownstreamRequest increments a labeled downstream request counter.
// Labels: method, cluster (cluster may be empty if no route matched).
func IncDownstreamRequest(method, cluster string) {
    if method == "" { method = "UNKNOWN" }
    if cluster == "" { cluster = "none" }
    Add("downstream_http_requests_total.method."+strings.ToUpper(method)+".cluster."+cluster, 1)
}

// IncUpstreamOutcome increments labeled upstream request outcomes.
// outcome: success | timeout | error
func IncUpstreamOutcome(cluster, outcome string) {
    if cluster == "" { cluster = "none" }
    if outcome == "" { outcome = "unknown" }
    Add("upstream_requests_total.cluster."+cluster+".outcome."+outcome, 1)
}

// IncCode bumps response counters for total and code class (2xx, 4xx, 5xx).
func IncCode(status int) {
	Inc("http_responses_total")
	class := status / 100
	switch class {
	case 1:
		Inc("http_responses_1xx_total")
	case 2:
		Inc("http_responses_2xx_total")
	case 3:
		Inc("http_responses_3xx_total")
	case 4:
		Inc("http_responses_4xx_total")
	case 5:
		Inc("http_responses_5xx_total")
	default:
		Inc("http_responses_other_total")
	}
}

// Snapshot returns a stable copy of current metrics.
func Snapshot() map[string]int64 {
	out := map[string]int64{}
	muCounters.Lock()
	for k, c := range counters {
		out[k] = atomic.LoadInt64(&c.v)
	}
	muCounters.Unlock()
	muGauges.Lock()
	for k, g := range gauges {
		out[k] = atomic.LoadInt64(&g.v)
	}
	muGauges.Unlock()
	return out
}

// RenderText returns metrics in a simple, sorted text format.
func RenderText() string {
	snap := Snapshot()
	keys := make([]string, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString(" ")
		b.WriteString(strconv.FormatInt(snap[k], 10))
		b.WriteString("\n")
	}
	return b.String()
}
