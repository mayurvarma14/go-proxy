package httpcm

import (
	"strings"

	"github.com/mayurvarma14/go-proxy/internal/config"
)

// Router matches request paths to clusters using simple prefix rules.
type Router struct{ routes []config.RouteRule }

func NewRouter(routes []config.RouteRule) Router { return Router{routes: routes} }

func (r Router) OnRequest(rc *RequestCtx) error {
	path := rc.Req.URL.Path
	for i, rr := range r.routes {
		if rr.Prefix == "/" && rr.Cluster != "" {
			// catch-all route if no better match
			if rc.UpstreamCluster == "" {
				rc.UpstreamCluster = rr.Cluster
			}
		}
		if strings.HasPrefix(path, rr.Prefix) {
			rc.UpstreamCluster = rr.Cluster
			rc.RouteID = i
			// Preserve per-route options to be applied later by RouteOptionsFilter
			// Compute rewritten path if configured
			remainder := strings.TrimPrefix(path, rr.Prefix)
			// Ensure remainder begins with "/" if non-empty
			if remainder == "" {
				remainder = "/"
			} else if !strings.HasPrefix(remainder, "/") {
				remainder = "/" + remainder
			}
			if rr.PrefixRewrite != "" {
				// Replace matched prefix with configured string
				// Normalize join of PrefixRewrite + remainder
				base := rr.PrefixRewrite
				if base == "" {
					base = "/"
				}
				if !strings.HasPrefix(base, "/") {
					base = "/" + base
				}
				// Avoid double slash
				if strings.HasSuffix(base, "/") && strings.HasPrefix(remainder, "/") {
					rc.RewritePath = base[:len(base)-1] + remainder
				} else {
					rc.RewritePath = base + remainder
				}
			} else if rr.StripPrefix {
				// Remove matched prefix entirely
				rc.RewritePath = remainder
			}
			break
		}
	}
	return nil
}
