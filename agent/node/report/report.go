// Package report connects a node agent to the portal (KM-29): it registers
// with the node's projected token (KM-68), pushes a metrics sample every
// protocol.MetricsInterval while the node is accepted, and re-registers with
// a fresh token before the old one expires. Each node agent has its own
// connection, so a node keeps reporting when the node running the cluster
// agent is down.
package report

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"kubemap/agent/node/identity"
	"kubemap/agent/tunnel"
	"kubemap/pkg/protocol"
)

// Collector takes one metrics sample (collect.Collector).
type Collector interface {
	Collect() (*protocol.Metrics, error)
}

// Identity loads the node identity: identity.Load in production, a fake in
// tests. It is called on every connect, so a rotated token is picked up.
type Identity func(now time.Time) (*identity.Identity, error)

// Registrar returns a tunnel.Registrar that loads a fresh identity for every
// connect, and a SessionUntil that ends the connection when that identity's
// token is due for renewal.
func Registrar(load Identity, agentVersion string, now func() time.Time) (tunnel.Registrar, func() time.Time) {
	var mu sync.Mutex
	var current *identity.Identity
	reg := func(tunnel.State) (*protocol.Register, error) {
		id, err := load(now())
		if err != nil {
			return nil, err
		}
		mu.Lock()
		current = id
		mu.Unlock()
		return id.Register(agentVersion), nil
	}
	until := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		if current == nil {
			return now().Add(time.Hour)
		}
		return current.RenewAt(now())
	}
	return reg, until
}

// Pusher sends a metrics sample every Interval until ctx ends. A failed
// sample is logged and skipped: one unreadable /proc file must not stop the
// node's reports. A failed send ends the loop; the tunnel reconnects.
type Pusher struct {
	Collector Collector
	Sender    tunnel.Sender
	Interval  time.Duration // default protocol.MetricsInterval
	Log       *slog.Logger
}

// Run pushes until ctx ends or a send fails.
func (p *Pusher) Run(ctx context.Context) error {
	interval := p.Interval
	if interval <= 0 {
		interval = protocol.MetricsInterval
	}
	log := p.Log
	if log == nil {
		log = slog.Default()
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
		m, err := p.Collector.Collect()
		if err != nil {
			failures++
			if failures == 1 || failures%30 == 0 { // do not flood the log
				log.Warn("metrics sample failed", "err", err, "failures", failures)
			}
			continue
		}
		failures = 0
		if err := p.Sender.Send("", m); err != nil {
			return err
		}
	}
}
