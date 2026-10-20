// Package cache holds the cluster agent's in-memory copy of the cluster:
// core, apps, batch, networking, autoscaling, policy and RBAC objects, plus
// the Argo CD, Traefik and CNPG custom resources when the cluster has them.
// It is filled by list+watch (client-go shared informers), so reads never hit
// the API server. Secrets are never cached.
//
// Every resource is checked before its informer starts: a resource the
// cluster does not serve is "absent", and one the agent's RBAC does not allow
// is "forbidden". Neither stops the rest of the cache.
package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	toolscache "k8s.io/client-go/tools/cache"
)

// State is where one resource's informer stands.
type State string

const (
	StatePending   State = "pending"   // not started yet
	StateAbsent    State = "absent"    // the cluster does not serve it (CRD not installed)
	StateForbidden State = "forbidden" // RBAC does not allow list/watch
	StateError     State = "error"     // the access check failed for another reason
	StateSyncing   State = "syncing"   // informer started, first list not done
	StateSynced    State = "synced"    // up to date, served from memory
)

// ResourceStatus describes one resource for logs, the summary push (KM-24)
// and the portal's diagnostics.
type ResourceStatus struct {
	GVR   schema.GroupVersionResource
	Kind  string
	State State
	Count int
	// LastError is the latest list or watch error, if any. A synced
	// informer that loses its watch keeps serving the last known data and
	// retries with backoff, so State stays synced while LastError is set.
	LastError string
}

// Options tune the cache. Zero values give the defaults.
type Options struct {
	// MaxConfigMapValue is the largest ConfigMap value kept in memory, in
	// bytes; longer values are cut. Default 16 KiB.
	MaxConfigMapValue int
	// CheckTimeout bounds each access check at start. Default 10 s.
	CheckTimeout time.Duration
	Logger       *slog.Logger
}

// ErrNotCached is returned by List and Get for a resource the cache does not
// hold (unknown, absent, forbidden, or not synced yet).
var ErrNotCached = errors.New("resource not cached")

// Cache is the agent's object cache. Create it with New, then call Start and
// WaitForSync. All read methods are safe for concurrent use.
type Cache struct {
	kube kubernetes.Interface
	dyn  dynamic.Interface
	disc discovery.DiscoveryInterface
	opts Options
	log  *slog.Logger

	typed   informers.SharedInformerFactory
	events  informers.SharedInformerFactory
	dynamic dynamicinformer.DynamicSharedInformerFactory

	mu         sync.RWMutex
	entries    map[schema.GroupVersionResource]*entry
	order      []schema.GroupVersionResource
	generation atomic.Uint64
}

type entry struct {
	res       Resource
	state     State
	lastError string
	informer  toolscache.SharedIndexInformer
	lister    toolscache.GenericLister
	// watchFailed is set by a watch error and cleared by the next event, so
	// a recovered watch does not keep showing an old error.
	watchFailed atomic.Bool
}

// New returns an empty cache. It does not talk to the API server yet.
func New(kube kubernetes.Interface, dyn dynamic.Interface, disc discovery.DiscoveryInterface, opts Options) *Cache {
	if opts.MaxConfigMapValue <= 0 {
		opts.MaxConfigMapValue = 16 << 10
	}
	if opts.CheckTimeout <= 0 {
		opts.CheckTimeout = 10 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	tf := transform(opts.MaxConfigMapValue)
	c := &Cache{
		kube: kube, dyn: dyn, disc: disc, opts: opts, log: opts.Logger,
		typed: informers.NewSharedInformerFactoryWithOptions(kube, 0, informers.WithTransform(tf)),
		// Normal events are most of all events and never shown; keep only warnings.
		events: informers.NewSharedInformerFactoryWithOptions(kube, 0, informers.WithTransform(tf),
			informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.FieldSelector = "type=Warning" })),
		dynamic: dynamicinformer.NewFilteredDynamicSharedInformerFactory(dyn, 0, metav1.NamespaceAll, nil),
		entries: map[schema.GroupVersionResource]*entry{},
	}
	for _, r := range Resources() {
		c.entries[r.GVR] = &entry{res: r, state: StatePending}
		c.order = append(c.order, r.GVR)
	}
	return c
}

// Start checks every resource and starts the informers for the usable ones.
// It returns once the informers run; use WaitForSync to wait for the first
// full list. Informers stop when ctx ends.
func (c *Cache) Start(ctx context.Context) error {
	served, err := c.servedResources()
	if err != nil {
		return fmt.Errorf("cache: discovery: %w", err)
	}
	started := 0
	for _, gvr := range c.order {
		e := c.entries[gvr]
		if !served[gvr] {
			c.setState(e, StateAbsent, "")
			continue
		}
		if st, msg := c.checkAccess(ctx, e.res); st != StateSyncing {
			c.setState(e, st, msg)
			c.log.Warn("cache: resource skipped", "resource", gvr.String(), "state", st, "error", msg)
			continue
		}
		if err := c.register(e); err != nil {
			c.setState(e, StateError, err.Error())
			continue
		}
		c.setState(e, StateSyncing, "")
		started++
	}
	if started == 0 {
		return errors.New("cache: no resource can be watched; check the agent's RBAC")
	}
	c.typed.Start(ctx.Done())
	c.events.Start(ctx.Done())
	c.dynamic.Start(ctx.Done())
	return nil
}

// servedResources asks discovery which of our resources the cluster serves.
// A group version that fails discovery (for example an aggregated API that is
// down) only hides its own resources.
func (c *Cache) servedResources() (map[schema.GroupVersionResource]bool, error) {
	served := map[schema.GroupVersionResource]bool{}
	byGV := map[schema.GroupVersion][]schema.GroupVersionResource{}
	for _, gvr := range c.order {
		gv := gvr.GroupVersion()
		byGV[gv] = append(byGV[gv], gvr)
	}
	coreOK := false
	for gv, gvrs := range byGV {
		list, err := c.disc.ServerResourcesForGroupVersion(gv.String())
		if err != nil {
			if !apierrors.IsNotFound(err) {
				c.log.Warn("cache: discovery failed for group version", "groupVersion", gv.String(), "error", err)
			}
			continue
		}
		if gv.Group == "" {
			coreOK = true
		}
		names := map[string]bool{}
		for _, r := range list.APIResources {
			names[r.Name] = true
		}
		for _, gvr := range gvrs {
			served[gvr] = names[gvr.Resource]
		}
	}
	if !coreOK {
		return nil, errors.New("core API group v1 not served")
	}
	return served, nil
}

// checkAccess lists one object to learn whether RBAC allows the resource,
// so a forbidden informer does not retry and log forever.
func (c *Cache) checkAccess(ctx context.Context, r Resource) (State, string) {
	ctx, cancel := context.WithTimeout(ctx, c.opts.CheckTimeout)
	defer cancel()
	opts := metav1.ListOptions{Limit: 1}
	if r.GVR == Events {
		opts.FieldSelector = "type=Warning"
	}
	_, err := c.dyn.Resource(r.GVR).List(ctx, opts)
	switch {
	case err == nil:
		return StateSyncing, ""
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
		return StateForbidden, err.Error()
	case apierrors.IsNotFound(err):
		return StateAbsent, err.Error()
	default:
		return StateError, err.Error()
	}
}

func (c *Cache) register(e *entry) error {
	var gi informers.GenericInformer
	var err error
	switch {
	case e.res.Optional:
		gi = c.dynamic.ForResource(e.res.GVR)
	case e.res.GVR == Events:
		gi, err = c.events.ForResource(e.res.GVR)
	default:
		gi, err = c.typed.ForResource(e.res.GVR)
	}
	if err != nil {
		return err
	}
	inf := gi.Informer()
	if e.res.Optional {
		// The typed factories apply the transform themselves.
		if err := inf.SetTransform(transform(c.opts.MaxConfigMapValue)); err != nil {
			return err
		}
	}
	if err := inf.SetWatchErrorHandler(func(_ *toolscache.Reflector, err error) { c.watchError(e, err) }); err != nil {
		return err
	}
	bump := func() { c.changed(e) }
	if _, err := inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { bump() },
		UpdateFunc: func(any, any) { bump() },
		DeleteFunc: func(any) { bump() },
	}); err != nil {
		return err
	}
	c.mu.Lock()
	e.informer, e.lister = inf, gi.Lister()
	c.mu.Unlock()
	return nil
}

// watchError records a list or watch failure; the informer retries it.
func (c *Cache) watchError(e *entry, err error) {
	c.mu.Lock()
	e.lastError = err.Error()
	c.mu.Unlock()
	e.watchFailed.Store(true)
	c.log.Warn("cache: watch error", "resource", e.res.GVR.String(), "error", err)
}

// changed counts an add, update or delete. The first event after a watch
// error clears that error: the watch works again.
func (c *Cache) changed(e *entry) {
	c.generation.Add(1)
	if e.watchFailed.CompareAndSwap(true, false) {
		c.mu.Lock()
		e.lastError = ""
		c.mu.Unlock()
	}
}

func (c *Cache) setState(e *entry, s State, msg string) {
	c.mu.Lock()
	e.state, e.lastError = s, msg
	c.mu.Unlock()
}

// WaitForSync blocks until every started informer has done its first full
// list, or ctx ends. It reports whether all of them synced; the ones that did
// are usable either way.
func (c *Cache) WaitForSync(ctx context.Context) bool {
	c.mu.RLock()
	var waiting []*entry
	for _, gvr := range c.order {
		if e := c.entries[gvr]; e.state == StateSyncing {
			waiting = append(waiting, e)
		}
	}
	c.mu.RUnlock()
	all := true
	for _, e := range waiting {
		if toolscache.WaitForCacheSync(ctx.Done(), e.informer.HasSynced) {
			c.setState(e, StateSynced, c.lastError(e))
		} else {
			all = false
		}
	}
	return all
}

func (c *Cache) lastError(e *entry) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return e.lastError
}

// Generation grows by one on every add, update or delete of any cached
// object. Callers that derive data (summary, relationships) recompute only
// when it has changed.
func (c *Cache) Generation() uint64 { return c.generation.Load() }

// promoteLate marks every syncing entry whose informer has now done its
// first list as synced. WaitForSync may have given up before a slow list
// finished (the caller then serves what is synced); without this, such a
// resource would stay unusable until the agent restarts.
func (c *Cache) promoteLate() {
	c.mu.RLock()
	late := false
	for _, e := range c.entries {
		if e.state == StateSyncing && e.informer != nil && e.informer.HasSynced() {
			late = true
			break
		}
	}
	c.mu.RUnlock()
	if !late {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		if e.state == StateSyncing && e.informer != nil && e.informer.HasSynced() {
			e.state = StateSynced
			c.log.Info("cache: resource synced late", "resource", e.res.GVR.String())
		}
	}
}

func (c *Cache) usable(gvr schema.GroupVersionResource) (*entry, error) {
	c.promoteLate()
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[gvr]
	if !ok {
		return nil, fmt.Errorf("%w: %s is not a cached resource", ErrNotCached, gvr)
	}
	if e.state != StateSynced {
		return nil, fmt.Errorf("%w: %s is %s", ErrNotCached, gvr, e.state)
	}
	return e, nil
}

// List returns the cached objects of one resource. An empty namespace means
// all namespaces (and is required for cluster-scoped resources); a nil
// selector means everything. Built-in objects are typed (*corev1.Pod, ...),
// custom resources are *unstructured.Unstructured. The objects are shared
// with the cache: callers must not change them.
func (c *Cache) List(gvr schema.GroupVersionResource, namespace string, sel labels.Selector) ([]runtime.Object, error) {
	e, err := c.usable(gvr)
	if err != nil {
		return nil, err
	}
	if sel == nil {
		sel = labels.Everything()
	}
	if namespace == "" {
		return e.lister.List(sel)
	}
	return e.lister.ByNamespace(namespace).List(sel)
}

// Get returns one cached object, or a Kubernetes NotFound error.
// The object is shared with the cache: callers must not change it.
func (c *Cache) Get(gvr schema.GroupVersionResource, namespace, name string) (runtime.Object, error) {
	e, err := c.usable(gvr)
	if err != nil {
		return nil, err
	}
	if namespace == "" {
		return e.lister.Get(name)
	}
	return e.lister.ByNamespace(namespace).Get(name)
}

// Status reports every resource, in a stable order.
func (c *Cache) Status() []ResourceStatus {
	c.promoteLate()
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ResourceStatus, 0, len(c.order))
	for _, gvr := range c.order {
		e := c.entries[gvr]
		s := ResourceStatus{GVR: gvr, Kind: e.res.Kind, State: e.state, LastError: e.lastError}
		if e.informer != nil {
			s.Count = len(e.informer.GetStore().ListKeys())
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return stateRank(out[i].State) < stateRank(out[j].State) })
	return out
}

func stateRank(s State) int {
	switch s {
	case StateSynced:
		return 0
	case StateSyncing:
		return 1
	case StateForbidden, StateError:
		return 2
	}
	return 3
}
