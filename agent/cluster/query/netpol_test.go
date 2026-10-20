package query

import (
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/pkg/protocol"
)

func TestNetpolCheckOp(t *testing.T) {
	gPol := gvr("networking.k8s.io", "v1", "networkpolicies")
	gNS := gvr("", "v1", "namespaces")
	web := pod("shop", "web", "n1", map[string]string{"app": "web"})
	web.Status.PodIP = "10.0.0.1"
	db := pod("shop", "db", "n1", map[string]string{"app": "db"})
	db.Status.PodIP = "10.0.0.2"
	objs := map[schema.GroupVersionResource][]runtime.Object{
		gPol: {&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "default-deny"},
			Spec: networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}}}},
		gNS:   {&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}},
		gPods: {web, db},
	}
	h, _, _ := newHandler(t, objs)
	check := &protocol.NetpolCheckRequest{From: protocol.NetpolEndpoint{Namespace: "shop", Pod: "web"},
		To: protocol.NetpolEndpoint{Namespace: "shop", Pod: "db"}, Port: 5432}
	r := do(h, &protocol.Request{Op: protocol.OpNetpolCheck, NetpolCheck: check})
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	var res protocol.NetpolResult
	if err := json.Unmarshal(r.Object, &res); err != nil {
		t.Fatal(err)
	}
	if res.Allowed || !strings.Contains(res.Summary, "shop/default-deny") {
		t.Errorf("result = %+v", res)
	}
	// Unknown pod → not_found; a bad request never reaches the engine.
	check.To.Pod = "nope"
	if r := do(h, &protocol.Request{Op: protocol.OpNetpolCheck, NetpolCheck: check}); code(r) != protocol.CodeNotFound {
		t.Errorf("unknown pod: %+v", r.Error)
	}
	if r := do(h, &protocol.Request{Op: protocol.OpNetpolCheck}); code(r) != protocol.CodeBadRequest {
		t.Errorf("no netpolCheck: %+v", r.Error)
	}
	// Without the policies in the cache, the answer would be a guess.
	delete(objs, gPol)
	check.To.Pod = "db"
	if r := do(h, &protocol.Request{Op: protocol.OpNetpolCheck, NetpolCheck: check}); code(r) != protocol.CodeUnavailable {
		t.Errorf("no policies cached: %+v", r.Error)
	}
}
