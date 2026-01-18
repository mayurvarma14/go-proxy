package admin

import (
    "context"
    "encoding/json"
    "net/http"
    "time"

    "github.com/mayurvarma14/go-proxy/internal/config"
    "github.com/mayurvarma14/go-proxy/internal/obs/metrics"
)

// Start runs a small admin HTTP server exposing /stats, /config, /healthz, /reload,
// and operational endpoints like /drain_start and /quit.
// - cfgProvider returns the current config snapshot
// - reload triggers a hot reload
// - drainStart stops accepting new downstream connections
// - quit cancels the top-level context to exit the process gracefully
// endpointsProvider returns a runtime debug snapshot of clusters/endpoints.
// It's optional; when nil, /endpoints will return 501.
func Start(ctx context.Context, addr string,
    cfgProvider func() config.ProxyConfig,
    reload func() error,
    drainStart func() error,
    quit func(),
    endpointsProvider func() interface{},
) error {
    mux := http.NewServeMux()

    mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusOK)
        _, _ = w.Write([]byte("ok\n"))
    })
    mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "text/plain; charset=utf-8")
        _, _ = w.Write([]byte(metrics.RenderText()))
    })
    mux.HandleFunc("/config", func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        enc := json.NewEncoder(w)
        enc.SetIndent("", "  ")
        cfg := cfgProvider()
        _ = enc.Encode(cfg)
    })
    mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
        // Prometheus text exposition format 0.0.4
        w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
        _, _ = w.Write([]byte(metrics.RenderProm()))
    })
    mux.HandleFunc("/routes", func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        cfg := cfgProvider()
        type routeView struct{
            Prefix string `json:"prefix"`
            Cluster string `json:"cluster"`
            StripPrefix bool `json:"strip_prefix,omitempty"`
            PrefixRewrite string `json:"prefix_rewrite,omitempty"`
        }
        routes := make([]routeView, 0, len(cfg.Routes))
        for _, rr := range cfg.Routes {
            routes = append(routes, routeView{Prefix: rr.Prefix, Cluster: rr.Cluster, StripPrefix: rr.StripPrefix, PrefixRewrite: rr.PrefixRewrite})
        }
        enc := json.NewEncoder(w); enc.SetIndent("", "  "); _ = enc.Encode(routes)
    })
    mux.HandleFunc("/endpoints", func(w http.ResponseWriter, r *http.Request) {
        if endpointsProvider == nil {
            http.Error(w, "endpoints debug not available", http.StatusNotImplemented)
            return
        }
        w.Header().Set("Content-Type", "application/json")
        enc := json.NewEncoder(w); enc.SetIndent("", "  ")
        _ = enc.Encode(endpointsProvider())
    })
    mux.HandleFunc("/reload", func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
            w.WriteHeader(http.StatusMethodNotAllowed)
            _, _ = w.Write([]byte("use POST to reload\n"))
            return
        }
        if err := reload(); err != nil {
            http.Error(w, "reload failed: "+err.Error(), http.StatusInternalServerError)
            return
        }
        w.WriteHeader(http.StatusOK)
        _, _ = w.Write([]byte("reloaded\n"))
    })
    mux.HandleFunc("/drain_start", func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost && r.Method != http.MethodGet {
            w.WriteHeader(http.StatusMethodNotAllowed)
            return
        }
        if drainStart != nil {
            _ = drainStart()
        }
        w.WriteHeader(http.StatusOK)
        _, _ = w.Write([]byte("drain started\n"))
    })
    // Single /quitquitquit endpoint to gracefully stop (Envoy-style)
    quitHandler := func(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost && r.Method != http.MethodGet {
            w.WriteHeader(http.StatusMethodNotAllowed)
            return
        }
        w.WriteHeader(http.StatusOK)
        _, _ = w.Write([]byte("shutting down\n"))
        if quit != nil {
            go quit()
        }
    }
    mux.HandleFunc("/quitquitquit", quitHandler)

    srv := &http.Server{Addr: addr, Handler: mux}
    go func() {
        <-ctx.Done()
        shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
        defer cancel()
        _ = srv.Shutdown(shutdownCtx)
    }()

    // Run server; http.ErrServerClosed is normal on shutdown.
    if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
        return err
    }
    return nil
}
