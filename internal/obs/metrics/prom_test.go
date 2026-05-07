package metrics

import (
	"strings"
	"testing"
)

func TestProm_LabeledDownstreamAndHistogram(t *testing.T) {
	// Downstream labeled counter
	IncDownstreamRequest("GET", "test")
	out := RenderProm()
	if !strings.Contains(out, "downstream_http_requests_total{method=\"GET\",cluster=\"test\"} 1") {
		t.Fatalf("missing labeled downstream counter in prom output:\n%s", out)
	}
	// Histogram sample
	ObserveUpstreamLatencyMs("test", 20)
	out = RenderProm()
	if !strings.Contains(out, "upstream_request_duration_ms_bucket{cluster=\"test\",le=\"25\"}") {
		t.Fatalf("missing histogram bucket in prom output:\n%s", out)
	}
}
