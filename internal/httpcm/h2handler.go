package httpcm

import (
    "bufio"
    "bytes"
    "fmt"
    "net/http"
    "strings"
    "time"

    "github.com/mayurvarma14/go-proxy/internal/cluster"
    "github.com/mayurvarma14/go-proxy/internal/config"
)

// NewH2Handler builds an http.Handler that runs the same HTTP filter chain
// used by HCM, but writes responses via http.ResponseWriter. It works by
// translating the http.Response bytes produced by filters into ResponseWriter
// calls (status + headers + body).
func NewH2Handler(cm *cluster.Manager, cfg config.ProxyConfig) http.Handler {
    var httpFilters []HTTPFilter
    httpFilters = append(httpFilters, LoggingFilter{})
    httpFilters = append(httpFilters, NewForwardedHeaders("go-proxy"))
    if rl := NewRateLimiter(cfg.RateLimit); rl != nil {
        httpFilters = append(httpFilters, rl)
    }
    httpFilters = append(httpFilters, NewRouter(cfg.Routes))
    httpFilters = append(httpFilters, NewUpstreamProxy(cm, cfg.Clusters))
    chain := NewChain(httpFilters)
    return h2Handler{chain: chain}
}

type h2Handler struct{ chain Chain }

func (h h2Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    // Build RequestCtx compatible with our filters
    rc := &RequestCtx{
        Context: r.Context(),
        Req:     r,
        Remote:  stringAddr{s: r.RemoteAddr},
        Start:   time.Now(),
        W:       newRespTranslator(w),
        DownstreamTLS: r.TLS != nil,
    }
    _ = h.chain.Run(rc)
    if !rc.Responded {
        // Default response as in HCM
        status := http.StatusNotFound
        body := "no route\n"
        if rc.UpstreamCluster != "" {
            status = http.StatusOK
            body = "routed to: " + rc.UpstreamCluster + "\n"
        }
        w.Header().Set("Content-Type", "text/plain; charset=utf-8")
        w.WriteHeader(status)
        _, _ = w.Write([]byte(body))
    }
}

// stringAddr is a minimal net.Addr backed by a string host:port.
type stringAddr struct{ s string }
func (a stringAddr) Network() string { return "tcp" }
func (a stringAddr) String() string  { return a.s }

// respTranslator parses an HTTP/1.x response stream and applies it to an
// http.ResponseWriter (status, headers, then body). This lets existing filters
// that write http.Response.Write() work for HTTP/2 as well.
type respTranslator struct {
    w       http.ResponseWriter
    buf     bytes.Buffer
    parsed  bool
}

func newRespTranslator(w http.ResponseWriter) *respTranslator { return &respTranslator{w: w} }

func (rt *respTranslator) Write(p []byte) (int, error) {
    if rt.parsed {
        return rt.w.Write(p)
    }
    // Accumulate until we see CRLF CRLF
    rt.buf.Write(p)
    data := rt.buf.Bytes()
    sep := bytes.Index(data, []byte("\r\n\r\n"))
    if sep == -1 {
        // Not enough yet
        return len(p), nil
    }
    head := data[:sep]
    rest := data[sep+4:]
    // Parse status line + headers
    br := bufio.NewReader(bytes.NewReader(head))
    // Status line: HTTP/1.1 200 OK
    line, _ := br.ReadString('\n')
    parts := strings.Split(strings.TrimSpace(line), " ")
    status := http.StatusOK
    if len(parts) >= 2 {
        // parse code
        if c, err := strconvAtoi(parts[1]); err == nil {
            status = c
        }
    }
    // Headers
    hdr := http.Header{}
    for {
        l, err := br.ReadString('\n')
        if err != nil || len(l) == 0 { break }
        s := strings.TrimRight(l, "\r\n")
        if s == "" { break }
        kv := strings.SplitN(s, ":", 2)
        if len(kv) == 2 {
            k := strings.TrimSpace(kv[0])
            v := strings.TrimSpace(kv[1])
            hdr.Add(k, v)
        }
    }
    for k, vv := range hdr {
        for _, v := range vv { rt.w.Header().Add(k, v) }
    }
    rt.w.WriteHeader(status)
    // Mark parsed and write remaining body (if any)
    rt.parsed = true
    if len(rest) > 0 {
        _, _ = rt.w.Write(rest)
    }
    // Clear buffer to release memory
    rt.buf.Reset()
    return len(p), nil
}

// tiny atoi to avoid importing strconv in this file
func strconvAtoi(s string) (int, error) {
    n := 0
    for i := 0; i < len(s); i++ {
        c := s[i]
        if c < '0' || c > '9' { return 0, fmt.Errorf("invalid") }
        n = n*10 + int(c-'0')
    }
    return n, nil
}
