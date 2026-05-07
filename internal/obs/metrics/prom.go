package metrics

import (
	"regexp"
	"sort"
	"strings"
)

// RenderProm renders the current metrics in Prometheus text exposition format.
// It maps some structured keys into labeled metrics and falls back to
// sanitized names for others.
func RenderProm() string {
	snap := Snapshot()
	keys := make([]string, 0, len(snap))
	for k := range snap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder

	// Precompiled regexes for labeled metrics
	reClusterInflight := regexp.MustCompile(`^cluster\.([^.]+)\.inflight_requests$`)
	reEndpointInflight := regexp.MustCompile(`^endpoint\.([^.]+)\.(.+)\.inflight_requests$`)
	reRespClass := regexp.MustCompile(`^http_responses_([1-5])xx_total$`)
	// Histogram backing keys: hist.<metric>.<cluster>.(bucket.<le>|sum|count)
	reHistBucket := regexp.MustCompile(`^hist\.([^.]+)\.([^.]+)\.bucket\.([A-Za-z0-9]+)$`)
	reHistSum := regexp.MustCompile(`^hist\.([^.]+)\.([^.]+)\.sum$`)
	reHistCount := regexp.MustCompile(`^hist\.([^.]+)\.([^.]+)\.count$`)
	// Labeled downstream requests: downstream_http_requests_total.method.<METHOD>.cluster.<CLUSTER>
	reDownReq := regexp.MustCompile(`^downstream_http_requests_total\.method\.([^.]+)\.cluster\.([^.]+)$`)
	// Labeled upstream outcomes: upstream_requests_total.cluster.<CLUSTER>.outcome.<OUTCOME>
	reUpOutcome := regexp.MustCompile(`^upstream_requests_total\.cluster\.([^.]+)\.outcome\.([^.]+)$`)

	// Helpers
	esc := func(v string) string {
		v = strings.ReplaceAll(v, "\\", "\\\\")
		v = strings.ReplaceAll(v, "\"", "\\\"")
		return v
	}

	// TYPE hints (minimal)
	typeHints := map[string]string{
		"downstream_http_requests_total":        "counter",
		"downstream_connections_accepted_total": "counter",
		"http_responses_total":                  "counter",
		"rate_limit_dropped_total":              "counter",
		"upstream_attempts_total":               "counter",
		"upstream_failures_total":               "counter",
		"endpoints_ejected_total":               "counter",
		"cluster_inflight_requests":             "gauge",
		"endpoint_inflight_requests":            "gauge",
	}
	// Emit TYPE lines once per metric name encountered
	emittedType := map[string]bool{}

	for _, k := range keys {
		v := snap[k]

		if m := reClusterInflight.FindStringSubmatch(k); m != nil {
			name := "cluster_inflight_requests"
			if !emittedType[name] {
				b.WriteString("# TYPE " + name + " " + typeHints[name] + "\n")
				emittedType[name] = true
			}
			b.WriteString(name)
			b.WriteString("{cluster=\"")
			b.WriteString(esc(m[1]))
			b.WriteString("\"}")
			b.WriteString(" ")
			b.WriteString(intToString(v))
			b.WriteString("\n")
			continue
		}
		if m := reEndpointInflight.FindStringSubmatch(k); m != nil {
			name := "endpoint_inflight_requests"
			if !emittedType[name] {
				b.WriteString("# TYPE " + name + " " + typeHints[name] + "\n")
				emittedType[name] = true
			}
			b.WriteString(name)
			b.WriteString("{cluster=\"")
			b.WriteString(esc(m[1]))
			b.WriteString("\",endpoint=\"")
			b.WriteString(esc(m[2]))
			b.WriteString("\"}")
			b.WriteString(" ")
			b.WriteString(intToString(v))
			b.WriteString("\n")
			continue
		}
		if m := reRespClass.FindStringSubmatch(k); m != nil {
			name := "http_responses_total"
			if !emittedType[name] {
				b.WriteString("# TYPE " + name + " " + typeHints[name] + "\n")
				emittedType[name] = true
			}
			b.WriteString(name)
			b.WriteString("{code_class=\"")
			b.WriteString(m[1] + "xx")
			b.WriteString("\"}")
			b.WriteString(" ")
			b.WriteString(intToString(v))
			b.WriteString("\n")
			continue
		}

		// Downstream labeled requests
		if m := reDownReq.FindStringSubmatch(k); m != nil {
			name := "downstream_http_requests_total"
			if !emittedType[name] {
				b.WriteString("# TYPE " + name + " counter\n")
				emittedType[name] = true
			}
			b.WriteString(name)
			b.WriteString("{method=\"")
			b.WriteString(esc(m[1]))
			b.WriteString("\",cluster=\"")
			b.WriteString(esc(m[2]))
			b.WriteString("\"}")
			b.WriteString(" ")
			b.WriteString(intToString(v))
			b.WriteString("\n")
			continue
		}
		// Upstream outcomes labeled
		if m := reUpOutcome.FindStringSubmatch(k); m != nil {
			name := "upstream_requests_total"
			if !emittedType[name] {
				b.WriteString("# TYPE " + name + " counter\n")
				emittedType[name] = true
			}
			b.WriteString(name)
			b.WriteString("{cluster=\"")
			b.WriteString(esc(m[1]))
			b.WriteString("\",outcome=\"")
			b.WriteString(esc(m[2]))
			b.WriteString("\"}")
			b.WriteString(" ")
			b.WriteString(intToString(v))
			b.WriteString("\n")
			continue
		}

		// Histogram buckets (cluster-labeled)
		if m := reHistBucket.FindStringSubmatch(k); m != nil {
			base := m[1] // e.g., upstream_request_duration_ms
			cluster := m[2]
			le := m[3]
			name := base + "_bucket"
			if !emittedType[name] {
				b.WriteString("# TYPE " + name + " histogram\n")
				emittedType[name] = true
			}
			b.WriteString(name)
			b.WriteString("{cluster=\"")
			b.WriteString(esc(cluster))
			b.WriteString("\",le=\"")
			if le == "Inf" {
				b.WriteString("+Inf")
			} else {
				b.WriteString(esc(le))
			}
			b.WriteString("\"}")
			b.WriteString(" ")
			b.WriteString(intToString(v))
			b.WriteString("\n")
			continue
		}
		if m := reHistSum.FindStringSubmatch(k); m != nil {
			base := m[1]
			cluster := m[2]
			name := base + "_sum"
			if !emittedType[name] {
				b.WriteString("# TYPE " + name + " histogram\n")
				emittedType[name] = true
			}
			b.WriteString(name)
			b.WriteString("{cluster=\"")
			b.WriteString(esc(cluster))
			b.WriteString("\"}")
			b.WriteString(" ")
			b.WriteString(intToString(v))
			b.WriteString("\n")
			continue
		}
		if m := reHistCount.FindStringSubmatch(k); m != nil {
			base := m[1]
			cluster := m[2]
			name := base + "_count"
			if !emittedType[name] {
				b.WriteString("# TYPE " + name + " histogram\n")
				emittedType[name] = true
			}
			b.WriteString(name)
			b.WriteString("{cluster=\"")
			b.WriteString(esc(cluster))
			b.WriteString("\"}")
			b.WriteString(" ")
			b.WriteString(intToString(v))
			b.WriteString("\n")
			continue
		}

		// Fallback: sanitize name and emit as a simple metric
		name := sanitizeName(k)
		if t, ok := typeHints[name]; ok && !emittedType[name] {
			b.WriteString("# TYPE " + name + " " + t + "\n")
			emittedType[name] = true
		}
		b.WriteString(name)
		b.WriteString(" ")
		b.WriteString(intToString(v))
		b.WriteString("\n")
	}
	return b.String()
}

func sanitizeName(k string) string {
	// Replace disallowed chars with '_'
	s := k
	s = strings.ReplaceAll(s, ".", "_")
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, "/", "_")
	return s
}

func intToString(v int64) string {
	// Minimal int64 to string without importing fmt to keep this lean
	return strings.TrimLeft(strings.ReplaceAll(strings.TrimSpace(strings.ReplaceAll(strings.Join([]string{"", itoa(v)}, ""), " ", "")), "", ""), " ")
}

// Simple int to string (base 10) helper.
func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := false
	if v < 0 {
		neg = true
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
