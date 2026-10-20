package chain

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"kubemap/agent/cluster/apps"
	"kubemap/agent/cluster/deps"
	"kubemap/agent/cluster/graph"
	"kubemap/agent/cluster/netpol"
	"kubemap/agent/cluster/routes"
	"kubemap/pkg/protocol"
)

// shop is url-shortener in miniature: a Deployment with a ReplicaSet and
// two pods, a Service and a route to it, a ConfigMap, a Secret name, a claim
// and its volume, a ServiceAccount, a node, an HPA, a dependency on redis
// and a caller, and a NetworkPolicy.
func shop(t *testing.T) Inputs {
	t.Helper()
	node := func(id, status string, missing bool, lbl map[string]string) map[string]any {
		p := strings.SplitN(id, "/", 3)
		return map[string]any{"id": id, "kind": p[0], "namespace": p[1], "name": p[2], "status": status, "missing": missing, "labels": lbl}
	}
	app := map[string]string{"app": "web"}
	nodes := []map[string]any{
		node("Deployment/shop/web", "2/2 ready", false, nil),
		node("ReplicaSet/shop/web-1", "2/2 ready", false, nil),
		node("Pod/shop/web-1-a", "Running", false, app),
		node("Pod/shop/web-1-b", "CrashLoopBackOff", false, app),
		node("Service/shop/web", "", false, nil),
		node("ConfigMap/shop/web-config", "", false, nil),
		node("Secret/shop/web-db", "", true, nil),
		node("PersistentVolumeClaim/shop/data", "Bound", false, nil),
		node("PersistentVolume//pv-1", "Bound", false, nil),
		node("ServiceAccount/shop/web", "", false, nil),
		node("Node//k8server", "Ready", false, nil),
		node("HorizontalPodAutoscaler/shop/web", "", false, nil),
		node("Service/shop/redis", "", false, nil),
		node("Deployment/shop/worker", "1/1 ready", false, nil),
		node("IngressRoute.traefik.io/shop/web", "", false, nil),
	}
	e := func(from, to string, typ graph.EdgeType, detail string) map[string]any {
		return map[string]any{"from": from, "to": to, "type": typ, "detail": detail}
	}
	edges := []map[string]any{
		e("Deployment/shop/web", "ReplicaSet/shop/web-1", graph.EdgeOwns, ""),
		e("ReplicaSet/shop/web-1", "Pod/shop/web-1-a", graph.EdgeOwns, ""),
		e("ReplicaSet/shop/web-1", "Pod/shop/web-1-b", graph.EdgeOwns, ""),
		e("Service/shop/web", "Pod/shop/web-1-a", graph.EdgeSelects, ""),
		e("Service/shop/web", "Pod/shop/web-1-b", graph.EdgeSelects, ""),
		e("HorizontalPodAutoscaler/shop/web", "Deployment/shop/web", graph.EdgeScales, ""),
	}
	for _, p := range []string{"Pod/shop/web-1-a", "Pod/shop/web-1-b"} {
		edges = append(edges,
			e(p, "ConfigMap/shop/web-config", graph.EdgeConfig, "env DB_HOST"),
			e(p, "Secret/shop/web-db", graph.EdgeSecretRef, "env DB_PASSWORD"),
			e(p, "PersistentVolumeClaim/shop/data", graph.EdgeMounts, "volume data"),
			e(p, "ServiceAccount/shop/web", graph.EdgeRunsAs, ""),
			e(p, "Node//k8server", graph.EdgeRunsOn, ""))
	}
	edges = append(edges, e("PersistentVolumeClaim/shop/data", "PersistentVolume//pv-1", graph.EdgeBinds, ""))
	raw, _ := json.Marshal(map[string]any{"nodes": nodes, "edges": edges})
	var g graph.Graph
	if err := json.Unmarshal(raw, &g); err != nil {
		t.Fatal(err)
	}
	pod := func(name string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name, Labels: app}}
	}
	pol := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-ingress"},
		Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: app},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress}}}
	other := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "redis-only"},
		Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "redis"}}}}
	return Inputs{
		Graph: &g,
		Routes: []routes.Route{{Source: "IngressRoute.traefik.io/shop/web", Host: "shop.example.com", Path: "/", PathKind: "PathPrefix",
			Service: "Service/shop/web", Port: "80"}},
		Deps: []deps.Dependency{
			{From: "Deployment/shop/web", To: "Service/shop/redis", Via: "env REDIS_HOST"},
			{From: "Deployment/shop/worker", To: "Service/shop/web", Via: "env WEB_URL"},
		},
		Apps:   []apps.App{{ID: "shop/web", Members: []string{"Deployment/shop/web", "Service/shop/web"}}},
		Netpol: &netpol.Snapshot{Policies: []*networkingv1.NetworkPolicy{pol, other}, Pods: []*corev1.Pod{pod("web-1-a"), pod("web-1-b")}},
	}
}

// summary is "section: id (detail), …" per section, for readable asserts.
func summary(c *protocol.ObjectChain) map[string]string {
	out := map[string]string{}
	for _, s := range c.Sections {
		var parts []string
		for _, it := range s.Items {
			p := it.ID
			if it.Detail != "" {
				p += " (" + it.Detail + ")"
			}
			if it.Missing {
				p += " MISSING"
			}
			parts = append(parts, p)
		}
		if s.More > 0 {
			parts = append(parts, fmt.Sprintf("+%d", s.More))
		}
		out[s.Key] = strings.Join(parts, ", ")
	}
	return out
}

func build(t *testing.T, in Inputs, id string) *protocol.ObjectChain {
	t.Helper()
	c, ok := Build(in, id)
	if !ok {
		t.Fatalf("%s: not found", id)
	}
	return c
}

func TestDeploymentChain(t *testing.T) {
	c := build(t, shop(t), "Deployment/shop/web")
	got := summary(c)
	want := map[string]string{
		"owned":           "ReplicaSet/shop/web-1",
		"pods":            "Pod/shop/web-1-a, Pod/shop/web-1-b",
		"services":        "Service/shop/web",
		"routes":          "IngressRoute.traefik.io/shop/web (shop.example.com/ → shop/web:80)",
		"config":          "ConfigMap/shop/web-config (env DB_HOST)",
		"secrets":         "Secret/shop/web-db (env DB_PASSWORD) MISSING",
		"volumes":         "PersistentVolumeClaim/shop/data (volume data), PersistentVolume//pv-1 (bound to the claim)",
		"serviceaccounts": "ServiceAccount/shop/web",
		"policies":        "NetworkPolicy/shop/web-ingress (isolates ingress and egress)",
		"autoscalers":     "HorizontalPodAutoscaler/shop/web",
		"nodes":           "Node//k8server",
		"calls":           "Service/shop/redis (env REDIS_HOST)",
		"calledBy":        "Deployment/shop/worker (env WEB_URL)",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s:\n got %s\nwant %s", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("sections %v", got)
	}
	if c.Object.ID != "Deployment/shop/web" || c.Object.Status != "2/2 ready" || strings.Join(c.Apps, ",") != "shop/web" {
		t.Errorf("object %+v apps %v", c.Object, c.Apps)
	}
	// Sections come in the fixed order.
	var keys []string
	for _, s := range c.Sections {
		keys = append(keys, s.Key)
	}
	if strings.Join(keys, ",") != "owned,pods,services,routes,config,secrets,volumes,serviceaccounts,policies,autoscalers,nodes,calls,calledBy" {
		t.Errorf("order %v", keys)
	}
}

// Every kind of object leads to the same pods, and never lists itself.
func TestChainFromEveryKind(t *testing.T) {
	in := shop(t)
	for id, check := range map[string]func(map[string]string) bool{
		"Pod/shop/web-1-b": func(s map[string]string) bool {
			return s["owners"] == "Deployment/shop/web, ReplicaSet/shop/web-1" && s["pods"] == "" && s["services"] == "Service/shop/web"
		},
		"Service/shop/web": func(s map[string]string) bool {
			return s["pods"] == "Pod/shop/web-1-a, Pod/shop/web-1-b" && s["services"] == "" && strings.HasPrefix(s["routes"], "IngressRoute") &&
				s["calledBy"] == "Deployment/shop/worker (env WEB_URL)"
		},
		"ConfigMap/shop/web-config": func(s map[string]string) bool {
			return s["pods"] == "Pod/shop/web-1-a, Pod/shop/web-1-b" && s["config"] == "" && s["services"] == "Service/shop/web"
		},
		"PersistentVolume//pv-1": func(s map[string]string) bool {
			return strings.Contains(s["volumes"], "PersistentVolumeClaim/shop/data") && !strings.Contains(s["volumes"], "pv-1") && s["pods"] != ""
		},
		"HorizontalPodAutoscaler/shop/web": func(s map[string]string) bool {
			return strings.Contains(s["owned"], "Deployment/shop/web (scales it)") && s["pods"] == "Pod/shop/web-1-a, Pod/shop/web-1-b"
		},
		"IngressRoute.traefik.io/shop/web": func(s map[string]string) bool {
			return s["services"] == "Service/shop/web" && s["pods"] != "" && s["routes"] == ""
		},
		"Node//k8server": func(s map[string]string) bool {
			return s["pods"] == "Pod/shop/web-1-a, Pod/shop/web-1-b" && s["nodes"] == ""
		},
		"NetworkPolicy/shop/web-ingress": func(s map[string]string) bool {
			return s["pods"] == "Pod/shop/web-1-a, Pod/shop/web-1-b" && s["policies"] == ""
		},
	} {
		c := build(t, in, id)
		if s := summary(c); !check(s) {
			t.Errorf("%s: %v", id, s)
		}
		for _, sec := range c.Sections {
			for _, it := range sec.Items {
				if it.ID == id {
					t.Errorf("%s lists itself in %s", id, sec.Key)
				}
			}
		}
	}
	// A policy that selects no pod has an empty chain; an unknown object
	// is not found.
	if c := build(t, in, "NetworkPolicy/shop/redis-only"); len(c.Sections) != 0 {
		t.Errorf("redis-only: %v", summary(c))
	}
	if _, ok := Build(in, "Deployment/shop/nope"); ok {
		t.Error("unknown object found")
	}
}

func TestNotesAndLimits(t *testing.T) {
	in := shop(t)
	in.Netpol = nil
	c := build(t, in, "Deployment/shop/web")
	if len(c.Notes) != 1 || !strings.Contains(c.Notes[0], "NetworkPolicies were not read") {
		t.Errorf("notes %v", c.Notes)
	}
	// A Service without pods says why.
	in = shop(t)
	c = build(t, in, "Service/shop/redis")
	if len(c.Notes) != 1 || !strings.Contains(c.Notes[0], "no endpoints") {
		t.Errorf("redis notes %v", c.Notes)
	}
	// Sections are capped; the rest is counted.
	var b builder
	b.g, b.sections = in.Graph, map[string]*section{}
	for i := range protocol.MaxChainItems + 7 {
		b.addItem(protocol.ChainPods, protocol.ChainItem{ID: fmt.Sprintf("Pod/x/p-%d", i)})
	}
	if l := b.list(); len(l) != 1 || len(l[0].Items) != protocol.MaxChainItems || l[0].More != 7 {
		t.Errorf("cap: %+v", l)
	}
}

func TestFind(t *testing.T) {
	in := shop(t)
	var ids []string
	for _, m := range Find(in.Graph, in.Netpol, "WEB") {
		ids = append(ids, m.ID)
	}
	// Exact name first, then names that start with it, shorter first; the
	// NetworkPolicy from the snapshot is found too.
	if len(ids) < 4 || ids[0] != "Service/shop/web" || !strings.Contains(strings.Join(ids, ","), "NetworkPolicy/shop/web-ingress") {
		t.Errorf("find web = %v", ids)
	}
	if got := Find(in.Graph, nil, "zzz"); len(got) != 0 {
		t.Errorf("find zzz = %v", got)
	}
}
