package httpcm

import (
	"github.com/mayurvarma14/go-proxy/internal/config"
	"strings"
	"time"
)

// RouteOptionsFilter applies per-route header rewrites and retry overrides.
type RouteOptionsFilter struct {
	// preprocessed retry overrides indexed by route index
	retry map[int]retryCfg
	// header rules by route index
	hdrSet      map[int]map[string]string
	hdrDrop     map[int]map[string]struct{}
	hostRewrite map[int]string
}

func NewRouteOptionsFilter(routes []config.RouteRule) RouteOptionsFilter {
	f := RouteOptionsFilter{
		retry:       make(map[int]retryCfg),
		hdrSet:      make(map[int]map[string]string),
		hdrDrop:     make(map[int]map[string]struct{}),
		hostRewrite: make(map[int]string),
	}
	for i, rr := range routes {
		if rr.RetryPolicy != nil {
			rc := retryCfg{
				requestTimeout: 0,
				perTryTimeout:  0,
				maxRetries:     0,
				idempotentOnly: true,
				backoffBase:    0,
				backoffMax:     0,
			}
			if rr.RetryPolicy.RequestTimeoutMs > 0 {
				rc.requestTimeout = toDurMs(rr.RetryPolicy.RequestTimeoutMs)
			}
			if rr.RetryPolicy.PerTryTimeoutMs > 0 {
				rc.perTryTimeout = toDurMs(rr.RetryPolicy.PerTryTimeoutMs)
			}
			if rr.RetryPolicy.MaxRetries > 0 {
				rc.maxRetries = rr.RetryPolicy.MaxRetries
			}
			if rr.RetryPolicy.BackoffBaseMs > 0 {
				rc.backoffBase = toDurMs(rr.RetryPolicy.BackoffBaseMs)
			}
			if rr.RetryPolicy.BackoffMaxMs > 0 {
				rc.backoffMax = toDurMs(rr.RetryPolicy.BackoffMaxMs)
			}
			f.retry[i] = rc
		}
		if rr.HostRewrite != "" {
			f.hostRewrite[i] = rr.HostRewrite
		}
		if len(rr.SetHeaders) > 0 {
			f.hdrSet[i] = rr.SetHeaders
		}
		if len(rr.RemoveHeaders) > 0 {
			m := map[string]struct{}{}
			for _, k := range rr.RemoveHeaders {
				m[strings.ToLower(k)] = struct{}{}
			}
			f.hdrDrop[i] = m
		}
	}
	return f
}

func (f RouteOptionsFilter) OnRequest(rc *RequestCtx) error {
	if rc.RouteID < 0 {
		return nil
	}
	if host, ok := f.hostRewrite[rc.RouteID]; ok && host != "" {
		rc.Req.Host = host
	}
	if set := f.hdrSet[rc.RouteID]; len(set) > 0 {
		for k, v := range set {
			rc.Req.Header.Set(k, v)
		}
	}
	if drop := f.hdrDrop[rc.RouteID]; len(drop) > 0 {
		for k := range rc.Req.Header {
			if _, ok := drop[strings.ToLower(k)]; ok {
				rc.Req.Header.Del(k)
			}
		}
	}
	if ro, ok := f.retry[rc.RouteID]; ok {
		// Install override; upstream proxy will merge this with cluster policy
		rc.RetryOverride = &ro
	}
	return nil
}

func toDurMs(ms int) (d time.Duration) { return time.Duration(ms) * time.Millisecond }
