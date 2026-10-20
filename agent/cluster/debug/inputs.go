package debug

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/agent/cluster/deps"
	"kubemap/agent/cluster/graph"
	"kubemap/agent/cluster/netpol"
	"kubemap/agent/cluster/svccheck"
	"kubemap/pkg/protocol"
)

var (
	podsGVR  = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	nodesGVR = schema.GroupVersionResource{Version: "v1", Resource: "nodes"}
)

// FromCache fills the inputs from the agent's cache around a diagnosis
// (pod.diagnose). What the cache cannot give is left nil and noted.
func FromCache(src graph.Source, g *graph.Graph, pod *corev1.Pod, d *protocol.PodDiagnosis) Inputs {
	in := Inputs{Diagnosis: d, Pod: pod, Graph: g, Deps: deps.Find(src)}
	if objs, err := src.List(podsGVR, "", nil); err == nil {
		for _, o := range objs {
			in.Pods = append(in.Pods, o.(*corev1.Pod))
		}
	}
	if objs, err := src.List(nodesGVR, "", nil); err == nil {
		for _, o := range objs {
			in.Nodes = append(in.Nodes, o.(*corev1.Node))
		}
	}
	if s, err := svccheck.Load(src); err == nil {
		in.Services = s
	} else {
		d.Notes = append(d.Notes, "service checks not available: "+err.Error())
	}
	if s, err := netpol.Load(src); err == nil {
		in.Netpol = s
	} else {
		d.Notes = append(d.Notes, "NetworkPolicy checks not available: "+err.Error())
	}
	return in
}
