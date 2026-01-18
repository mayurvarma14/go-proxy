package supervisor

import (
    "context"
    "fmt"
    "sync"
    "time"

    "github.com/mayurvarma14/go-proxy/internal/cluster"
    "github.com/mayurvarma14/go-proxy/internal/config"
    "github.com/mayurvarma14/go-proxy/internal/filter"
    "github.com/mayurvarma14/go-proxy/internal/listener"
)

// Supervisor owns the live proxy components and supports hot reload.
type Supervisor struct {
    mu        sync.Mutex
    baseCtx   context.Context
    cfgPath   string

    // current runtime
    curCtx    context.Context
    curCancel context.CancelFunc
    curMgr    *listener.Manager
    curCM     *cluster.Manager
    curCfg    config.ProxyConfig
    doneCh    chan struct{}
}

func New(baseCtx context.Context, cfgPath string) *Supervisor {
    return &Supervisor{baseCtx: baseCtx, cfgPath: cfgPath}
}

// CurrentConfig returns a copy of the last applied config.
func (s *Supervisor) CurrentConfig() config.ProxyConfig {
    s.mu.Lock(); defer s.mu.Unlock()
    return s.curCfg
}

// Start loads config and starts listeners. Safe to call once on startup.
func (s *Supervisor) Start() error {
    s.mu.Lock()
    defer s.mu.Unlock()
    if s.curMgr != nil {
        return nil
    }
    cfg, err := config.Load(s.cfgPath)
    if err != nil { return err }
    if err := config.Validate(cfg); err != nil { return fmt.Errorf("config invalid: %w", err) }
    cm, err := cluster.NewManager(cfg.Clusters)
    if err != nil { return err }
    // child context that we can cancel independently for reload
    ctx, cancel := context.WithCancel(s.baseCtx)
    cm.Start(ctx)
    reg := filter.NewRegistry(cfg, cm)
    mgr, err := listener.NewManager(cfg, reg)
    if err != nil { cancel(); return err }

    done := make(chan struct{})
    go func() {
        _ = mgr.Start(ctx)
        close(done)
    }()

    s.curCfg = cfg
    s.curCM = cm
    s.curMgr = mgr
    s.curCtx = ctx
    s.curCancel = cancel
    s.doneCh = done
    return nil
}

// Reload stops the current listeners, waits for drain, then starts with new config.
func (s *Supervisor) Reload() error {
    s.mu.Lock()
    oldMgr := s.curMgr
    oldDone := s.doneCh
    oldCancel := s.curCancel
    s.mu.Unlock()

    // Phase 1: drain — stop accepting, wait up to timeout for handlers to finish
    if oldMgr != nil {
        oldMgr.StopAccept()
    }
    drained := make(chan struct{})
    go func() {
        if oldDone != nil { <-oldDone }
        close(drained)
    }()
    const drainTimeout = 5 * time.Second
    select {
    case <-drained:
        // drained cleanly
    case <-time.After(drainTimeout):
        // Phase 2: force — cancel remaining work
        if oldCancel != nil { oldCancel() }
        if oldDone != nil { <-oldDone }
    }

    s.mu.Lock()
    defer s.mu.Unlock()

    cfg, err := config.Load(s.cfgPath)
    if err != nil {
        return fmt.Errorf("reload: load config: %w", err)
    }
    if err := config.Validate(cfg); err != nil { return fmt.Errorf("reload: config invalid: %w", err) }
    cm, err := cluster.NewManager(cfg.Clusters)
    if err != nil { return fmt.Errorf("reload: cluster init: %w", err) }
    ctx, cancel := context.WithCancel(s.baseCtx)
    cm.Start(ctx)
    reg := filter.NewRegistry(cfg, cm)
    mgr, err := listener.NewManager(cfg, reg)
    if err != nil { cancel(); return fmt.Errorf("reload: listener init: %w", err) }

    done := make(chan struct{})
    go func() { _ = mgr.Start(ctx); close(done) }()

    s.curCfg = cfg
    s.curCM = cm
    s.curMgr = mgr
    s.curCtx = ctx
    s.curCancel = cancel
    s.doneCh = done
    return nil
}

// DrainStart stops accepting new downstream connections on all listeners.
// Existing connections are allowed to drain according to listener behavior.
func (s *Supervisor) DrainStart() error {
    s.mu.Lock()
    mgr := s.curMgr
    s.mu.Unlock()
    if mgr != nil {
        mgr.StopAccept()
    }
    return nil
}

// EndpointsDebug exposes a snapshot of cluster/endpoint runtime state for admin.
func (s *Supervisor) EndpointsDebug() []cluster.ClusterDebug {
    s.mu.Lock(); defer s.mu.Unlock()
    if s.curCM == nil { return nil }
    return s.curCM.DebugSnapshot()
}
