package resources

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The portal reads only the fields it shows from the raw objects the agent
// sends, with its own small types: it does not need the Kubernetes API
// packages (they would grow the binary a lot).

type objectMeta struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
	Labels            map[string]string `json:"labels"`
	OwnerReferences   []struct {
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		Controller bool   `json:"controller"`
	} `json:"ownerReferences"`
}

// Namespace is one row of the namespace list.
type Namespace struct {
	Name    string    `json:"name"`
	Phase   string    `json:"phase"`
	Created time.Time `json:"created"`
	// PodSecurity is the enforced Pod Security level, if the namespace has one.
	PodSecurity string `json:"podSecurity,omitempty"`
}

func namespaceView(raw json.RawMessage) (Namespace, error) {
	var ns struct {
		Metadata objectMeta `json:"metadata"`
		Status   struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := json.Unmarshal(raw, &ns); err != nil || ns.Metadata.Name == "" {
		return Namespace{}, fmt.Errorf("not a namespace")
	}
	return Namespace{
		Name: ns.Metadata.Name, Phase: ns.Status.Phase, Created: ns.Metadata.CreationTimestamp,
		PodSecurity: ns.Metadata.Labels["pod-security.kubernetes.io/enforce"],
	}, nil
}

// Pod is one row of the pod list, with what the log view needs.
type Pod struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// Status is what "kubectl get pods" shows: Running, CrashLoopBackOff,
	// Init:0/1, Completed, Terminating ...
	Status   string    `json:"status"`
	Phase    string    `json:"phase"`
	Ready    int       `json:"ready"` // ready containers
	Total    int       `json:"total"` // containers (not init)
	Restarts int32     `json:"restarts"`
	Node     string    `json:"node,omitempty"`
	Created  time.Time `json:"created"`
	// Owner is the controller that owns the pod ("ReplicaSet/web-7d9f8").
	Owner      string      `json:"owner,omitempty"`
	Containers []Container `json:"containers"`
}

// Container is one container of a pod, for the log view's picker.
type Container struct {
	Name     string `json:"name"`
	Init     bool   `json:"init,omitempty"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
	State    string `json:"state"` // running, waiting: <reason>, terminated: <reason>
	// Previous is true when the container has a terminated earlier run, so
	// "previous" logs exist.
	Previous bool `json:"previous,omitempty"`
}

type containerState struct {
	Running *struct{} `json:"running"`
	Waiting *struct {
		Reason string `json:"reason"`
	} `json:"waiting"`
	Terminated *struct {
		Reason   string `json:"reason"`
		ExitCode int32  `json:"exitCode"`
	} `json:"terminated"`
}

type containerStatus struct {
	Name         string         `json:"name"`
	Ready        bool           `json:"ready"`
	RestartCount int32          `json:"restartCount"`
	State        containerState `json:"state"`
	LastState    containerState `json:"lastState"`
}

type rawPod struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		NodeName       string `json:"nodeName"`
		InitContainers []struct {
			Name string `json:"name"`
		} `json:"initContainers"`
		Containers []struct {
			Name string `json:"name"`
		} `json:"containers"`
	} `json:"spec"`
	Status struct {
		Phase                 string            `json:"phase"`
		Reason                string            `json:"reason"`
		InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
		ContainerStatuses     []containerStatus `json:"containerStatuses"`
	} `json:"status"`
}

// PodView reads the pod row from a raw Pod object.
func PodView(raw json.RawMessage) (Pod, error) {
	var p rawPod
	if err := json.Unmarshal(raw, &p); err != nil || p.Metadata.Name == "" {
		return Pod{}, fmt.Errorf("not a pod")
	}
	out := Pod{
		Namespace: p.Metadata.Namespace, Name: p.Metadata.Name, Phase: p.Status.Phase,
		Node: p.Spec.NodeName, Created: p.Metadata.CreationTimestamp, Total: len(p.Spec.Containers),
		Containers: []Container{},
	}
	for _, o := range p.Metadata.OwnerReferences {
		if o.Controller {
			out.Owner = o.Kind + "/" + o.Name
		}
	}
	byName := func(list []containerStatus, name string) *containerStatus {
		for i := range list {
			if list[i].Name == name {
				return &list[i]
			}
		}
		return nil
	}
	add := func(name string, init bool, st *containerStatus) {
		c := Container{Name: name, Init: init, State: "waiting"}
		if st != nil {
			c.Ready, c.Restarts, c.State = st.Ready, st.RestartCount, stateText(st.State)
			c.Previous = st.LastState.Terminated != nil
		}
		out.Containers = append(out.Containers, c)
	}
	for _, c := range p.Spec.InitContainers {
		add(c.Name, true, byName(p.Status.InitContainerStatuses, c.Name))
	}
	for _, c := range p.Spec.Containers {
		st := byName(p.Status.ContainerStatuses, c.Name)
		add(c.Name, false, st)
		if st != nil {
			out.Restarts += st.RestartCount
			if st.Ready {
				out.Ready++
			}
		}
	}
	out.Status = podStatus(&p)
	return out, nil
}

func stateText(s containerState) string {
	switch {
	case s.Running != nil:
		return "running"
	case s.Waiting != nil:
		return "waiting: " + s.Waiting.Reason
	case s.Terminated != nil:
		return "terminated: " + s.Terminated.Reason
	}
	return "waiting"
}

// podStatus follows "kubectl get pods" closely: the pod's reason (Evicted),
// init container progress, then the first waiting or failed container's
// reason, then the phase; Terminating while being deleted.
func podStatus(p *rawPod) string {
	status := p.Status.Phase
	if p.Status.Reason != "" {
		status = p.Status.Reason
	}
	for i, c := range p.Status.InitContainerStatuses {
		switch {
		case c.State.Terminated != nil && c.State.Terminated.ExitCode == 0:
			continue
		case c.State.Terminated != nil:
			if c.State.Terminated.Reason != "" {
				return "Init:" + c.State.Terminated.Reason
			}
			return fmt.Sprintf("Init:ExitCode:%d", c.State.Terminated.ExitCode)
		case c.State.Waiting != nil && c.State.Waiting.Reason != "" && c.State.Waiting.Reason != "PodInitializing":
			return "Init:" + c.State.Waiting.Reason
		default:
			return fmt.Sprintf("Init:%d/%d", i, len(p.Spec.InitContainers))
		}
	}
	running := false
	for _, c := range p.Status.ContainerStatuses {
		switch {
		case c.State.Waiting != nil && c.State.Waiting.Reason != "":
			status = c.State.Waiting.Reason
		case c.State.Terminated != nil && c.State.Terminated.Reason != "":
			status = c.State.Terminated.Reason
		case c.State.Running != nil:
			running = true
		}
	}
	if status == "Completed" && running {
		status = "Running"
	}
	if p.Metadata.DeletionTimestamp != nil {
		return "Terminating"
	}
	return status
}

// Service is one row of the Service list, with its ports for the
// NetworkPolicy checker.
type Service struct {
	Namespace string            `json:"namespace"`
	Name      string            `json:"name"`
	Type      string            `json:"type"`
	ClusterIP string            `json:"clusterIP,omitempty"`
	Selector  map[string]string `json:"selector,omitempty"`
	Ports     []ServicePort     `json:"ports"`
}

// ServicePort is one port of a Service.
type ServicePort struct {
	Name       string `json:"name,omitempty"`
	Port       int32  `json:"port"`
	Protocol   string `json:"protocol"`
	TargetPort string `json:"targetPort,omitempty"`
}

// ServiceView reads the Service row from a raw Service object.
func ServiceView(raw json.RawMessage) (Service, error) {
	var s struct {
		Metadata objectMeta `json:"metadata"`
		Spec     struct {
			Type      string            `json:"type"`
			ClusterIP string            `json:"clusterIP"`
			Selector  map[string]string `json:"selector"`
			Ports     []struct {
				Name       string          `json:"name"`
				Port       int32           `json:"port"`
				Protocol   string          `json:"protocol"`
				TargetPort json.RawMessage `json:"targetPort"`
			} `json:"ports"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(raw, &s); err != nil || s.Metadata.Name == "" {
		return Service{}, fmt.Errorf("not a service")
	}
	out := Service{Namespace: s.Metadata.Namespace, Name: s.Metadata.Name, Type: s.Spec.Type,
		ClusterIP: s.Spec.ClusterIP, Selector: s.Spec.Selector, Ports: []ServicePort{}}
	for _, p := range s.Spec.Ports {
		proto := p.Protocol
		if proto == "" {
			proto = "TCP"
		}
		tp := strings.Trim(string(p.TargetPort), `"`) // a number or a port name
		out.Ports = append(out.Ports, ServicePort{Name: p.Name, Port: p.Port, Protocol: proto, TargetPort: tp})
	}
	return out, nil
}
