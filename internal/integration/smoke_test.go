package integration

import (
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net"
    "net/http"
    "os"
    "path/filepath"
    "testing"
    "time"

    "github.com/mayurvarma14/go-proxy/internal/admin"
    "github.com/mayurvarma14/go-proxy/internal/runtime/supervisor"
)

func freePort(t *testing.T) int {
    t.Helper()
    ln, err := net.Listen("tcp", "127.0.0.1:0")
    if err != nil { t.Fatalf("listen: %v", err) }
    defer ln.Close()
    return ln.Addr().(*net.TCPAddr).Port
}

func startEcho(t *testing.T, addr string) func() {
    t.Helper()
    mux := http.NewServeMux()
    mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "text/plain")
        w.WriteHeader(http.StatusOK)
        _, _ = w.Write([]byte("echo\n"))
    })
    srv := &http.Server{Addr: addr, Handler: mux}
    go srv.ListenAndServe()
    return func() {
        ctx, cancel := context.WithTimeout(context.Background(), time.Second)
        defer cancel()
        _ = srv.Shutdown(ctx)
    }
}

func TestSmoke_HTTP1_AdminEndpoints(t *testing.T) {
    echoPort := freePort(t)
    stopEcho := startEcho(t, fmt.Sprintf("127.0.0.1:%d", echoPort))
    defer stopEcho()

    listenPort := freePort(t)
    adminPort := freePort(t)

    // Build minimal JSON config
    cfg := fmt.Sprintf(`{
      "listeners": [
        {"name": "http", "address": "127.0.0.1:%d", "filter_chains": [{"name": "basic", "filters": ["logging", "hcm"]}]}
      ],
      "routes": [
        {"prefix": "/", "cluster": "echo"}
      ],
      "clusters": [
        {"name": "echo", "endpoints": ["127.0.0.1:%d"], "lb_policy": "round_robin"}
      ]
    }`, listenPort, echoPort)
    dir := t.TempDir()
    cfgPath := filepath.Join(dir, "smoke.json")
    if err := os.WriteFile(cfgPath, []byte(cfg), 0644); err != nil { t.Fatalf("write cfg: %v", err) }

    // Start supervisor + admin
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    sup := supervisor.New(ctx, cfgPath)
    if err := sup.Start(); err != nil { t.Fatalf("start: %v", err) }
    go func() {
        _ = admin.Start(ctx, fmt.Sprintf("127.0.0.1:%d", adminPort), sup.CurrentConfig, sup.Reload, sup.DrainStart, cancel, func() interface{} { return sup.EndpointsDebug() })
    }()

    // Give it a moment to bind
    time.Sleep(150 * time.Millisecond)

    // 1) Data plane smoke: GET through proxy
    resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", listenPort))
    if err != nil { t.Fatalf("proxy GET: %v", err) }
    defer resp.Body.Close()
    if resp.StatusCode != 200 { t.Fatalf("want 200, got %d", resp.StatusCode) }
    b, _ := io.ReadAll(resp.Body)
    if len(b) == 0 { t.Fatalf("expected body, got empty") }

    // 2) Admin /routes
    r2, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/routes", adminPort))
    if err != nil { t.Fatalf("admin /routes: %v", err) }
    var routes []map[string]any
    if err := json.NewDecoder(r2.Body).Decode(&routes); err != nil { t.Fatalf("decode routes: %v", err) }
    _ = r2.Body.Close()
    if len(routes) == 0 { t.Fatalf("expected routes, got none") }

    // 3) Admin /endpoints
    r3, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/endpoints", adminPort))
    if err != nil { t.Fatalf("admin /endpoints: %v", err) }
    var eps []map[string]any
    if err := json.NewDecoder(r3.Body).Decode(&eps); err != nil { t.Fatalf("decode endpoints: %v", err) }
    _ = r3.Body.Close()
    if len(eps) == 0 { t.Fatalf("expected endpoints snapshot, got none") }
}

