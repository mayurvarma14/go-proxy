package filter

import (
	"fmt"
	"net/http"

	"github.com/mayurvarma14/go-proxy/internal/cluster"
	"github.com/mayurvarma14/go-proxy/internal/config"
	"github.com/mayurvarma14/go-proxy/internal/connctx"
	"github.com/mayurvarma14/go-proxy/internal/httpcm"
)

// NetworkFilter processes a downstream connection.
type NetworkFilter interface{ Handle(*connctx.ConnCtx) error }

// Chain runs filters in order.
type Chain struct{ filters []NetworkFilter }

func NewChain(filters []NetworkFilter) Chain {
	fs := make([]NetworkFilter, len(filters))
	copy(fs, filters)
	return Chain{filters: fs}
}

func (c Chain) Execute(ctx *connctx.ConnCtx) error {
	for _, f := range c.filters {
		if err := f.Handle(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Registry maps names to filters.
type Registry interface {
	Resolve(name string) (NetworkFilter, bool)
	H2Handler() http.Handler
}

type simpleRegistry struct {
	m  map[string]NetworkFilter
	h2 http.Handler
}

func NewRegistry(cfg config.ProxyConfig, cm *cluster.Manager) Registry {
	r := &simpleRegistry{m: map[string]NetworkFilter{}}
	r.m["logging"] = LoggingFilter{}
	// Build HCM with HTTP filters: logging + forwarded + (optional rate-limit) + router + route options + upstream proxy.
	var httpFilters []httpcm.HTTPFilter
	httpFilters = append(httpFilters, httpcm.LoggingFilter{})
	// Add forwarded headers + request ID filter early so all requests get an ID
	httpFilters = append(httpFilters, httpcm.NewForwardedHeaders("go-proxy"))
	if rl := httpcm.NewRateLimiter(cfg.RateLimit); rl != nil {
		httpFilters = append(httpFilters, rl)
	}
	httpFilters = append(httpFilters, httpcm.NewRouter(cfg.Routes))
	httpFilters = append(httpFilters, httpcm.NewRouteOptionsFilter(cfg.Routes))
	httpFilters = append(httpFilters, httpcm.NewUpstreamProxy(cm, cfg.Clusters))
	h := httpcm.NewWithFilters(httpFilters)
	r.m["hcm"] = h
	// HTTP/2 handler reuses the same HTTP filter chain
	r.h2 = httpcm.NewH2Handler(cm, cfg)
	return r
}

func (r *simpleRegistry) Resolve(name string) (NetworkFilter, bool) { f, ok := r.m[name]; return f, ok }
func (r *simpleRegistry) H2Handler() http.Handler                   { return r.h2 }

// LoggingFilter: prints a one-line accept log.
type LoggingFilter struct{}

func (LoggingFilter) Handle(ctx *connctx.ConnCtx) error {
	fmt.Printf("accept remote=%s start=%s\n", ctx.Remote, ctx.Start.Format("15:04:05.000"))
	return nil
}
