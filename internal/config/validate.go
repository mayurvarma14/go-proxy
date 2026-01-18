package config

import (
    "fmt"
    "net"
    "os"
    "strings"
)

// Validate performs basic semantic checks on a parsed ProxyConfig and returns
// a descriptive error on the first failure found.
func Validate(cfg ProxyConfig) error {
    // index clusters by name
    clusters := map[string]ClusterSpec{}
    for _, c := range cfg.Clusters {
        if c.Name == "" { return fmt.Errorf("cluster: empty name") }
        if _, dup := clusters[c.Name]; dup { return fmt.Errorf("cluster: duplicate name %q", c.Name) }
        clusters[c.Name] = c
        if len(c.Endpoints) == 0 {
            return fmt.Errorf("cluster %q: endpoints empty", c.Name)
        }
        for _, ep := range c.Endpoints {
            // Allow hostnames for strict_dns, but must contain port
            host, port, err := net.SplitHostPort(ep)
            if err != nil || port == "" || host == "" {
                return fmt.Errorf("cluster %q: invalid endpoint %q (want host:port)", c.Name, ep)
            }
        }
        switch strings.ToLower(c.LBPolicy) {
        case "", "round_robin", "random", "least_requests":
        default:
            return fmt.Errorf("cluster %q: invalid lb_policy %q", c.Name, c.LBPolicy)
        }
        if c.CircuitBreaker != nil {
            if c.CircuitBreaker.MaxRequests < 0 || c.CircuitBreaker.MaxPending < 0 || c.CircuitBreaker.PerEndpointMaxRequests < 0 {
                return fmt.Errorf("cluster %q: circuit_breaker values must be >= 0", c.Name)
            }
        }
    }

    // routes must target existing clusters
    for i, r := range cfg.Routes {
        if r.Prefix == "" { return fmt.Errorf("route[%d]: empty prefix", i) }
        if r.Cluster == "" { return fmt.Errorf("route[%d]: empty cluster", i) }
        if _, ok := clusters[r.Cluster]; !ok {
            return fmt.Errorf("route[%d]: unknown cluster %q", i, r.Cluster)
        }
    }

    // listeners address and TLS files
    for _, l := range cfg.Listeners {
        if l.Name == "" { return fmt.Errorf("listener: empty name") }
        if l.Address == "" { return fmt.Errorf("listener %q: empty address", l.Name) }
        if _, _, err := net.SplitHostPort(l.Address); err != nil {
            return fmt.Errorf("listener %q: invalid address %q (want host:port)", l.Name, l.Address)
        }
        if l.TLS != nil {
            if err := fileExists(l.TLS.CertPath); err != nil { return fmt.Errorf("listener %q: tls cert: %w", l.Name, err) }
            if err := fileExists(l.TLS.KeyPath); err != nil { return fmt.Errorf("listener %q: tls key: %w", l.Name, err) }
            if l.TLS.RequireClientCert {
                if l.TLS.ClientCAPath == "" { return fmt.Errorf("listener %q: tls require_client_cert but client_ca_path empty", l.Name) }
                if err := fileExists(l.TLS.ClientCAPath); err != nil { return fmt.Errorf("listener %q: tls client_ca_path: %w", l.Name, err) }
            }
            // Additional named certs (if any)
            for _, ce := range l.TLS.Certs {
                if err := fileExists(ce.CertPath); err != nil { return fmt.Errorf("listener %q: tls certs cert: %w", l.Name, err) }
                if err := fileExists(ce.KeyPath); err != nil { return fmt.Errorf("listener %q: tls certs key: %w", l.Name, err) }
            }
        }
        // filter names check (known simple set)
        known := map[string]struct{}{"logging": {}, "hcm": {}}
        if len(l.FilterChains) == 0 { return fmt.Errorf("listener %q: no filter_chains", l.Name) }
        for _, fc := range l.FilterChains {
            if len(fc.Filters) == 0 { return fmt.Errorf("listener %q: filter chain %q has no filters", l.Name, fc.Name) }
            for _, fn := range fc.Filters {
                if _, ok := known[fn]; !ok {
                    return fmt.Errorf("listener %q: unknown filter %q", l.Name, fn)
                }
            }
        }
    }
    return nil
}

func fileExists(path string) error {
    if path == "" { return fmt.Errorf("empty path") }
    if _, err := os.Stat(path); err != nil { return err }
    return nil
}

