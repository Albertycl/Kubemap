package diagnose

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"kubemap/pkg/protocol"
)

// PodMetrics is the metrics-server resource (read with get only).
var PodMetrics = schema.GroupVersionResource{Group: "metrics.k8s.io", Version: "v1beta1", Resource: "pods"}

// PreviousLogRetry is the pause before the one retry of a previous-run log
// read that failed.
var PreviousLogRetry = 2 * time.Second

// maxLogBytes bounds each container's log read; 100 lines rarely need more.
const maxLogBytes = 64 << 10

// Collector gathers the inputs. Pod comes from the agent's cache; events,
// logs and metrics are read live (list, get and pods/log only). Any of the
// clients may be nil; the diagnosis then says what it lacks.
type Collector struct {
	Pod     func(namespace, name string) (*corev1.Pod, error)
	Owner   func(p *corev1.Pod) string
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	// LogStream opens a log stream; nil uses Kube (tests replace it).
	LogStream func(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error)
}

func (c *Collector) openLogs(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
	if c.LogStream != nil {
		return c.LogStream(ctx, namespace, pod, opts)
	}
	return c.Kube.CoreV1().Pods(namespace).GetLogs(pod, opts).Stream(ctx)
}

// Diagnose collects and analyses one pod.
func (c *Collector) Diagnose(ctx context.Context, namespace, name string) (*protocol.PodDiagnosis, error) {
	p, err := c.Pod(namespace, name)
	if err != nil {
		return nil, err
	}
	in := Inputs{Pod: p, Logs: map[string]Log{}}
	if c.Owner != nil {
		in.Owner = c.Owner(p)
	}
	note := func(format string, args ...any) { in.Notes = append(in.Notes, fmt.Sprintf(format, args...)) }
	if c.Kube == nil {
		note("events and logs not available: no API client")
	} else {
		in.Events, err = c.events(ctx, p)
		if err != nil {
			note("events not read: %s", reason(err))
		}
		for _, ct := range append(append([]corev1.Container{}, p.Spec.InitContainers...), p.Spec.Containers...) {
			l, ok, err := c.logs(ctx, p, ct.Name)
			switch {
			case err != nil:
				note("logs of %s not read: %s", ct.Name, reason(err))
			case ok:
				in.Logs[ct.Name] = l
			}
		}
	}
	in.Usage, err = c.usage(ctx, p)
	if err != nil {
		note("usage not available: %s", reason(err))
	}
	return Analyse(in), nil
}

// events lists the pod's events live: the cache keeps Warning events only,
// and a diagnosis needs Killing, BackOff and Pulled too. Events of an older
// pod with the same name (a StatefulSet) are left out by UID.
func (c *Collector) events(ctx context.Context, p *corev1.Pod) ([]corev1.Event, error) {
	sel := fields.Set{"involvedObject.name": p.Name, "involvedObject.namespace": p.Namespace}.AsSelector().String()
	list, err := c.Kube.CoreV1().Events(p.Namespace).List(ctx, metav1.ListOptions{FieldSelector: sel, Limit: 200})
	if err != nil {
		return nil, err
	}
	var out []corev1.Event
	for _, e := range list.Items {
		if e.InvolvedObject.UID == "" || p.UID == "" || e.InvolvedObject.UID == p.UID {
			out = append(out, e)
		}
	}
	return out, nil
}

// logs reads the last lines of the previous run when there was one (the
// crash), else of the current run. A container that never started has none.
func (c *Collector) logs(ctx context.Context, p *corev1.Pod, container string) (Log, bool, error) {
	var cs *corev1.ContainerStatus
	for _, s := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if s.Name == container {
			cs = &s
		}
	}
	if cs == nil {
		return Log{}, false, nil
	}
	previous := cs.RestartCount > 0 && cs.LastTerminationState.Terminated != nil
	if !previous && cs.State.Running == nil && cs.State.Terminated == nil {
		return Log{}, false, nil // never started: nothing to read
	}
	tail, limit := int64(protocol.MaxDiagnoseLogLines), int64(maxLogBytes)
	opts := &corev1.PodLogOptions{Container: container, Previous: previous, TailLines: &tail, LimitBytes: &limit}
	stream, err := c.openLogs(ctx, p.Namespace, p.Name, opts)
	if err != nil && previous {
		// Right after a restart the runtime may still be removing the old
		// container: "unable to retrieve container logs" (KM-71 F2). It
		// works a moment later, so try once more.
		select {
		case <-ctx.Done():
			return Log{}, false, err
		case <-time.After(PreviousLogRetry):
		}
		stream, err = c.openLogs(ctx, p.Namespace, p.Name, opts)
	}
	if err != nil {
		return Log{}, false, err
	}
	defer stream.Close()
	b, err := io.ReadAll(io.LimitReader(stream, maxLogBytes+1))
	if err != nil {
		return Log{}, false, err
	}
	l := Log{Text: string(b), From: "current"}
	if previous {
		l.From = "previous"
	}
	if len(b) > maxLogBytes {
		l.Text, l.Truncated = string(b[:maxLogBytes]), true
	}
	return l, true, nil
}

// usage reads the pod's metrics from the metrics API (metrics-server).
func (c *Collector) usage(ctx context.Context, p *corev1.Pod) (map[string]Usage, error) {
	if c.Dynamic == nil {
		return nil, errors.New("no API client")
	}
	if p.Status.Phase != corev1.PodRunning {
		return nil, fmt.Errorf("the pod is %s", p.Status.Phase)
	}
	u, err := c.Dynamic.Resource(PodMetrics).Namespace(p.Namespace).Get(ctx, p.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return parseUsage(u), nil
}

func parseUsage(u *unstructured.Unstructured) map[string]Usage {
	out := map[string]Usage{}
	cs, _, _ := unstructured.NestedSlice(u.Object, "containers")
	for _, x := range cs {
		m, ok := x.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		usage, _ := m["usage"].(map[string]any)
		var us Usage
		if s, ok := usage["cpu"].(string); ok {
			if q, err := resource.ParseQuantity(s); err == nil {
				us.CPUMillis = q.MilliValue()
			}
		}
		if s, ok := usage["memory"].(string); ok {
			if q, err := resource.ParseQuantity(s); err == nil {
				us.MemoryBytes = q.Value()
			}
		}
		out[name] = us
	}
	return out
}

// reason turns an API error into a short, actionable note.
func reason(err error) string {
	switch {
	case apierrors.IsForbidden(err):
		return "forbidden by the agent's RBAC (" + clip(err.Error(), 160) + ")"
	case apierrors.IsNotFound(err):
		return "not found"
	case strings.Contains(err.Error(), "the server could not find the requested resource"):
		return "the API is not served (is metrics-server installed?)"
	}
	return clip(err.Error(), 200)
}
