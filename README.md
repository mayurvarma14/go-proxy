# go-proxy

A small reverse proxy / L7 load balancer written in Go. Built to work through
the data-plane patterns I kept seeing in Envoy and NGINX — listener/filter
chains, cluster-based upstream management, active + passive health checking,
circuit breaking, retry budgets — and to have something I could actually run
locally and poke at.

This is a learning project, not a drop-in replacement for anything. If you
need a real proxy, use Envoy. If you want to read ~3.5k lines of Go that
implements the same shape end-to-end, this might be useful.

## Contents

- [Quick start](#quick-start)
- [What it does](#what-it-does)
- [What it doesn't do](#what-it-doesnt-do)
- [Architecture](#architecture)
- [Configuration](#configuration)
- [Operations](#operations)
- [Design notes](#design-notes)
- [Layout](#layout)
- [Testing it locally](#testing-it-locally)

## Quick start

Requires Go 1.24+ and `openssl` for the dev TLS cert.

```sh
git clone https://github.com/mayurvarma14/go-proxy.git
cd go-proxy
make certs           # generates self-signed server.crt / server.key for localhost
make run             # builds and runs with config/dev.yaml
```

Defaults from `config/dev.yaml`:

| Port | What |
|------|------|
| 8080 | HTTP listener |
| 8443 | HTTPS listener (SNI-aware filter chains) |
| 9901 | Admin server (`/healthz`, `/metrics`, `/config`, `/reload`, ...) |

A small echo server is included for end-to-end testing:

```sh
go run ./cmd/echo --addr 127.0.0.1:9001 &
go run ./cmd/echo --addr 127.0.0.1:9002 &
go run ./cmd/echo --addr 127.0.0.1:9003 &
curl localhost:8080/svc1/   # round-trips through the proxy to 9001/9002
```

Docker:

```sh
make docker
docker run --rm -p 8080:8080 -p 9901:9901 go-proxy:latest
```

## What it does

- Multi-listener HTTP/1.1 and HTTPS with TLS termination, SNI-based filter
  chain selection, optional mTLS.
- Path-prefix routing with prefix strip / rewrite, host rewrite, header
  add/remove, per-route retry overrides.
- Three load balancing policies: round robin (default), random, least
  requests. All skip endpoints marked unhealthy by either active checks or
  outlier detection.
- Active health checks (HTTP or TCP) with configurable thresholds, run as
  per-endpoint goroutines.
- Passive outlier detection: N consecutive failures ⇒ eject for T seconds.
- Two circuit breakers per cluster: cluster-wide concurrent in-flight cap and
  per-endpoint concurrent cap. Optional bounded pending queue with 504 when
  the queue or request context times out.
- Retry policy with exponential backoff + jitter, idempotent-only by default,
  per-try and overall request timeouts.
- Token-bucket rate limit, global or per-client-IP.
- Strict DNS service discovery: hostnames in a cluster's endpoint list are
  re-resolved on an interval and the endpoint set is swapped atomically.
- Hot reload via `POST /reload` — config is re-parsed and listeners that
  changed are restarted; clusters are rebuilt without dropping in-flight
  requests on unchanged clusters.
- Prometheus exposition at `/metrics` plus a human-readable text dump at
  `/stats`.
- Graceful shutdown / drain: `POST /drain_start` stops accepting new
  connections, `/quitquitquit` cancels the root context.

## What it doesn't do

Worth being honest about — these are the edges I hit and chose not to cross:

- **HTTP/2 downstream is partial.** There's an h2 handler but it doesn't get
  the full filter pipeline; production-quality h2 would mean a real connection
  manager that owns the stream lifecycle. HTTP/2 *upstream* is enabled when a
  cluster uses TLS and ALPN negotiates h2.
- **No gRPC-aware routing.** Headers/methods only.
- **Wildcard SNI is not supported.** Exact match only.
- **No xDS, no control plane.** Static YAML/JSON, with a manual `/reload`
  hook. Adding something like a file watcher is a few lines; an actual xDS
  client is not.
- **Discovery is DNS only.** No EDS, no Consul/etcd integration.
- **No request/response body transforms** beyond the headers and path
  rewrites listed above.
- **Metrics are an in-process counter map.** Good enough for portfolio /
  local use; for real workloads you'd swap in `prometheus/client_golang`
  with proper labeled metrics.

## Architecture

Roughly mirrors Envoy's vocabulary but at a much smaller scale.

```
                                 ┌────────────────────────────┐
                                 │     Admin (:9901)          │
                                 │  /healthz /metrics /config │
                                 │  /reload  /drain_start     │
                                 │  /endpoints /quitquitquit  │
                                 └─────────────▲──────────────┘
                                               │
   downstream                                  │
   ───────────►  Listener  ──►  Filter chain ──┴──►  HCM  ──►  Router  ──►  Upstream
                 (TCP+TLS)      (network)            (HTTP)    (prefix)     (cluster
                 SNI-routed     logging,             access     match,       client +
                                forwarded,           log,       rewrite,     retry +
                                rate-limit,          h1/h2      override     CB +
                                ...                                          LB pick)
                                                                                │
                                                                                ▼
                                                                       ┌──────────────┐
                                                                       │   Cluster    │
                                                                       │              │
                                                                       │  endpoints   │
                                                                       │  + active HC │
                                                                       │  + outlier   │
                                                                       │  + CB        │
                                                                       │  + DNS disco │
                                                                       └──────────────┘
```

The pre-rendered diagrams in `images/` cover the same flow plus details for
load balancing, circuit breaker states, the TLS handshake, and DNS resolution
loops.

### Request lifecycle

1. Listener accepts a TCP conn. For HTTPS, completes the TLS handshake; the
   selected filter chain is the one whose `sni_hosts` matches the client's
   SNI (or the first chain with no `sni_hosts` as the default).
2. Network filters run in order on the connection. The terminal filter is
   the HCM (HTTP connection manager).
3. The HCM parses HTTP requests (h1 today; h2 hooks are wired but not the
   full filter chain) and runs HTTP filters: forwarded headers → rate limit
   → router → upstream proxy.
4. Router picks a cluster by longest-matching prefix and computes the
   rewritten path / route options.
5. Upstream proxy tries up to `1 + max_retries` attempts: acquire cluster CB
   capacity, pick an endpoint via the LB policy (skipping unhealthy and
   per-endpoint-saturated endpoints), forward with a per-try timeout, on
   failure mark the endpoint and back off.
6. Response is streamed back to the downstream connection.

## Configuration

YAML and JSON are both accepted; the loader picks based on file extension.
The schema lives in `internal/config/types.go` — that is the source of
truth. The example below covers the common knobs.

```yaml
listeners:
  - name: http_main
    address: 0.0.0.0:8080
    filter_chains:
      - name: basic
        filters: [logging, hcm]

  - name: https_main
    address: 0.0.0.0:8443
    tls:
      cert_path: ./server.crt
      key_path: ./server.key
      min_version: "1.2"
      # client_ca_path / require_client_cert for mTLS
    filter_chains:
      - name: default
        filters: [hcm]
      - name: app
        sni_hosts: [app.example.com]
        filters: [logging, hcm]

routes:
  - prefix: /svc1
    cluster: service1
    prefix_rewrite: /
  - prefix: /
    cluster: default
    strip_prefix: true

rate_limit:
  rps: 1000
  burst: 1000
  scope: global   # or "ip"

clusters:
  - name: service1
    endpoints: [127.0.0.1:9001, 127.0.0.1:9002]
    lb_policy: least_requests
    health_check:
      type: http
      http_path: /
      interval_ms: 2000
      timeout_ms: 800
      healthy_threshold: 1
      unhealthy_threshold: 2
    outlier:
      consecutive_failures: 3
      ejection_seconds: 30
    circuit_breaker:
      max_requests: 50
      max_pending: 1000              # bounded wait queue; 0 = fast-fail
      per_endpoint_max_requests: 100
    retry_policy:
      request_timeout_ms: 3000
      per_try_timeout_ms: 1200
      max_retries: 1
      idempotent_only: true
      backoff_base_ms: 100
      backoff_max_ms: 500

  - name: dns_svc
    endpoints: [api.internal:9000]   # hostname → resolved every dns_refresh_seconds
    discovery: strict_dns
    dns_refresh_seconds: 5
```

## Operations

The admin server is intentionally tiny:

| Endpoint        | Method  | Purpose                                |
|-----------------|---------|----------------------------------------|
| `/healthz`      | GET     | Liveness                               |
| `/metrics`      | GET     | Prometheus exposition                  |
| `/stats`        | GET     | Same numbers, plain text               |
| `/config`       | GET     | Effective config snapshot              |
| `/routes`       | GET     | Current route rules                    |
| `/endpoints`    | GET     | Per-endpoint health and in-flight      |
| `/reload`       | POST    | Re-read config file, restart listeners |
| `/drain_start`  | POST    | Stop accepting new downstream conns    |
| `/quitquitquit` | POST    | Graceful shutdown                      |

A few selected metrics (full list in `internal/obs/metrics`):

```
downstream_http_requests_total
http_responses_2xx_total / 4xx_total / 5xx_total
upstream_attempts_total
upstream_failures_total
upstream_outcome_total{cluster,outcome=success|error|timeout}
upstream_latency_ms{cluster} (histogram)
cluster_inflight_requests{cluster}
endpoint_inflight_requests{cluster,endpoint}
endpoints_ejected_total
endpoints_added_total / endpoints_removed_total
circuit_breaker_open_total / circuit_breaker_trips_total
pending_dropped_total
rate_limit_dropped_total
dns_resolve_total / dns_resolve_errors_total
```

## Design notes

A few choices that aren't obvious from the code:

- **Endpoints are addressed by string.** No `Endpoint` interface, no
  per-endpoint subobject. State is keyed by `host:port` strings under a
  single `sync.RWMutex` per cluster. Cheap, easy to reason about, and the
  hot path uses atomics for in-flight counters so the lock is only taken
  when state actually changes.
- **Per-endpoint goroutines for health checks.** Adding/removing endpoints
  starts/stops a goroutine. For O(10) endpoints per cluster this is fine; if
  you scaled to thousands you'd batch checks instead.
- **Strict-DNS resolves the union of all seed hostnames** plus any
  IP-literal seeds. Atomic swap of the endpoint slice avoids needing a
  CoW data structure.
- **`MaxRequests` is enforced by an atomic add/rollback**, not a semaphore.
  Slightly faster, slightly looser (you can exceed the cap by 1 transiently
  when the limit is hit). For a learning project, fine; for production I'd
  use a semaphore + benchmarks.
- **Retries are bounded by request context.** A retry will not start past
  the overall `request_timeout_ms`, and `per_try_timeout_ms` bounds each
  attempt. Backoff is exponential with up to 25% jitter.
- **TLS 1.2 is the default minimum.** 1.3 is supported with `min_version:
  "1.3"`. Cipher suites are Go's defaults.
- **The HCM is HTTP/1.1-first.** There is an h2 path, but the filter chain
  is best exercised on h1 right now.

## Layout

```
cmd/
  proxy/         # main binary
  echo/          # tiny upstream for E2E tests
config/          # dev.yaml, dev.json
internal/
  admin/         # admin HTTP server
  cluster/       # endpoints, LB, health, outlier, DNS, CB
  config/        # types + load + validate
  connctx/       # per-conn metadata
  filter/        # network filter interface
  httpcm/        # HTTP connection manager + filters + router + upstream
  listener/      # TCP/TLS listener + chain selection
  obs/
    accesslog/
    metrics/
  runtime/
    supervisor/  # owns lifecycle, drives /reload
  integration/   # smoke tests
images/          # rendered architecture diagrams
```

## Testing it locally

```sh
make test                           # race + count=1
make cover                          # coverage summary
make bench                          # benchmarks (currently sparse)
go test ./internal/cluster -run TestOutlier -v
```

A handful of useful one-liners while a proxy is running locally:

```sh
# trip the cluster CB (default cluster has max_requests: 50)
seq 200 | xargs -P 80 -n1 -I_ curl -s -o /dev/null -w "%{http_code}\n" \
  http://localhost:8080/ | sort | uniq -c

# rate limiter (default rps=1000, burst=1000 — bump load to see 429s)
hey -n 5000 -c 200 http://localhost:8080/ 2>/dev/null | grep -A1 "Status code"

# eject + recover an endpoint by killing a backend, then restarting it
curl -s localhost:9901/endpoints | jq '.[] | {name, endpoints: .endpoints[]?}'

# hot reload after editing config/dev.yaml
curl -X POST localhost:9901/reload
```

## License

MIT — see [LICENSE](./LICENSE).
