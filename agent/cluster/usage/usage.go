// Package usage turns the metrics API's PodMetrics (pods.metrics.k8s.io,
// served by metrics-server) into per-pod resource usage (KM-28): CPU and
// memory working set per pod and per container, next to the requests and
// limits from the pod spec. The cluster agent reads PodMetrics with get and
// list only and makes no kubelet calls.
package usage

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/pkg/protocol"
)

// PodMetricsGVR is the metrics API resource the agent reads.
var PodMetricsGVR = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}

// PodMetrics is the part of metrics.k8s.io/v1beta1 PodMetrics the agent uses.
type PodMetrics struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Timestamp  metav1.Time     `json:"timestamp"`
	Window     metav1.Duration `json:"window"`
	Containers []struct {
		Name  string              `json:"name"`
		Usage corev1.ResourceList `json:"usage"`
	} `json:"containers"`
}

// Parse converts PodMetrics objects read with the dynamic client.
func Parse(items []unstructured.Unstructured) ([]PodMetrics, error) {
	out := make([]PodMetrics, 0, len(items))
	for i := range items {
		b, err := json.Marshal(items[i].Object)
		if err != nil {
			return nil, err
		}
		var pm PodMetrics
		if err := json.Unmarshal(b, &pm); err != nil {
			return nil, fmt.Errorf("PodMetrics %s/%s: %w", items[i].GetNamespace(), items[i].GetName(), err)
		}
		out = append(out, pm)
	}
	return out, nil
}

// Report builds the usage of the pods in metrics. Specs gives each pod's
// spec for its node, requests and limits, keyed "namespace/name"; a pod
// without a spec (just deleted) is reported without them. Running pods in
// specs that have no metrics yet are counted in a note.
func Report(metrics []PodMetrics, specs map[string]*corev1.Pod, now time.Time, notes []string) *protocol.PodUsageReport {
	out := &protocol.PodUsageReport{Time: now.UTC(), Pods: []protocol.PodUsage{}, Notes: notes}
	seen := map[string]bool{}
	for i := range metrics {
		pm := &metrics[i]
		key := pm.Metadata.Namespace + "/" + pm.Metadata.Name
		seen[key] = true
		out.Pods = append(out.Pods, pod(pm, specs[key]))
	}
	missing := 0
	for key, p := range specs {
		if !seen[key] && p.Status.Phase == corev1.PodRunning {
			missing++
		}
	}
	if missing > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("%d running pods have no metrics yet (metrics-server scrapes every 15 to 60 s, and skips containers that are restarting)", missing))
	}
	sort.Slice(out.Pods, func(a, b int) bool {
		pa, pb := out.Pods[a], out.Pods[b]
		if pa.MemoryBytes != pb.MemoryBytes {
			return pa.MemoryBytes > pb.MemoryBytes
		}
		return pa.Namespace+"/"+pa.Name < pb.Namespace+"/"+pb.Name
	})
	if len(out.Pods) > protocol.MaxUsagePods {
		out.Notes = append(out.Notes, fmt.Sprintf("%d smaller pods not shown (at most %d); ask for one namespace", len(out.Pods)-protocol.MaxUsagePods, protocol.MaxUsagePods))
		out.Pods = out.Pods[:protocol.MaxUsagePods]
	}
	return out
}

func pod(pm *PodMetrics, spec *corev1.Pod) protocol.PodUsage {
	pu := protocol.PodUsage{Namespace: pm.Metadata.Namespace, Name: pm.Metadata.Name,
		SampledAt: pm.Timestamp.UTC(), WindowSeconds: pm.Window.Seconds(), Containers: []protocol.ContainerUsage{}}
	if pm.Timestamp.IsZero() {
		pu.SampledAt = time.Time{}
	}
	res := map[string]corev1.ResourceRequirements{}
	var order []string
	if spec != nil {
		pu.Node = spec.Spec.NodeName
		for _, c := range spec.Spec.Containers {
			res[c.Name] = c.Resources
			order = append(order, c.Name)
		}
	}
	for _, cm := range pm.Containers {
		cu := protocol.ContainerUsage{Name: cm.Name, CPUMillis: milli(cm.Usage[corev1.ResourceCPU]), MemoryBytes: bytes(cm.Usage[corev1.ResourceMemory])}
		if r, ok := res[cm.Name]; ok {
			cu.CPURequestMillis = r.Requests.Cpu().MilliValue()
			cu.CPULimitMillis = r.Limits.Cpu().MilliValue()
			cu.MemoryRequestBytes = r.Requests.Memory().Value()
			cu.MemoryLimitBytes = r.Limits.Memory().Value()
			cu.CPUOfRequest = share(cu.CPUMillis, cu.CPURequestMillis)
			cu.CPUOfLimit = share(cu.CPUMillis, cu.CPULimitMillis)
			cu.MemoryOfRequest = share(cu.MemoryBytes, cu.MemoryRequestBytes)
			cu.MemoryOfLimit = share(cu.MemoryBytes, cu.MemoryLimitBytes)
		}
		pu.CPUMillis += cu.CPUMillis
		pu.MemoryBytes += cu.MemoryBytes
		pu.Containers = append(pu.Containers, cu)
	}
	// Spec order when known (what kubectl shows), else by name.
	rank := func(name string) int {
		for k, n := range order {
			if n == name {
				return k
			}
		}
		return len(order)
	}
	sort.SliceStable(pu.Containers, func(a, b int) bool {
		ra, rb := rank(pu.Containers[a].Name), rank(pu.Containers[b].Name)
		if ra != rb {
			return ra < rb
		}
		return pu.Containers[a].Name < pu.Containers[b].Name
	})
	return pu
}

func milli(q resource.Quantity) int64 { return q.MilliValue() }
func bytes(q resource.Quantity) int64 { return q.Value() }

func share(used, of int64) float64 {
	if of <= 0 {
		return 0
	}
	return float64(used) * 100 / float64(of)
}
