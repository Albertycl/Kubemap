package query

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/agent/cluster/graph"
	"kubemap/pkg/protocol"
)

// twoApps has url-shortener (with its Redis) in sre-lab-app and a lone app
// in another namespace.
func twoApps() map[schema.GroupVersionResource][]runtime.Object {
	i32 := func(v int32) *int32 { return &v }
	dep := func(ns, name string, lbl map[string]string, env ...corev1.EnvVar) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: lbl},
			Spec: appsv1.DeploymentSpec{Replicas: i32(1), Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "c", Image: name + ":1", Env: env}}}}},
			Status: appsv1.DeploymentStatus{ReadyReplicas: 1}}
	}
	own := func(kind, name string) []metav1.OwnerReference {
		return []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: kind, Name: name}}
	}
	pod := func(ns, name, owner string, lbl map[string]string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: lbl, OwnerReferences: own("ReplicaSet", owner)},
			Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	}
	us := map[string]string{"app": "url-shortener"}
	rd := map[string]string{"app": "redis"}
	return map[schema.GroupVersionResource][]runtime.Object{
		gDeploy: {
			dep("sre-lab-app", "url-shortener", us, corev1.EnvVar{Name: "REDIS_HOST", Value: "redis"}),
			dep("sre-lab-app", "redis", rd),
			dep("other", "lonely", map[string]string{"app": "lonely"}),
		},
		gvr("apps", "v1", "replicasets"): {
			&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "sre-lab-app", Name: "us-rs", OwnerReferences: own("Deployment", "url-shortener")}},
			&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "sre-lab-app", Name: "rd-rs", OwnerReferences: own("Deployment", "redis")}},
			&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "l-rs", OwnerReferences: own("Deployment", "lonely")}},
		},
		gPods: {
			pod("sre-lab-app", "us-1", "us-rs", us), pod("sre-lab-app", "rd-1", "rd-rs", rd), pod("other", "l-1", "l-rs", map[string]string{"app": "lonely"}),
		},
		gvr("", "v1", "services"): {
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "sre-lab-app", Name: "redis"}, Spec: corev1.ServiceSpec{Selector: rd}},
		},
		gvr("", "v1", "serviceaccounts"): {
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "sre-lab-app", Name: "default"}},
			&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "default"}},
		},
	}
}

func appMap(t *testing.T, h *Handler, req *protocol.Request) AppMap {
	t.Helper()
	req.Op = protocol.OpGraph
	r := do(h, req)
	if r.Error != nil {
		t.Fatalf("graph: %v", r.Error)
	}
	var m AppMap
	if err := json.Unmarshal(r.Object, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestGraphWhole(t *testing.T) {
	h, _, _ := newHandler(t, twoApps())
	m := appMap(t, h, &protocol.Request{})
	if len(m.Apps) != 3 || len(m.Links) != 1 || m.Graph == nil || len(m.Graph.Nodes) == 0 {
		t.Fatalf("map: %d apps, %d links", len(m.Apps), len(m.Links))
	}
	if l := m.Links[0]; l.From != "sre-lab-app/url-shortener" || l.To != "sre-lab-app/redis" {
		t.Errorf("link = %+v", l)
	}
	if len(m.Datastores) != 1 || m.Datastores[0].Engine != "redis" || len(m.Datastores[0].Clients) != 1 {
		t.Errorf("datastores = %+v", m.Datastores)
	}
}

func TestGraphNamespaceAndAround(t *testing.T) {
	src := twoApps()
	h, _, _ := newHandler(t, src)
	h.Graph = graph.NewBuilder(h.Cache)
	m := appMap(t, h, &protocol.Request{Namespace: "other"})
	if len(m.Apps) != 1 || m.Apps[0].ID != "other/lonely" || len(m.Links) != 0 || len(m.Datastores) != 0 {
		t.Errorf("namespace map = %+v", m.Apps)
	}
	for _, n := range m.Graph.Nodes {
		if n.Namespace != "other" && n.Namespace != "" {
			t.Errorf("node from another namespace: %s", n.ID)
		}
	}
	m = appMap(t, h, &protocol.Request{Graph: &protocol.GraphOptions{Around: "Service/sre-lab-app/redis", Depth: 1}})
	if len(m.Apps) != 1 || m.Apps[0].ID != "sre-lab-app/redis" {
		t.Errorf("around apps = %+v", m.Apps)
	}
	if _, ok := m.Graph.Node("Pod/sre-lab-app/rd-1"); !ok {
		t.Error("around graph lacks the selected pod")
	}
	if r := do(h, &protocol.Request{Op: protocol.OpGraph, Graph: &protocol.GraphOptions{Around: "Pod/x/none"}}); code(r) != protocol.CodeNotFound {
		t.Errorf("unknown around: %+v", r.Error)
	}
}

func drawMap(t *testing.T, h *Handler, req *protocol.Request) protocol.AppMap {
	t.Helper()
	req.Op = protocol.OpAppMap
	r := do(h, req)
	if r.Error != nil {
		t.Fatalf("appmap: %v", r.Error)
	}
	dec := json.NewDecoder(bytes.NewReader(r.Object))
	dec.DisallowUnknownFields()
	var m protocol.AppMap
	if err := dec.Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAppMapOp(t *testing.T) {
	h, _, _ := newHandler(t, twoApps())
	m := drawMap(t, h, &protocol.Request{})
	if len(m.Apps) != 3 || len(m.Links) != 1 {
		t.Fatalf("map: %d apps, %d links", len(m.Apps), len(m.Links))
	}
	l := m.Links[0]
	if l.From != "Deployment/sre-lab-app/url-shortener" || l.To != "Deployment/sre-lab-app/redis" ||
		l.FromApp != "sre-lab-app/url-shortener" || l.ToApp != "sre-lab-app/redis" ||
		len(l.Evidence) != 1 || l.Evidence[0].Type != protocol.EvidenceEnv || l.Evidence[0].Name != "REDIS_HOST" {
		t.Errorf("link = %+v", l)
	}
	for _, a := range m.Apps {
		if a.ID != "sre-lab-app/redis" {
			continue
		}
		c := a.Components[0]
		// Pods come from the cache: node and readiness from the pod object.
		if c.Tier != protocol.TierCache || c.Engine != "redis" || len(c.Pods) != 1 || c.Pods[0].Name != "rd-1" {
			t.Errorf("redis = %+v", c)
		}
	}

	m = drawMap(t, h, &protocol.Request{Namespace: "other"})
	if len(m.Apps) != 1 || m.Apps[0].ID != "other/lonely" || len(m.Links) != 0 || len(m.Peers) != 0 {
		t.Errorf("namespace map = %+v", m)
	}
	m = drawMap(t, h, &protocol.Request{AppMap: &protocol.AppMapOptions{App: "sre-lab-app/redis"}})
	if len(m.Apps) != 1 || len(m.Links) != 1 || len(m.Peers) != 1 || m.Peers[0].ID != "Deployment/sre-lab-app/url-shortener" {
		t.Errorf("app map = %+v", m)
	}
	r := do(h, &protocol.Request{Op: protocol.OpAppMap, AppMap: &protocol.AppMapOptions{App: "sre-lab-app/none"}})
	if code(r) != protocol.CodeNotFound {
		t.Errorf("unknown app: %+v", r.Error)
	}
	r = do(h, &protocol.Request{Op: protocol.OpAppMap, AppMap: &protocol.AppMapOptions{App: "bad id"}})
	if code(r) != protocol.CodeBadRequest {
		t.Errorf("bad app id: %+v", r.Error)
	}
}

func TestAppMapTooLarge(t *testing.T) {
	src := twoApps()
	// Enough apps with long names to pass one message.
	for i := range 3000 {
		name := fmt.Sprintf("app-%04d-%s", i, strings.Repeat("x", 200))
		src[gDeploy] = append(src[gDeploy], &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "bulk", Name: name}})
	}
	h, _, _ := newHandler(t, src)
	r := do(h, &protocol.Request{Op: protocol.OpAppMap})
	if code(r) != protocol.CodeTooLarge || !strings.Contains(r.Error.Message, "one namespace or one app") {
		t.Errorf("too large: %+v", r.Error)
	}
}
