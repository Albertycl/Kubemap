// Package localcluster is the cluster agent that runs inside the server
// process: the informer cache, the query handler, log streams, summaries and
// the action runner. The server attaches it to its Hub with
// tunnel.Hub.AttachLocal, so no WebSocket, token or registration sits
// between the server and the cluster it runs in.
package localcluster

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"time"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"kubemap/agent/cluster/cache"
	"kubemap/agent/cluster/graph"
	"kubemap/agent/cluster/health"
	"kubemap/agent/cluster/query"
	"kubemap/agent/cluster/stream"
	"kubemap/agent/cluster/summary"
	"kubemap/agent/cluster/traces"
	"kubemap/pkg/protocol"
	"kubemap/portal/tunnel"
)

// ClusterID and ClusterName identify the one cluster the server runs in.
const (
	ClusterID   = "local"
	ClusterName = "this cluster"
)

// Agent implements tunnel.Local.
type Agent struct {
	Cache      *cache.Cache
	Handler    *query.Handler
	Streams    *stream.Manager
	Graphs     *graph.Builder
	K8sVersion string
	Log        *slog.Logger
}

var _ tunnel.Local = (*Agent)(nil)

// Start fills the cache from the cluster the server runs in and returns the
// agent. namespace is the server's own namespace (for the action runner).
func Start(ctx context.Context, cfg *rest.Config, namespace string, log *slog.Logger) (*Agent, error) {
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, err
	}
	c := cache.New(kube, dyn, disc, cache.Options{Logger: log})
	if err := c.Start(ctx); err != nil {
		return nil, err
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	if !c.WaitForSync(wctx) {
		log.Warn("cache not fully synced; serving what is synced")
	}
	cancel()
	k8sVersion := ""
	if v, err := disc.ServerVersion(); err == nil {
		k8sVersion = v.GitVersion
	}
	return New(c, kube, dyn, namespace, k8sVersion, log), nil
}

// New builds the agent on an existing cache and clients; tests use it with
// fakes.
func New(c *cache.Cache, kube kubernetes.Interface, dyn dynamic.Interface, namespace, k8sVersion string, log *slog.Logger) *Agent {
	graphs := graph.NewBuilder(c)
	h := query.New(c, kube, dyn, graphs)
	// Traces confirm app map links (KM-54): Tempo found in the cluster, or
	// KUBEMAP_TEMPO_URL; "off" disables it. Only HTTP GET searches.
	h.Traces = traces.NewSource(os.Getenv("KUBEMAP_TEMPO_URL"))
	if t, err := HealthThresholds(); err != nil {
		log.Warn("health thresholds; using the kubelet defaults", "err", err)
	} else {
		h.Thresholds = t
	}
	return &Agent{Cache: c, Handler: h, Streams: &stream.Manager{Logs: stream.KubeLogs(kube)}, Graphs: graphs, K8sVersion: k8sVersion, Log: log}
}

func (a *Agent) Handle(ctx context.Context, req *protocol.Request) *protocol.Response {
	return a.Handler.Handle(ctx, req)
}

func (a *Agent) OpenStream(ctx context.Context, id string, o *protocol.StreamOpen, s tunnel.Sender) {
	a.Streams.OpenFor(ctx, id, o, s)
}

func (a *Agent) CloseStream(id string) { a.Streams.Close(id) }
func (a *Agent) CloseStreams()         { a.Streams.CloseAll() }

// RunSummary pushes the cluster summary to s until ctx ends.
func (a *Agent) RunSummary(ctx context.Context, s tunnel.Sender) {
	p := &summary.Pusher{Src: a.Cache, Graph: a.Graphs, Sender: s, K8sVersion: a.K8sVersion}
	if err := p.Run(ctx); err != nil && ctx.Err() == nil {
		a.Log.Warn("summary push stopped", "err", err)
	}
}

// HealthThresholds reads the kubelet eviction settings for health.check from
// the environment, in the kubelet's own syntax:
//
//	KUBEMAP_EVICTION_HARD="nodefs.available<5%,imagefs.available<5%"
//	KUBEMAP_EVICTION_MINIMUM_RECLAIM="nodefs.available=10%,imagefs.available=10%"
//	KUBEMAP_IMAGE_GC_HIGH_PERCENT=85
func HealthThresholds() (health.Thresholds, error) {
	gc := 0
	if v := os.Getenv("KUBEMAP_IMAGE_GC_HIGH_PERCENT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return health.Defaults(), err
		}
		gc = n
	}
	t, err := health.Configure(os.Getenv("KUBEMAP_EVICTION_HARD"), os.Getenv("KUBEMAP_EVICTION_MINIMUM_RECLAIM"), gc)
	if err != nil {
		return health.Defaults(), err
	}
	return t, nil
}
