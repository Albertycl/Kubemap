package query

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/agent/cluster/traces"
	"kubemap/pkg/protocol"
)

// KM-54: the appmap op asks Tempo when there is one, and never fails the
// map because of it.
func TestAppMapTraces(t *testing.T) {
	yes := true
	objs := map[schema.GroupVersionResource][]runtime.Object{
		gPods: {&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-1",
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-7d9", Controller: &yes}}},
			Status: corev1.PodStatus{PodIP: "10.0.0.5"}}},
		gvr("apps", "v1", "replicasets"): {&appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web-7d9",
			OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web", Controller: &yes}}}}},
		gvr("", "v1", "nodes"):    {},
		gvr("", "v1", "services"): {},
		gDeploy:                   {},
	}
	h, _, _ := newHandler(t, objs)
	get := func() *protocol.AppMap {
		t.Helper()
		r := do(h, &protocol.Request{Op: protocol.OpAppMap})
		if r.Error != nil {
			t.Fatalf("appmap: %+v", r.Error)
		}
		var m protocol.AppMap
		json.Unmarshal(r.Object, &m)
		return &m
	}
	// No Tempo in the cache: no summary at all.
	h.Traces = traces.NewSource("")
	if m := get(); m.Traces != nil {
		t.Errorf("no tempo: %+v", m.Traces)
	}
	// "off" disables even a configured one.
	h.Traces = traces.NewSource("off")
	if m := get(); m.Traces != nil {
		t.Errorf("off: %+v", m.Traces)
	}
	// Tempo down: the map still comes, and says why there are no
	// confirmations.
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer down.Close()
	h.Traces = traces.NewSource(down.URL)
	if m := get(); m.Traces == nil || !strings.Contains(m.Traces.Error, "503") || m.Traces.Source != "tempo "+down.URL {
		t.Errorf("tempo down: %+v", m.Traces)
	}
	// A working Tempo with only probes: a summary, nothing confirmed.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("q"), "kind = server") {
			w.Write([]byte(`{"traces":[{"spanSets":[{"spans":[{"spanID":"a","startTimeUnixNano":"1790590926276337612","attributes":[` +
				`{"key":"kind","value":{"stringValue":"server"}},{"key":"service.name","value":{"stringValue":"web"}},` +
				`{"key":"http.user_agent","value":{"stringValue":"kube-probe/1.34"}}]}]}]}]}`))
			return
		}
		w.Write([]byte(`{"traces":[]}`))
	}))
	defer up.Close()
	h.Traces = traces.NewSource(up.URL)
	m := get()
	if m.Traces == nil || m.Traces.Error != "" || m.Traces.Spans != 1 || m.Traces.Probes != 1 || m.Traces.Window != "1h0m0s" {
		t.Errorf("tempo up: %+v", m.Traces)
	}
}
