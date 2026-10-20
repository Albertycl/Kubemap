package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"kubemap/agent/node/collect"
	"kubemap/agent/node/identity"
	"kubemap/agent/node/report"
	"kubemap/agent/tunnel"
	"kubemap/pkg/version"
)

// runNodeAgent is the node agent proper (KM-29): it samples this node and
// sends the samples over its own connection to the server, registered with
// the node's projected ServiceAccount token (KM-68). The settings come from
// the DaemonSet: NODE_NAME (downward API) and CLUSTER_ID, PORTAL_URL (the
// server Service, plain ws inside the cluster).
func runNodeAgent(procRoot, sysRoot, hostRoot, tokenPath string) int {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("agent", "node", "version", version.Version)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := identity.Config{TokenPath: tokenPath, ClusterID: os.Getenv("CLUSTER_ID"), NodeName: os.Getenv("NODE_NAME")}
	// Check the identity once at start, so a wrong manifest fails loudly
	// here instead of as "unauthorized" at the portal.
	id, err := identity.Load(cfg, time.Now())
	if err != nil {
		log.Error("node identity", "err", err)
		return 1
	}
	col, err := collect.New(collect.Config{ProcRoot: procRoot, SysRoot: sysRoot, HostRoot: hostRoot, NodeName: cfg.NodeName})
	if err != nil {
		log.Error("node collector", "err", err)
		return 1
	}
	load := func(now time.Time) (*identity.Identity, error) { return identity.Load(cfg, now) }
	reg, until := report.Registrar(load, version.Version, time.Now)
	client, err := tunnel.New(tunnel.Config{
		PortalURL: os.Getenv("PORTAL_URL"), Log: log,
		Register: reg, SessionUntil: until,
		Handlers: tunnel.Handlers{
			OnActive: func(ctx context.Context, s tunnel.Sender) {
				p := &report.Pusher{Collector: col, Sender: s, Log: log}
				if err := p.Run(ctx); err != nil && ctx.Err() == nil {
					log.Warn("metrics push stopped", "err", err)
				}
			},
		},
	}, tunnel.State{ClusterID: cfg.ClusterID})
	if err != nil {
		log.Error("tunnel", "err", err)
		return 1
	}
	log.Info("node agent started", "identity", id.String(), "portal", os.Getenv("PORTAL_URL"))
	if err := client.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("tunnel stopped", "err", err)
		return 1
	}
	log.Info("node agent stopped")
	return 0
}
