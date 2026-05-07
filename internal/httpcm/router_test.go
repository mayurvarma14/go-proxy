package httpcm

import (
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/mayurvarma14/go-proxy/internal/config"
)

type stubAddr struct{ s string }

func (a stubAddr) Network() string { return "tcp" }
func (a stubAddr) String() string  { return a.s }

func newRC(path string) *RequestCtx {
	u := &url.URL{Scheme: "http", Host: "example", Path: path}
	req, _ := http.NewRequest("GET", u.String(), nil)
	return &RequestCtx{Req: req, Remote: net.Addr(stubAddr{"127.0.0.1:1234"}), Start: time.Now()}
}

func TestRouter_PrefixRewrite(t *testing.T) {
	r := NewRouter([]config.RouteRule{{Prefix: "/svc", Cluster: "svc", PrefixRewrite: "/api"}})
	rc := newRC("/svc/foo")
	_ = r.OnRequest(rc)
	if rc.UpstreamCluster != "svc" {
		t.Fatalf("want cluster svc, got %q", rc.UpstreamCluster)
	}
	if rc.RewritePath != "/api/foo" {
		t.Fatalf("want rewrite /api/foo, got %q", rc.RewritePath)
	}
}

func TestRouter_StripPrefix(t *testing.T) {
	r := NewRouter([]config.RouteRule{{Prefix: "/svc", Cluster: "svc", StripPrefix: true}})
	rc := newRC("/svc/foo")
	_ = r.OnRequest(rc)
	if rc.UpstreamCluster != "svc" {
		t.Fatalf("want cluster svc, got %q", rc.UpstreamCluster)
	}
	if rc.RewritePath != "/foo" {
		t.Fatalf("want rewrite /foo, got %q", rc.RewritePath)
	}
}

func TestRouter_CatchAll(t *testing.T) {
	r := NewRouter([]config.RouteRule{{Prefix: "/", Cluster: "default"}})
	rc := newRC("/anything")
	_ = r.OnRequest(rc)
	if rc.UpstreamCluster != "default" {
		t.Fatalf("want cluster default, got %q", rc.UpstreamCluster)
	}
}
