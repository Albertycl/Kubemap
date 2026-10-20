package cache

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakedisc "k8s.io/client-go/discovery/fake"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

var argoApps = optionalResources[0].GVR

type fixture struct {
	kube *kubefake.Clientset
	dyn  *dynfake.FakeDynamicClient
	c    *Cache
}

// newFixture builds fake clients. Discovery serves every built-in resource
// plus the optional resources named in crds.
func newFixture(t *testing.T, crds []schema.GroupVersionResource, typed []runtime.Object, custom ...runtime.Object) *fixture {
	t.Helper()
	kube := kubefake.NewClientset(typed...)
	served := map[string][]metav1.APIResource{}
	add := func(g schema.GroupVersionResource) {
		gv := g.GroupVersion().String()
		served[gv] = append(served[gv], metav1.APIResource{Name: g.Resource, Namespaced: true})
	}
	for _, r := range builtinResources {
		add(r.GVR)
	}
	for _, g := range crds {
		add(g)
	}
	disc := kube.Discovery().(*fakedisc.FakeDiscovery)
	for gv, rs := range served {
		disc.Resources = append(disc.Resources, &metav1.APIResourceList{GroupVersion: gv, APIResources: rs})
	}
	listKinds := map[schema.GroupVersionResource]string{}
	for _, r := range Resources() {
		listKinds[r.GVR] = r.Kind + "List"
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, custom...)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &fixture{kube: kube, dyn: dyn, c: New(kube, dyn, disc, Options{Logger: log, MaxConfigMapValue: 8})}
}

func (f *fixture) start(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := f.c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
	defer wcancel()
	if !f.c.WaitForSync(wctx) {
		t.Fatal("cache did not sync")
	}
	return ctx
}

func status(c *Cache, g schema.GroupVersionResource) ResourceStatus {
	for _, s := range c.Status() {
		if s.GVR == g {
			return s
		}
	}
	return ResourceStatus{}
}

func pod(ns, name string, lbl map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: lbl}}
}

func TestNoSecrets(t *testing.T) {
	for _, r := range Resources() {
		if r.GVR.Resource == "secrets" {
			t.Fatal("the cache must never watch Secrets")
		}
	}
}

func TestListAndGet(t *testing.T) {
	f := newFixture(t, nil, []runtime.Object{
		pod("shop", "web-1", map[string]string{"app": "web"}),
		pod("shop", "db-0", map[string]string{"app": "db"}),
		pod("other", "web-9", map[string]string{"app": "web"}),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"}},
	})
	f.start(t)

	all, err := f.c.List(Pods, "", nil)
	if err != nil || len(all) != 3 {
		t.Fatalf("all pods = %d, %v", len(all), err)
	}
	web, _ := f.c.List(Pods, "shop", labels.SelectorFromSet(labels.Set{"app": "web"}))
	if len(web) != 1 || web[0].(*corev1.Pod).Name != "web-1" {
		t.Errorf("selector list = %v", web)
	}
	obj, err := f.c.Get(Deployments, "shop", "web")
	if err != nil || obj.(*appsv1.Deployment).Name != "web" {
		t.Errorf("get deployment = %v, %v", obj, err)
	}
	if _, err := f.c.Get(Namespaces, "", "shop"); err != nil {
		t.Errorf("get namespace: %v", err)
	}
	if _, err := f.c.Get(Pods, "shop", "nope"); !apierrors.IsNotFound(err) {
		t.Errorf("missing pod err = %v, want NotFound", err)
	}
	if st := status(f.c, Pods); st.State != StateSynced || st.Count != 3 {
		t.Errorf("pods status = %+v", st)
	}
	if _, err := f.c.List(schema.GroupVersionResource{Resource: "secrets", Version: "v1"}, "", nil); !errors.Is(err, ErrNotCached) {
		t.Errorf("secrets err = %v", err)
	}
}

func TestOptionalCRDAbsentAndPresent(t *testing.T) {
	app := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"namespace": "argocd", "name": "flask-vue",
			"managedFields": []any{map[string]any{"manager": "argocd"}}},
		"spec": map[string]any{"destination": map[string]any{"namespace": "flask-vue"}},
	}}
	f := newFixture(t, []schema.GroupVersionResource{argoApps}, nil, app)
	f.start(t)

	obj, err := f.c.Get(argoApps, "argocd", "flask-vue")
	if err != nil {
		t.Fatal(err)
	}
	u := obj.(*unstructured.Unstructured)
	if len(u.GetManagedFields()) != 0 {
		t.Error("managedFields not stripped from custom resource")
	}
	if ns, _, _ := unstructured.NestedString(u.Object, "spec", "destination", "namespace"); ns != "flask-vue" {
		t.Errorf("spec lost: %v", u.Object)
	}
	// Traefik and CNPG are not served by this fake cluster.
	for _, r := range optionalResources[1:] {
		if st := status(f.c, r.GVR); st.State != StateAbsent {
			t.Errorf("%s = %s, want absent", r.GVR, st.State)
		}
		if _, err := f.c.List(r.GVR, "", nil); !errors.Is(err, ErrNotCached) {
			t.Errorf("list absent %s: %v", r.GVR, err)
		}
	}
}

func TestForbiddenResourceIsSkipped(t *testing.T) {
	f := newFixture(t, nil, []runtime.Object{pod("a", "p", nil)})
	f.dyn.PrependReactor("list", "clusterroles", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "rbac.authorization.k8s.io", Resource: "clusterroles"}, "", errors.New("RBAC: denied"))
	})
	f.dyn.PrependReactor("list", "cronjobs", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection reset")
	})
	f.start(t)
	cr := gvr("rbac.authorization.k8s.io", "v1", "clusterroles")
	if st := status(f.c, cr); st.State != StateForbidden || !strings.Contains(st.LastError, "denied") {
		t.Errorf("clusterroles = %+v", st)
	}
	if st := status(f.c, gvr("batch", "v1", "cronjobs")); st.State != StateError {
		t.Errorf("cronjobs = %+v", st)
	}
	if _, err := f.c.List(cr, "", nil); !errors.Is(err, ErrNotCached) {
		t.Errorf("forbidden list err = %v", err)
	}
	// Everything else still works.
	if l, err := f.c.List(Pods, "", nil); err != nil || len(l) != 1 {
		t.Errorf("pods = %v, %v", l, err)
	}
	// Status lists synced resources first.
	if s := f.c.Status(); s[0].State != StateSynced || s[len(s)-1].State != StateAbsent {
		t.Errorf("status order: first %s, last %s", s[0].State, s[len(s)-1].State)
	}
}

func TestStartFailsWhenNothingIsAllowed(t *testing.T) {
	f := newFixture(t, nil, nil)
	f.dyn.PrependReactor("list", "*", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{}, "", errors.New("no"))
	})
	if err := f.c.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "RBAC") {
		t.Errorf("err = %v", err)
	}
}

func TestStartFailsWithoutCoreAPI(t *testing.T) {
	f := newFixture(t, nil, nil)
	disc := f.kube.Discovery().(*fakedisc.FakeDiscovery)
	disc.Resources = nil
	if err := f.c.Start(context.Background()); err == nil {
		t.Error("want an error when discovery serves nothing")
	}
}

func TestTransform(t *testing.T) {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "mon", Name: "dash",
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "helm"}},
			Annotations:   map[string]string{lastApplied: `{"big":"json"}`, "keep": "me"}},
		Data:       map[string]string{"small": "db-host", "big": "0123456789abcdef"},
		BinaryData: map[string][]byte{"logo.png": {1, 2, 3}},
	}
	f := newFixture(t, nil, []runtime.Object{cm})
	f.start(t)
	obj, err := f.c.Get(ConfigMaps, "mon", "dash")
	if err != nil {
		t.Fatal(err)
	}
	got := obj.(*corev1.ConfigMap)
	if len(got.ManagedFields) != 0 || got.Annotations[lastApplied] != "" || got.Annotations["keep"] != "me" {
		t.Errorf("metadata = %+v", got.ObjectMeta)
	}
	if got.Data["small"] != "db-host" || got.Data["big"] != "01234567" || got.BinaryData != nil {
		t.Errorf("data = %v, binary = %v", got.Data, got.BinaryData)
	}
	if !strings.Contains(got.Annotations[TruncatedAnnotation], "2 values") {
		t.Errorf("truncated annotation = %q", got.Annotations[TruncatedAnnotation])
	}
	// Small ConfigMaps are left alone.
	small := &corev1.ConfigMap{Data: map[string]string{"a": "b"}}
	truncateConfigMap(small, 8)
	if small.Annotations != nil {
		t.Error("small ConfigMap was marked")
	}
	// Non-objects pass through.
	if out, err := transform(8)("tombstone"); err != nil || out != "tombstone" {
		t.Errorf("tombstone = %v, %v", out, err)
	}
}

func TestGenerationAndWatchUpdates(t *testing.T) {
	f := newFixture(t, nil, []runtime.Object{pod("a", "p1", nil)})
	ctx := f.start(t)
	g0 := f.c.Generation()
	if g0 == 0 {
		t.Error("initial list must bump the generation")
	}
	if _, err := f.kube.CoreV1().Pods("a").Create(ctx, pod("a", "p2", nil), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, _ := f.c.List(Pods, "a", nil)
		if len(l) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("watch did not deliver the new pod; have %d", len(l))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if f.c.Generation() <= g0 {
		t.Error("generation did not grow after an add")
	}
}

func TestWarningEventsOnly(t *testing.T) {
	f := newFixture(t, nil, nil)
	var selector string
	f.dyn.PrependReactor("list", "events", func(a clienttesting.Action) (bool, runtime.Object, error) {
		selector = a.(clienttesting.ListActionImpl).GetListRestrictions().Fields.String()
		return false, nil, nil
	})
	f.start(t)
	if selector != "type=Warning" {
		t.Errorf("event access check selector = %q", selector)
	}
}

// review-km-23 B1: a resource whose first list finishes after WaitForSync
// gave up must become usable, without a restart.
func TestLateSyncIsServed(t *testing.T) {
	f := newFixture(t, nil, []runtime.Object{pod("a", "p1", nil)})
	// The pod list fails until released: an API server too slow or too busy
	// to answer in time. (A blocking reactor would stall the whole fake
	// clientset, which holds one lock for all reactors.)
	var released atomic.Bool
	f.kube.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		if !released.Load() {
			return true, nil, apierrors.NewTimeoutError("list pods", 1)
		}
		return false, nil, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := f.c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	wctx, wcancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer wcancel()
	if f.c.WaitForSync(wctx) {
		t.Fatal("WaitForSync must report that pods did not sync")
	}
	if _, err := f.c.List(Pods, "", nil); !errors.Is(err, ErrNotCached) {
		t.Fatalf("before the list finishes: err = %v", err)
	}
	if st := status(f.c, Pods); st.State != StateSyncing {
		t.Errorf("pods state = %s, want syncing", st.State)
	}
	if _, err := f.c.List(Deployments, "", nil); err != nil {
		t.Errorf("synced resources must be served meanwhile: %v", err)
	}
	released.Store(true)
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, err := f.c.List(Pods, "", nil)
		if err == nil && len(l) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pods never became usable after the late sync: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st := status(f.c, Pods); st.State != StateSynced || st.Count != 1 {
		t.Errorf("pods status = %+v", st)
	}
}

// review-km-23 N1: a watch error shows in Status until the watch delivers
// again.
func TestWatchErrorClearedByNextEvent(t *testing.T) {
	f := newFixture(t, nil, nil)
	f.start(t)
	f.c.mu.RLock()
	e := f.c.entries[Pods]
	f.c.mu.RUnlock()
	f.c.watchError(e, errors.New("watch closed: connection reset"))
	if st := status(f.c, Pods); st.LastError != "watch closed: connection reset" {
		t.Fatalf("after error: %+v", st)
	}
	g := f.c.Generation()
	f.c.changed(e)
	if st := status(f.c, Pods); st.LastError != "" {
		t.Errorf("after an event: %+v", st)
	}
	if f.c.Generation() != g+1 {
		t.Error("an event must bump the generation")
	}
}
