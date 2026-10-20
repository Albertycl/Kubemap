package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"kubemap/pkg/protocol"
)

var now = time.Date(2026, 9, 28, 10, 13, 0, 0, time.UTC)

// The fixture is the real metrics API answer for sre-lab-app on k8server,
// trimmed to flask-vue-backend and redis-master.
func fixture(t *testing.T) []PodMetrics {
	b, err := os.ReadFile("testdata/podmetrics-sre-lab-app.json")
	if err != nil {
		t.Fatal(err)
	}
	var list unstructured.UnstructuredList
	if err := list.UnmarshalJSON(b); err != nil {
		t.Fatal(err)
	}
	pms, err := Parse(list.Items)
	if err != nil {
		t.Fatal(err)
	}
	return pms
}

func spec(ns, name, node string, phase corev1.PodPhase, containers ...corev1.Container) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: corev1.PodSpec{NodeName: node, Containers: containers}, Status: corev1.PodStatus{Phase: phase}}
}

func limits(name, reqCPU, limCPU, reqMem, limMem string) corev1.Container {
	c := corev1.Container{Name: name, Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{}, Limits: corev1.ResourceList{}}}
	for k, v := range map[corev1.ResourceName][2]string{corev1.ResourceCPU: {reqCPU, limCPU}, corev1.ResourceMemory: {reqMem, limMem}} {
		if v[0] != "" {
			c.Resources.Requests[k] = resource.MustParse(v[0])
		}
		if v[1] != "" {
			c.Resources.Limits[k] = resource.MustParse(v[1])
		}
	}
	return c
}

func TestReportFromRealMetrics(t *testing.T) {
	pms := fixture(t)
	backend := "sre-lab-app/flask-vue-backend-6688dd7cbf-6tspt"
	specs := map[string]*corev1.Pod{
		backend: spec("sre-lab-app", "flask-vue-backend-6688dd7cbf-6tspt", "k8server", corev1.PodRunning, limits("backend", "50m", "", "64Mi", "256Mi")),
		// redis-master-0 has no spec here: deleted meanwhile.
		"sre-lab-app/idle-1": spec("sre-lab-app", "idle-1", "k8server", corev1.PodRunning),
		"sre-lab-app/wait-1": spec("sre-lab-app", "wait-1", "", corev1.PodPending),
	}
	r := Report(pms, specs, now, nil)
	if len(r.Pods) != 2 || r.Time != now {
		t.Fatalf("report = %+v", r)
	}
	b := r.Pods[0]
	if b.Name != "flask-vue-backend-6688dd7cbf-6tspt" || b.Node != "k8server" || b.MemoryBytes != 46052<<10 || b.CPUMillis != 2 ||
		b.WindowSeconds != 17.324 || !b.SampledAt.Equal(time.Date(2026, 9, 28, 2, 12, 32, 0, time.UTC)) {
		t.Errorf("backend = %+v", b)
	}
	c := b.Containers[0]
	if c.MemoryLimitBytes != 256<<20 || c.MemoryRequestBytes != 64<<20 || c.CPURequestMillis != 50 || c.CPULimitMillis != 0 || c.CPUOfLimit != 0 ||
		c.MemoryOfLimit != float64(46052<<10)*100/float64(256<<20) || c.CPUOfRequest != 4 {
		t.Errorf("backend container = %+v", c)
	}
	redis := r.Pods[1]
	if redis.Node != "" || len(redis.Containers) != 2 || redis.Containers[0].Name != "metrics" || redis.MemoryBytes != (13148+5260)<<10 ||
		redis.CPUMillis != 2+25 || redis.Containers[1].MemoryLimitBytes != 0 {
		t.Errorf("redis (no spec) = %+v", redis)
	}
	if len(r.Notes) != 1 || !strings.HasPrefix(r.Notes[0], "1 running pods have no metrics yet") {
		t.Errorf("notes = %v", r.Notes)
	}
}

func pm(ns, name string, memMiB int64, containers ...string) PodMetrics {
	var p PodMetrics
	p.Metadata.Namespace, p.Metadata.Name = ns, name
	for _, c := range containers {
		p.Containers = append(p.Containers, struct {
			Name  string              `json:"name"`
			Usage corev1.ResourceList `json:"usage"`
		}{c, corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1m"), corev1.ResourceMemory: *resource.NewQuantity(memMiB<<20, resource.BinarySI)}})
	}
	return p
}

func TestOrderAndCap(t *testing.T) {
	specs := map[string]*corev1.Pod{"shop/web": spec("shop", "web", "n1", corev1.PodRunning, limits("app", "", "", "", "40Mi"), limits("sidecar", "", "", "", ""))}
	r := Report([]PodMetrics{pm("shop", "web", 10, "sidecar", "app"), pm("shop", "db", 30, "db"), pm("other", "x", 20, "x")}, specs, now, nil)
	var names []string
	for _, p := range r.Pods {
		names = append(names, p.Namespace+"/"+p.Name)
	}
	if strings.Join(names, " ") != "shop/db other/x shop/web" {
		t.Errorf("order = %v", names)
	}
	web := r.Pods[2]
	if web.MemoryBytes != 20<<20 || web.Containers[0].Name != "app" || web.Containers[0].MemoryOfLimit != 25 || !web.SampledAt.IsZero() {
		t.Errorf("web = %+v", web)
	}
	var many []PodMetrics
	for i := range protocol.MaxUsagePods + 3 {
		many = append(many, pm("ns", fmt.Sprintf("p%03d", i), int64(i), "c"))
	}
	r = Report(many, nil, now, nil)
	if len(r.Pods) != protocol.MaxUsagePods || len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "3 smaller pods") {
		t.Errorf("cap: %d pods, notes %v", len(r.Pods), r.Notes)
	}
	if _, err := json.Marshal(r); err != nil {
		t.Fatal(err)
	}
}
