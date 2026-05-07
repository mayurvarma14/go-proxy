package config

// ProxyConfig is the root of the static config file.
type ProxyConfig struct {
	Listeners []ListenerSpec `json:"listeners"`
	Routes    []RouteRule    `json:"routes"`
	Clusters  []ClusterSpec  `json:"clusters"`
	RateLimit *RateLimitSpec `json:"rate_limit,omitempty"`
}

// ListenerSpec describes one downstream entrypoint.
type ListenerSpec struct {
	Name         string        `json:"name"`
	Address      string        `json:"address"` // e.g. "0.0.0.0:8080"
	TLS          *TLSConfig    `json:"tls,omitempty"`
	FilterChains []FilterChain `json:"filter_chains"`
}

// FilterChain is an ordered list of network filters (by name) to apply.
type FilterChain struct {
	Name    string   `json:"name"`
	Filters []string `json:"filters"`
	// Optional SNI match list for TLS listeners. If set, this chain is
	// selected when the downstream TLS ServerName matches any entry.
	// Use exact hostnames; wildcard matching is not implemented.
	SNIHosts []string `json:"sni_hosts,omitempty"`
}

// TLSConfig enables TLS termination on a listener.
type TLSConfig struct {
	CertPath   string `json:"cert_path"`
	KeyPath    string `json:"key_path"`
	MinVersion string `json:"min_version,omitempty"` // "1.2" (default) or "1.3"
	// Optional client auth (mTLS). If RequireClientCert is true, the proxy
	// requires and verifies a client certificate against ClientCAPath.
	ClientCAPath      string `json:"client_ca_path,omitempty"`
	RequireClientCert bool   `json:"require_client_cert,omitempty"`
	// Additional certificates keyed by SNI hosts.
	Certs []TLSCertEntry `json:"certs,omitempty"`
}

type TLSCertEntry struct {
	CertPath string   `json:"cert_path"`
	KeyPath  string   `json:"key_path"`
	SNIHosts []string `json:"sni_hosts"`
}

// RouteRule maps a URL path prefix to a cluster name.
type RouteRule struct {
	Prefix  string `json:"prefix"`
	Cluster string `json:"cluster"`
	// If true, remove the matched prefix before forwarding (e.g., /svc -> /)
	StripPrefix bool `json:"strip_prefix,omitempty"`
	// If set, replace the matched prefix with this string (e.g., /svc -> /api)
	PrefixRewrite string `json:"prefix_rewrite,omitempty"`
	// Per-route header manipulation and overrides
	HostRewrite   string            `json:"host_rewrite,omitempty"`
	SetHeaders    map[string]string `json:"set_headers,omitempty"`
	RemoveHeaders []string          `json:"remove_headers,omitempty"`
	// Optional per-route retry policy (overrides cluster policy for the route)
	RetryPolicy *RetryPolicy `json:"retry_policy,omitempty"`
}

// ClusterSpec defines a logical upstream service with endpoints.
type ClusterSpec struct {
	Name              string              `json:"name"`
	Endpoints         []string            `json:"endpoints"`           // host:port strings
	Discovery         string              `json:"discovery,omitempty"` // "strict_dns" or empty (static)
	DnsRefreshSeconds int                 `json:"dns_refresh_seconds,omitempty"`
	Outlier           *OutlierSpec        `json:"outlier,omitempty"`
	HealthCheck       *HealthCheckSpec    `json:"health_check,omitempty"`
	CircuitBreaker    *CircuitBreakerSpec `json:"circuit_breaker,omitempty"`
	LBPolicy          string              `json:"lb_policy,omitempty"` // "round_robin" (default), "random", "least_requests"
	UpstreamTLS       *UpstreamTLSConfig  `json:"upstream_tls,omitempty"`
	RetryPolicy       *RetryPolicy        `json:"retry_policy,omitempty"`
}

// UpstreamTLSConfig enables TLS to upstream endpoints.
type UpstreamTLSConfig struct {
	Enabled            bool   `json:"enabled"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty"`
	ServerName         string `json:"server_name,omitempty"`
}

// RetryPolicy controls per-cluster timeouts and retries.
type RetryPolicy struct {
	// Overall request timeout (ms) across all tries. 0 = disabled (use connection/request default).
	RequestTimeoutMs int `json:"request_timeout_ms,omitempty"`
	// Per-try timeout (ms). 0 = disabled (no extra per-try timeout beyond RequestTimeoutMs).
	PerTryTimeoutMs int `json:"per_try_timeout_ms,omitempty"`
	// Max number of retries after the first attempt.
	MaxRetries int `json:"max_retries,omitempty"`
	// Only retry idempotent methods (GET/HEAD/OPTIONS). Default true.
	IdempotentOnly bool `json:"idempotent_only,omitempty"`
	// Exponential backoff base/max (ms). If zero, uses small defaults.
	BackoffBaseMs int `json:"backoff_base_ms,omitempty"`
	BackoffMaxMs  int `json:"backoff_max_ms,omitempty"`
}

// OutlierSpec configures passive health-based ejection.
type OutlierSpec struct {
	ConsecutiveFailures int `json:"consecutive_failures"` // default 3
	EjectionSeconds     int `json:"ejection_seconds"`     // default 30
}

// HealthCheckSpec configures active health checking for endpoints in a cluster.
type HealthCheckSpec struct {
	Type               string `json:"type"`                // "tcp" or "http"; default "tcp"
	IntervalMillis     int    `json:"interval_ms"`         // default 2000
	TimeoutMillis      int    `json:"timeout_ms"`          // default 1000
	HTTPPath           string `json:"http_path,omitempty"` // for HTTP checks; default "/"
	HealthyThreshold   int    `json:"healthy_threshold"`   // consecutive successes to mark healthy; default 1
	UnhealthyThreshold int    `json:"unhealthy_threshold"` // consecutive failures to mark unhealthy; default 3
}

// CircuitBreakerSpec defines simple limits to protect backends.
type CircuitBreakerSpec struct {
	// MaxRequests limits concurrent in-flight requests per cluster. 0 = unlimited.
	MaxRequests int `json:"max_requests"`
	// MaxPending limits the number of requests allowed to wait for capacity
	// when the cluster is at MaxRequests. 0 = fast-fail only.
	MaxPending int `json:"max_pending,omitempty"`
	// PerEndpointMaxRequests limits concurrent in-flight requests per single
	// endpoint. 0 = unlimited.
	PerEndpointMaxRequests int `json:"per_endpoint_max_requests,omitempty"`
}

// RateLimitSpec defines a simple local rate limit.
type RateLimitSpec struct {
	// RequestsPerSecond is the steady allowed rate.
	RequestsPerSecond int `json:"rps"`
	// Burst is extra tokens allowed for short bursts.
	Burst int `json:"burst"`
	// Scope: "global" (one bucket) or "ip" (per-client IP). Default: global.
	Scope string `json:"scope,omitempty"`
}
