package listener

import (
    "context"
    "crypto/tls"
    "errors"
    "fmt"
    "net/http"
    "net"
    "os"
    "crypto/x509"
    "strings"
    "sync"
    "time"

    "github.com/mayurvarma14/go-proxy/internal/config"
    "github.com/mayurvarma14/go-proxy/internal/connctx"
    "github.com/mayurvarma14/go-proxy/internal/filter"
    "github.com/mayurvarma14/go-proxy/internal/obs/metrics"
    http2 "golang.org/x/net/http2"
)

type Listener struct {
    name  string
    addr  string
    ln    net.Listener
    tlsC  *tls.Config
    chains []chainEntry
    wg    sync.WaitGroup
    h2h   http.Handler
}

type chainEntry struct {
    chain     filter.Chain
    sniHosts  map[string]struct{}
    isDefault bool
}

func newListener(spec config.ListenerSpec, reg filter.Registry) (*Listener, error) {
    l := &Listener{name: spec.Name, addr: spec.Address}
    l.h2h = reg.H2Handler()
    // Build all filter chains; first chain acts as default
    for i, fc := range spec.FilterChains {
        var fs []filter.NetworkFilter
        for _, fname := range fc.Filters {
            f, ok := reg.Resolve(fname)
            if !ok {
                return nil, fmt.Errorf("unknown filter: %s", fname)
            }
            fs = append(fs, f)
        }
        ce := chainEntry{chain: filter.NewChain(fs), sniHosts: map[string]struct{}{}, isDefault: i == 0}
        for _, h := range fc.SNIHosts {
            ce.sniHosts[h] = struct{}{}
        }
        l.chains = append(l.chains, ce)
    }
    if spec.TLS != nil {
        cert, err := tls.LoadX509KeyPair(spec.TLS.CertPath, spec.TLS.KeyPath)
        if err != nil {
            return nil, fmt.Errorf("load tls keypair: %w", err)
        }
        minVer := uint16(tls.VersionTLS12)
        switch spec.TLS.MinVersion {
        case "1.3":
            minVer = tls.VersionTLS13
        case "", "1.2":
            minVer = tls.VersionTLS12
        }
        tconf := &tls.Config{
            MinVersion: minVer,
            Certificates: []tls.Certificate{cert},
            // Advertise h2 and http/1.1 via ALPN
            NextProtos: []string{"h2", "http/1.1"},
        }
        // Additional named certificates bound to SNI hosts
        if len(spec.TLS.Certs) > 0 {
            certMap := map[string]*tls.Certificate{}
            for _, ce := range spec.TLS.Certs {
                cpair, err := tls.LoadX509KeyPair(ce.CertPath, ce.KeyPath)
                if err != nil { return nil, fmt.Errorf("load tls keypair (extra): %w", err) }
                for _, h := range ce.SNIHosts {
                    certCopy := cpair
                    certMap[strings.ToLower(strings.TrimSpace(h))] = &certCopy
                }
            }
            def := &tconf.Certificates[0]
            tconf.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
                if hello == nil || hello.ServerName == "" { return def, nil }
                if c := certMap[strings.ToLower(hello.ServerName)]; c != nil { return c, nil }
                return def, nil
            }
        }
        // Optional mTLS
        if spec.TLS.RequireClientCert {
            if spec.TLS.ClientCAPath == "" {
                return nil, fmt.Errorf("tls: require_client_cert true but client_ca_path empty")
            }
            // Load CA cert pool
            caPEM, err := os.ReadFile(spec.TLS.ClientCAPath)
            if err != nil { return nil, fmt.Errorf("tls: read client_ca_path: %w", err) }
            pool := x509.NewCertPool()
            if !pool.AppendCertsFromPEM(caPEM) {
                return nil, fmt.Errorf("tls: failed to parse client CA certs")
            }
            tconf.ClientCAs = pool
            tconf.ClientAuth = tls.RequireAndVerifyClientCert
        }
        l.tlsC = tconf
    }
    return l, nil
}

func (l *Listener) run(ctx context.Context) error {
    ln, err := net.Listen("tcp", l.addr)
    if err != nil {
        return fmt.Errorf("listen %s: %w", l.addr, err)
    }
    // Wrap with TLS if configured
    if l.tlsC != nil {
        l.ln = tls.NewListener(ln, l.tlsC)
    } else {
        l.ln = ln
    }
    fmt.Printf("listener %q started on %s\n", l.name, l.addr)
    // Ensure Accept unblocks when ctx is canceled by closing the listener.
    go func() {
        <-ctx.Done()
        _ = l.ln.Close()
    }()

    for {
        conn, err := l.ln.Accept()
        if err != nil {
            if errors.Is(err, net.ErrClosed) {
                // Listener closed (drain or shutdown). Wait for handlers to finish.
                l.wg.Wait()
                fmt.Printf("listener %q stopped\n", l.name)
                return nil
            }
            if ne, ok := err.(net.Error); ok && ne.Temporary() {
                time.Sleep(50 * time.Millisecond)
                continue
            }
            select {
            case <-ctx.Done():
                l.wg.Wait()
                fmt.Printf("listener %q stopped\n", l.name)
                return ctx.Err()
            default:
            }
            return fmt.Errorf("accept: %w", err)
        }
        metrics.Inc("downstream_connections_accepted_total")
        l.wg.Add(1)
        go l.handle(ctx, conn)
    }
}

// StopAccept closes the underlying listener to stop accepting new connections.
func (l *Listener) StopAccept() {
    if l.ln != nil {
        _ = l.ln.Close()
    }
}

func (l *Listener) handle(ctx context.Context, conn net.Conn) {
    defer l.wg.Done()
    metrics.AddGauge("downstream_active_connections", 1)
    done := make(chan struct{})
    // Ensure long-running filters (e.g., echo) unblock on shutdown.
    go func() {
        select {
        case <-ctx.Done():
            // Trigger any blocking reads to return.
            _ = conn.SetReadDeadline(time.Now())
            _ = conn.Close()
        case <-done:
        }
    }()

    defer func() { close(done); _ = conn.Close(); metrics.AddGauge("downstream_active_connections", -1) }()

    // Prepare per-connection context
    cctx := &connctx.ConnCtx{Context: ctx, Conn: conn, Remote: conn.RemoteAddr(), Start: time.Now()}

    // If TLS, complete handshake and pick chain based on SNI or h2
    var chosen filter.Chain
    if tc, ok := conn.(*tls.Conn); ok {
        _ = tc.Handshake()
        st := tc.ConnectionState()
        cctx.TLS = &st
        if st.NegotiatedProtocol == "h2" && l.h2h != nil {
            // Serve HTTP/2 over this TLS connection using our handler
            srv := &http2.Server{}
            srv.ServeConn(tc, &http2.ServeConnOpts{Handler: l.h2h})
            return
        }
        sni := strings.TrimSpace(st.ServerName)
        // choose first chain whose SNIHosts contains sni; else default
        picked := false
        if sni != "" {
            for _, ce := range l.chains {
                if len(ce.sniHosts) == 0 { continue }
                if _, ok := ce.sniHosts[sni]; ok {
                    chosen = ce.chain
                    picked = true
                    break
                }
            }
        }
        if !picked {
            // default: first chain or first marked default
            if len(l.chains) > 0 {
                for i, ce := range l.chains {
                    if i == 0 || ce.isDefault { chosen = ce.chain; break }
                }
            }
        }
    } else {
        // Non-TLS: use first/default chain
        if len(l.chains) > 0 { chosen = l.chains[0].chain }
    }
    _ = chosen.Execute(cctx)
}
