package query

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/pkg/protocol"
)

func TestServiceCheckOp(t *testing.T) {
	gSvc := gvr("", "v1", "services")
	objs := map[schema.GroupVersionResource][]runtime.Object{
		gSvc: {&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "catalog"},
			Spec: corev1.ServiceSpec{Selector: map[string]string{"app": "catalog-api"}, Ports: []corev1.ServicePort{{Port: 80}}}}},
		gPods: {pod("shop", "catalog-1", "n1", map[string]string{"app": "catalog"})},
	}
	h, _, _ := newHandler(t, objs)
	r := do(h, &protocol.Request{Op: protocol.OpServiceCheck, Namespace: "shop"})
	if r.Error != nil {
		t.Fatal(r.Error)
	}
	var res protocol.ServiceCheckResult
	if err := json.Unmarshal(r.Object, &res); err != nil {
		t.Fatal(err)
	}
	if res.Problems != 1 || res.Services[0].Findings[0].Problem != "selector matches no pods" || res.Services[0].ReadyEndpoints != -1 {
		t.Errorf("result = %+v", res)
	}
	if r := do(h, &protocol.Request{Op: protocol.OpServiceCheck, Namespace: "shop", Name: "nope"}); code(r) != protocol.CodeNotFound {
		t.Errorf("unknown Service: %+v", r.Error)
	}
	if r := do(h, &protocol.Request{Op: protocol.OpServiceCheck, Name: "catalog"}); code(r) != protocol.CodeBadRequest {
		t.Errorf("name without namespace: %+v", r.Error)
	}
	delete(objs, gSvc)
	if r := do(h, &protocol.Request{Op: protocol.OpServiceCheck}); code(r) != protocol.CodeUnavailable {
		t.Errorf("no Services cached: %+v", r.Error)
	}
}
