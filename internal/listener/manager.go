package listener

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/mayurvarma14/go-proxy/internal/config"
	"github.com/mayurvarma14/go-proxy/internal/filter"
)

type Manager struct{ listeners []*Listener }

func NewManager(cfg config.ProxyConfig, reg filter.Registry) (*Manager, error) {
	ls := make([]*Listener, 0, len(cfg.Listeners))
	for _, spec := range cfg.Listeners {
		l, err := newListener(spec, reg)
		if err != nil {
			return nil, fmt.Errorf("listener %q: %w", spec.Name, err)
		}
		ls = append(ls, l)
	}
	return &Manager{listeners: ls}, nil
}

func (m *Manager) Start(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(len(m.listeners))
	errCh := make(chan error, len(m.listeners))
	for _, l := range m.listeners {
		go func(l *Listener) {
			defer wg.Done()
			if err := l.run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				errCh <- err
			}
		}(l)
	}
	select {
	case <-ctx.Done():
	case err := <-errCh:
		return err
	}
	wg.Wait()
	return nil
}

// StopAccept stops all listeners from accepting new connections. Existing
// connections continue until they complete.
func (m *Manager) StopAccept() {
    for _, l := range m.listeners {
        l.StopAccept()
    }
}
