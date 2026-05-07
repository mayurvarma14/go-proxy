# go-proxy

A small reverse proxy and L7 load balancer written in Go. I built it to work
through the data-plane patterns I kept seeing in Envoy and NGINX —
listener/filter chains, clusters, active and passive health checking, circuit
breaking, retries — and to have something I could actually run, break, and
poke at in a debugger.

It is not a drop-in replacement for anything. If you want a real proxy in
production, use Envoy. If you want to read ~3.5k lines of Go that implements
the same shape end to end, with diagrams, this might help.

## Contents

- [What is a reverse proxy?](#what-is-a-reverse-proxy)
- [Quick start](#quick-start)
- [Architecture at a glance](#architecture-at-a-glance)
- [Request lifecycle](#request-lifecycle)
- [Load balancing](#load-balancing)
- [Health checking](#health-checking)
- [Circuit breaking](#circuit-breaking)
- [Retries and timeouts](#retries-and-timeouts)
- [Rate limiting](#rate-limiting)
- [TLS, SNI, and mTLS](#tls-sni-and-mtls)
- [Service discovery](#service-discovery)
- [Configuration](#configuration)
- [Operations and observability](#operations-and-observability)
- [What it doesn't do](#what-it-doesnt-do)
- [Design notes](#design-notes)
- [Layout](#layout)
- [Testing it locally](#testing-it-locally)

---

## What is a reverse proxy?

A normal "forward" proxy sits in front of clients and forwards traffic out to
the internet on their behalf. A *reverse* proxy sits in front of your
servers and forwards traffic in to them. Clients talk to the proxy as if it
were the real service. The proxy decides which backend should actually
handle each request.

```
                 ┌─────────────┐         ┌─────────────┐
   client  ────► │  reverse    │ ──────► │  backend A  │
   client  ────► │   proxy     │ ──────► │  backend B  │
   client  ────► │             │ ──────► │  backend C  │
                 └─────────────┘         └─────────────┘
                       :8080
                  one address               many servers
```

What that buys you:

- **Load balancing** — spread requests across many backends, skip the dead
  ones.
- **TLS termination** — clients talk HTTPS to the proxy; the proxy talks
  HTTP (or its own HTTPS) to the backend. Your backends don't each need a
  cert.
- **Resilience** — health checks, circuit breakers, retries, timeouts, all
  in one place.
- **Routing** — `/api/v1/*` to one cluster, `/static/*` to another, on the
  same port.
- **Observability** — one chokepoint to instrument, one place to scrape
  metrics from.

`go-proxy` does all of those.

---

## Quick start

You'll need Go 1.24+ and `openssl` (for the dev TLS cert).

```sh
git clone https://github.com/mayurvarma14/go-proxy.git
cd go-proxy
make certs           # generates self-signed server.crt / server.key for localhost
make run             # builds and runs with config/dev.yaml
```

The proxy now listens on:

| Port | Purpose                                                    |
|------|------------------------------------------------------------|
| 8080 | HTTP                                                       |
| 8443 | HTTPS (SNI-aware filter chains)                            |
| 9901 | Admin (`/healthz`, `/metrics`, `/config`, `/reload`, ...)  |

There's a tiny echo server in `cmd/echo` for end-to-end testing. In three
separate terminals:

```sh
go run ./cmd/echo --addr 127.0.0.1:9001 &
go run ./cmd/echo --addr 127.0.0.1:9002 &
go run ./cmd/echo --addr 127.0.0.1:9003 &

curl localhost:8080/svc1/   # routed to the service1 cluster (9001 / 9002)
curl localhost:8080/        # routed to the default cluster (9003)
curl localhost:9901/healthz
```

Docker:

```sh
make docker
docker run --rm -p 8080:8080 -p 9901:9901 go-proxy:latest
```

---

## Architecture at a glance

`go-proxy` borrows Envoy's vocabulary, scaled down. There are four moving
parts:

![Architecture overview](./images/architecture.png)

| Piece                | Job                                                                                   |
|----------------------|---------------------------------------------------------------------------------------|
| **Listener**         | Owns a TCP port. Accepts conns, does TLS termination, picks a filter chain.           |
| **Filter chain**     | Ordered list of filters that run on the connection / request.                         |
| **HCM**              | "HTTP Connection Manager" — parses HTTP, runs HTTP filters, drives the upstream call. |
| **Cluster**          | A pool of upstream endpoints with its own health, LB policy, breakers, retry rules.   |
| **Admin (`:9901`)**  | Out-of-band HTTP for status, metrics, config dump, hot reload, drain.                 |

Or in ASCII:

```
                                ┌────────────────────────────┐
                                │      Admin (:9901)         │
                                │  /healthz /metrics /config │
                                │  /reload  /drain_start     │
                                │  /endpoints /quitquitquit  │
                                └─────────────▲──────────────┘
                                              │
   downstream                                 │
   ──────────►  Listener  ──►  Filter chain ──┴──►  HCM  ──►  Router  ──►  Upstream
                (TCP+TLS)      (network)            (HTTP)    (prefix)    (cluster +
                SNI-routed     logging,             access     match,      retries +
                               forwarded,           log,       rewrite,    CB +
                               rate-limit,          h1/h2      override    LB pick)
                               ...                                            │
                                                                              ▼
                                                                  ┌────────────────────┐
                                                                  │       Cluster      │
                                                                  │                    │
                                                                  │  endpoints[]       │
                                                                  │  + active HC       │
                                                                  │  + outlier (passive)│
                                                                  │  + circuit breaker │
                                                                  │  + DNS discovery   │
                                                                  └────────────────────┘
```

The pre-rendered diagrams in `images/` cover the same flow plus per-feature
detail (load balancing, the breaker state machine, the TLS handshake, the
DNS resolution loop, and so on).

---

## Request lifecycle

![Request flow](./images/request_flow.png)

What happens when a request hits port 8080:

1. **Accept.** Listener accepts a TCP connection.
2. **TLS (if HTTPS).** Listener finishes the TLS handshake. The filter
   chain selected for this connection is the first one whose `sni_hosts`
   matches the client's SNI; if none match, it falls back to the first chain
   without `sni_hosts`.
3. **Network filters.** Run in order on the connection. The terminal filter
   is the HCM.
4. **HTTP parse.** HCM reads the HTTP request (HTTP/1.1; h2 hooks exist but
   the filter chain is best exercised on h1 today).
5. **HTTP filters.** `forwarded_headers` → `rate_limit` → `router` →
   `upstream`. The router picks a cluster by longest-matching path prefix
   and computes the rewrite (strip prefix or replace prefix).
6. **Upstream.** Up to `1 + max_retries` attempts: try to acquire the
   cluster's circuit breaker slot, pick an endpoint, forward, watch the
   per-try timeout. On error, mark the endpoint, back off, retry the next
   endpoint.
7. **Stream the response back.** Headers and body, with hop-by-hop headers
   stripped.

---

## Load balancing

When a request lands on a cluster, *which* of its endpoints handles it?

![Load balancing](./images/load_balancing.png)

`go-proxy` ships three policies, picked per-cluster via `lb_policy`:

| Policy           | How it picks                                                  | Good for                              |
|------------------|---------------------------------------------------------------|---------------------------------------|
| `round_robin`    | `(idx + 1) % n`, skipping unhealthy / over-capacity endpoints | Default; uniform backends             |
| `random`         | Pick a random endpoint from healthy + under-cap               | Large pools, when fairness can be coarse |
| `least_requests` | Endpoint with the fewest in-flight requests                   | Variable response times               |

All three:

- skip endpoints flagged unhealthy by the active health checker,
- skip endpoints currently ejected by outlier detection,
- skip endpoints already at `per_endpoint_max_requests`.

If *every* endpoint is over the per-endpoint cap, the proxy returns a
sentinel error so the upstream retry loop can wait for capacity instead of
just failing the request.

```
round_robin:  req1→A  req2→B  req3→C  req4→A  req5→B  ...

random:       10 picks → A=3 B=4 C=3   (some variance)

least_requests:    A: 2 in-flight
                   B: 5 in-flight   ← skip
                   C: 1 in-flight   ← winner
```

---

## Health checking

A backend can be alive but failing — it should be pulled out of rotation
quickly, and put back in once it recovers. There are two complementary
mechanisms.

![Health checking](./images/health_checking.png)

### Active checks

The proxy probes each endpoint on an interval. HTTP or TCP.

```yaml
health_check:
  type: http              # or "tcp"
  http_path: /healthz
  interval_ms: 2000       # probe every 2s
  timeout_ms: 800         # fail the probe after 800ms
  healthy_threshold: 1    # 1 success after a failure → mark healthy
  unhealthy_threshold: 2  # 2 failures in a row → mark unhealthy
```

For HTTP, status `< 500` counts as healthy. For TCP, a successful connect
counts.

### Passive checks (outlier detection)

Active checks talk to `/healthz`. They tell you nothing about whether real
traffic is succeeding. Passive checks fix that: count consecutive failures
on real requests, eject the endpoint when the threshold is hit, recover
after a cool-down.

```yaml
outlier:
  consecutive_failures: 3   # 3 real failures in a row → eject
  ejection_seconds: 30      # ejected for 30s, then re-eligible
```

A request to an ejected endpoint is never attempted; it's skipped during LB
selection.

---

## Circuit breaking

![Circuit breaker](./images/circuit_breaker.png)

Goal: protect a backend from being overwhelmed. There are two limits per
cluster:

```yaml
circuit_breaker:
  max_requests: 50              # cluster-wide concurrent in-flight
  per_endpoint_max_requests: 10 # per single endpoint
  max_pending: 1000             # bounded wait queue (0 = fast-fail)
```

Behaviour:

- If `inflight < max_requests`, the request goes through.
- If `inflight >= max_requests` and `max_pending == 0`, return `503` immediately.
- If `inflight >= max_requests` and `max_pending > 0`, the request joins a
  bounded queue. It either acquires capacity, or hits the request context
  deadline and we return `504`.
- If every healthy endpoint is at `per_endpoint_max_requests`, the request
  also waits for capacity (or 503s) — without this, you can have spare
  cluster capacity but be wedged on one endpoint.

Quick sizing rule of thumb:

```
max_requests ≈ target_RPS × p99_latency_seconds
e.g. 200 RPS × 0.1s = ~20 concurrent
```

---

## Retries and timeouts

Networks lie. The retry policy lives on the cluster (and can be overridden
per route).

```yaml
retry_policy:
  request_timeout_ms: 3000   # overall budget across all attempts
  per_try_timeout_ms: 1200   # each attempt is bounded
  max_retries: 1             # so up to 2 attempts total
  idempotent_only: true      # POST etc are not retried
  backoff_base_ms: 100
  backoff_max_ms: 500        # exponential, capped, with up to 25% jitter
```

Two important defaults:

- **`idempotent_only: true`.** Retries only happen for `GET`, `HEAD`,
  `OPTIONS`. Retrying a `POST` blindly is how you double-charge a credit
  card.
- **The overall `request_timeout_ms` bounds the whole thing.** A retry
  won't start past that deadline; backoffs that would push past it are cut
  short.

```
attempt 1  ───►  endpoint A  ── error
                                  │
                              ◄── backoff (≈100ms + jitter)
                                  │
attempt 2  ───►  endpoint B  ── 200 OK
```

---

## Rate limiting

A token bucket. Refills at `rps` tokens per second up to a max of `burst`.
Each accepted request takes one token. No tokens → `429 Too Many Requests`.

![Rate limiting](./images/rate_limiting.png)

```yaml
rate_limit:
  rps: 1000      # steady-state allowed rate
  burst: 1000    # max tokens; absorbs short spikes
  scope: global  # one bucket for everyone, or "ip" for one bucket per client IP
```

Worked example with `rps=5, burst=10`:

| Time   | Bucket before | Arrivals | Allowed | Denied |
|--------|---------------|----------|---------|--------|
| 0.00s  | 10            | 12       | 10      | 2      |
| 0.20s  | 1             | 1        | 1       | 0      |
| 1.00s  | 5             | 4        | 4       | 0      |
| 2.00s  | 6             | 9        | 6       | 3      |

`scope: ip` is the more useful mode in practice — one noisy client can't
starve everyone else.

---

## TLS, SNI, and mTLS

![TLS handshake](./images/tls_handshake.png)

On a TLS listener you can serve more than one cert by setting `sni_hosts`
on the cert entries. The handshake's `ServerName` value picks the right
cert *and* the right filter chain:

```yaml
listeners:
  - name: https_main
    address: 0.0.0.0:8443
    tls:
      cert_path: ./server.crt
      key_path: ./server.key
      min_version: "1.2"          # "1.3" supported
      # client_ca_path / require_client_cert for mTLS
    filter_chains:
      - name: default              # no sni_hosts → fallback
        filters: [hcm]
      - name: app
        sni_hosts: [app.example.com]
        filters: [logging, hcm]
```

For mutual TLS, set `client_ca_path` and `require_client_cert: true`. The
proxy will then require the client to present a cert chained to that CA.

Wildcard SNI (`*.example.com`) is **not** implemented — exact host match
only. (If I added one thing here, it would be that.)

---

## Service discovery

![Service discovery](./images/service_discovery.png)

When a cluster's endpoint is a hostname (not an IP literal), `discovery:
strict_dns` resolves it on an interval and updates the endpoint set
atomically. Static IP-literal seeds in the same cluster are kept as-is —
the effective endpoint set is the union.

```yaml
clusters:
  - name: dns_svc
    endpoints: [api.internal:9000]
    discovery: strict_dns
    dns_refresh_seconds: 5
```

Each refresh cycle:

```
A api.internal  →  [10.0.1.5, 10.0.1.8, 10.0.2.3]
        ▼
endpoints set  =  {10.0.1.5:9000, 10.0.1.8:9000, 10.0.2.3:9000}
                  ∪ any IP-literal seeds
        ▼
diff vs current set
        ▼
start health checks for new addrs, stop them for removed addrs,
swap the endpoint slice under the cluster lock
```

Failed lookups are counted (`dns_resolve_errors_total`) and don't tear down
the existing endpoint set.

---

## Configuration

YAML and JSON are both accepted; the loader picks based on file extension.
The schema is in `internal/config/types.go` — that is the source of truth.
A reasonably complete example:

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
  scope: global

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
      max_pending: 1000
      per_endpoint_max_requests: 100
    retry_policy:
      request_timeout_ms: 3000
      per_try_timeout_ms: 1200
      max_retries: 1
      idempotent_only: true
      backoff_base_ms: 100
      backoff_max_ms: 500
```

---

## Operations and observability

The admin server is intentionally tiny and unauthenticated — bind it to
`127.0.0.1` (default) or to a private interface.

| Endpoint        | Method  | Purpose                                |
|-----------------|---------|----------------------------------------|
| `/healthz`      | GET     | Liveness                               |
| `/metrics`      | GET     | Prometheus exposition                  |
| `/stats`        | GET     | Same numbers, plain text               |
| `/config`       | GET     | Effective config snapshot (JSON)       |
| `/routes`       | GET     | Current route rules                    |
| `/endpoints`    | GET     | Per-endpoint health + in-flight        |
| `/reload`       | POST    | Re-read config file, restart listeners |
| `/drain_start`  | POST    | Stop accepting new downstream conns    |
| `/quitquitquit` | POST    | Graceful shutdown                      |

A few of the more useful metrics (full list in `internal/obs/metrics`):

```
downstream_http_requests_total
http_responses_2xx_total / 4xx_total / 5xx_total
upstream_attempts_total
upstream_failures_total
upstream_outcome_total{cluster,outcome=success|error|timeout}
upstream_latency_ms{cluster}                # histogram
cluster_inflight_requests{cluster}
endpoint_inflight_requests{cluster,endpoint}
endpoints_ejected_total
endpoints_added_total / endpoints_removed_total
circuit_breaker_open_total / circuit_breaker_trips_total
pending_dropped_total
rate_limit_dropped_total
dns_resolve_total / dns_resolve_errors_total
```

---

## What it doesn't do

I think it's worth being honest about the edges I hit and chose not to cross.
For an interview project, the *what's missing and why* is at least as
interesting as the what works.

- **HTTP/2 downstream is partial.** There's an h2 handler but it doesn't
  get the full filter pipeline; production-quality h2 needs a real
  connection manager that owns stream lifecycle. HTTP/2 *upstream* is
  enabled when a cluster uses TLS and ALPN negotiates h2.
- **No gRPC-aware routing.** Headers and methods only, no trailers.
- **Wildcard SNI is not supported.** Exact host match only.
- **No xDS, no control plane.** Static YAML/JSON with a manual `/reload`.
  A file-watcher would be a few lines; an xDS client would not.
- **Discovery is DNS only.** No EDS, no Consul, no etcd.
- **No body transforms** beyond the path / header rewrites already listed.
- **Metrics are an in-process counter map.** Fine for portfolio and local
  use; for real workloads you'd swap in `prometheus/client_golang` with
  proper labeled metrics.

---

## Design notes

A few choices that aren't obvious from the code:

- **Endpoints are addressed by string.** No `Endpoint` interface, no
  per-endpoint subobject. State is keyed by `host:port` strings under one
  `sync.RWMutex` per cluster. The hot path uses atomics for in-flight
  counters so the lock is only taken when state actually changes.
- **One goroutine per endpoint for active health checks.** Adding/removing
  endpoints starts/stops a goroutine. Fine for O(10) endpoints per cluster;
  if you scaled to thousands you'd batch checks.
- **Strict-DNS resolves the union of all seed hostnames** plus IP-literal
  seeds. Atomic swap of the endpoint slice avoids needing a CoW data
  structure.
- **`MaxRequests` is enforced by an atomic add+rollback**, not a semaphore.
  Slightly faster, slightly looser (you can transiently exceed the cap by
  one when the limit is hit). For a learning project, fine; for production
  I'd use a semaphore and benchmark.
- **Retries are bounded by request context.** A retry will not start past
  `request_timeout_ms`, and `per_try_timeout_ms` bounds each attempt.
  Backoff is exponential with up to 25% jitter.
- **TLS 1.2 is the default minimum.** 1.3 with `min_version: "1.3"`. Cipher
  suites are Go's defaults — I deliberately did not customise them.
- **The HCM is HTTP/1.1-first.** There's an h2 path; the filter chain is
  best exercised on h1 right now.

---

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

---

## Testing it locally

```sh
make test                       # race + count=1
make cover                      # coverage summary
make bench                      # benchmarks (currently sparse)
go test ./internal/cluster -run TestOutlier -v
```

A handful of useful one-liners while the proxy is running:

```sh
# trip the cluster CB (default cluster has max_requests: 50)
seq 200 | xargs -P 80 -n1 -I_ curl -s -o /dev/null -w "%{http_code}\n" \
  http://localhost:8080/ | sort | uniq -c

# rate limiter (default rps=1000, burst=1000 — bump load to see 429s)
hey -n 5000 -c 200 http://localhost:8080/ 2>/dev/null | grep -A1 "Status code"

# eject + recover an endpoint by killing a backend, then restarting it
curl -s localhost:9901/endpoints | jq

# hot reload after editing config/dev.yaml
curl -X POST localhost:9901/reload
```

---

## License

MIT — see [LICENSE](./LICENSE).
