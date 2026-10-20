package install

import (
	"bytes"
	"flag"
	"os"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"

	"kubemap/agent/cluster/cache"
	"kubemap/pkg/protocol"
)

var update = flag.Bool("update", false, "rewrite deploy/kubemap.yaml")

// The reference manifest in deploy/ uses a local image name (imported into K3s).
var golden = Options{Image: "localhost/kubemap:dev"}

const goldenPath = "../../deploy/kubemap.yaml"

// decode parses every document with the Kubernetes scheme: a typo in a
// field name or a wrong type fails here, like it would at kubectl apply.
func decode(t *testing.T, bundle []byte) []runtime.Object {
	t.Helper()
	var out []runtime.Object
	for _, doc := range bytes.Split(bundle, []byte("\n---\n")) {
		if len(bytes.TrimSpace(doc)) == 0 || bytes.HasPrefix(bytes.TrimSpace(doc), []byte("#")) && !bytes.Contains(doc, []byte("kind:")) {
			continue
		}
		obj, _, err := scheme.Codecs.UniversalDeserializer().Decode(doc, nil, nil)
		if err != nil {
			t.Fatalf("document does not decode: %v\n%s", err, doc)
		}
		out = append(out, obj)
	}
	return out
}

func find[T runtime.Object](objs []runtime.Object, name string) T {
	var zero T
	for _, o := range objs {
		if v, ok := o.(T); ok {
			if o.(interface{ GetName() string }).GetName() == name {
				return v
			}
		}
	}
	return zero
}

func TestGolden(t *testing.T) {
	b, err := Bundle(golden)
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		os.WriteFile(goldenPath, b, 0o644)
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if !bytes.Equal(want, b) {
		t.Errorf("%s is out of date: run go test ./agent/install -update", goldenPath)
	}
	if _, err := Bundle(Options{}); err == nil {
		t.Error("empty image accepted")
	}
}

func TestDocumentsAndOrder(t *testing.T) {
	b, _ := Bundle(golden)
	objs := decode(t, b)
	var kinds []string
	for _, o := range objs {
		kinds = append(kinds, o.GetObjectKind().GroupVersionKind().Kind)
	}
	want := []string{"Namespace", "Namespace", "ServiceAccount", "ClusterRole", "ClusterRoleBinding", "ClusterRole", "ClusterRoleBinding",
		"PersistentVolumeClaim", "Deployment", "Service", "ServiceAccount", "DaemonSet"}
	if !slices.Equal(kinds, want) {
		t.Errorf("kinds = %v", kinds)
	}
}

// KM-47 / A4: read-only, no wildcards, no Secrets, exactly the cached
// resources plus pods/log, and get/list on pods.metrics.k8s.io; no kubelet
// access (nodes/stats, nodes/proxy).
func TestReadOnlyClusterRole(t *testing.T) {
	b, _ := Bundle(golden)
	objs := decode(t, b)
	cr := find[*rbacv1.ClusterRole](objs, ServerName+"-readonly")
	if cr == nil {
		t.Fatal("no read-only ClusterRole")
	}
	granted := map[string]bool{}
	for _, r := range cr.Rules {
		want := []string{"get", "list", "watch"}
		if len(r.APIGroups) == 1 && r.APIGroups[0] == "metrics.k8s.io" {
			want = []string{"get", "list"}
		}
		if !slices.Equal(r.Verbs, want) {
			t.Errorf("rule %v has verbs %v, want %v", r.Resources, r.Verbs, want)
		}
		if len(r.APIGroups) != 1 || len(r.ResourceNames) != 0 || len(r.NonResourceURLs) != 0 {
			t.Errorf("unexpected rule shape: %+v", r)
		}
		for _, res := range r.Resources {
			if res == "*" || strings.Contains(res, "secret") || res == "pods/exec" || res == "pods/portforward" || strings.HasPrefix(res, "nodes/") {
				t.Errorf("forbidden resource %q in %v", res, r.APIGroups)
			}
			granted[r.APIGroups[0]+"/"+res] = true
		}
		if r.APIGroups[0] == "*" {
			t.Error("wildcard API group")
		}
	}
	for _, r := range cache.Resources() {
		if !granted[r.GVR.Group+"/"+r.GVR.Resource] {
			t.Errorf("cache watches %s but the role does not grant it", r.GVR)
		}
	}
	if !granted["/pods/log"] {
		t.Error("/pods/log missing (A4)")
	}
	if !granted["metrics.k8s.io/pods"] {
		t.Error("metrics.k8s.io/pods missing (KM-28)")
	}
	if len(granted) != len(cache.Resources())+2 {
		t.Errorf("role grants %d resources, want exactly the cache's %d plus pods/log and metrics.k8s.io/pods", len(granted), len(cache.Resources()))
	}
}

// A4: the server has no write access to the cluster by default, no Secrets;
// it may only create TokenReviews (to check node agent tokens).
func TestTokenReviewRole(t *testing.T) {
	b, _ := Bundle(golden)
	objs := decode(t, b)
	for _, o := range objs {
		if r, ok := o.(*rbacv1.Role); ok {
			t.Errorf("unexpected namespaced Role %s: the server needs no Secret access", r.Name)
		}
	}
	tr := find[*rbacv1.ClusterRole](objs, ServerName+"-tokenreview")
	if tr == nil || len(tr.Rules) != 1 || !slices.Equal(tr.Rules[0].Verbs, []string{"create"}) ||
		!slices.Equal(tr.Rules[0].Resources, []string{"tokenreviews"}) {
		t.Errorf("tokenreview role = %+v", tr)
	}
	for _, name := range []string{ServerName + "-readonly", ServerName + "-tokenreview"} {
		crb := find[*rbacv1.ClusterRoleBinding](objs, name)
		if crb == nil || crb.RoleRef.Name != name || len(crb.Subjects) != 1 ||
			crb.Subjects[0].Name != ServerName || crb.Subjects[0].Namespace != SystemNamespace {
			t.Errorf("binding %s = %+v", name, crb)
		}
	}
}

// restrictedOK checks the Pod Security "restricted" rules that apply to a
// container.
func restrictedOK(t *testing.T, what string, pod corev1.PodSpec, c corev1.Container, uid int64) {
	t.Helper()
	sc := c.SecurityContext
	switch {
	case sc == nil:
		t.Fatalf("%s: no security context", what)
	case sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser != uid:
		t.Errorf("%s: must run as non-root UID %d", what, uid)
	case sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation:
		t.Errorf("%s: privilege escalation allowed", what)
	case sc.Privileged != nil && *sc.Privileged:
		t.Errorf("%s: privileged", what)
	case sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem:
		t.Errorf("%s: writable root filesystem", what)
	case sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) || len(sc.Capabilities.Add) != 0:
		t.Errorf("%s: capabilities not all dropped", what)
	case sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault:
		t.Errorf("%s: no RuntimeDefault seccomp", what)
	}
	if pod.HostNetwork || pod.HostIPC {
		t.Errorf("%s: host network or IPC", what)
	}
	if c.Resources.Limits.Memory().IsZero() {
		t.Errorf("%s: no memory limit", what)
	}
}

// KM-48: the server passes Pod Security "restricted" (its namespace
// enforces it) and runs as 65532.
func TestServerDeployment(t *testing.T) {
	b, _ := Bundle(golden)
	d := find[*appsv1.Deployment](decode(t, b), ServerName)
	if d == nil {
		t.Fatal("no Deployment")
	}
	pod := d.Spec.Template.Spec
	c := pod.Containers[0]
	restrictedOK(t, "server", pod, c, ServerUID)
	if pod.HostPID || pod.HostNetwork {
		t.Error("server must have no host access")
	}
	vols := map[string]bool{}
	for _, v := range pod.Volumes {
		if v.HostPath != nil {
			t.Errorf("host path volume %s", v.Name)
		}
		vols[v.Name] = true
		if v.Name == "data" && v.PersistentVolumeClaim.ClaimName != DataClaim {
			t.Error("data volume is not the PVC")
		}
	}
	if !vols["data"] || !vols["tmp"] || len(vols) != 2 {
		t.Errorf("volumes = %v", vols)
	}
	if *d.Spec.Replicas != 1 || d.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Error("exactly one server (SQLite on a ReadWriteOnce volume), replaced not rolled")
	}
	if c.Image != golden.Image || !slices.Equal(c.Command, []string{"/usr/local/bin/kubemap-portal"}) || c.Env[0].ValueFrom.FieldRef.FieldPath != "metadata.namespace" {
		t.Errorf("container = %+v", c)
	}
	if pod.ServiceAccountName != ServerName || d.Namespace != SystemNamespace {
		t.Error("wrong account or namespace")
	}
}

// KM-63 / A7: host PID and read-only host mounts, nothing else; its own UID;
// no API token; the projected identity token for the server (KM-68).
func TestNodeAgentDaemonSet(t *testing.T) {
	b, _ := Bundle(golden)
	objs := decode(t, b)
	ds := find[*appsv1.DaemonSet](objs, NodeAgent)
	if ds == nil || ds.Namespace != NodeNamespace {
		t.Fatal("no DaemonSet in kubemap-node")
	}
	pod := ds.Spec.Template.Spec
	c := pod.Containers[0]
	restrictedOK(t, "node agent", pod, c, NodeAgentUID)
	if NodeAgentUID == ServerUID {
		t.Error("node agent must not share the server's UID")
	}
	if !pod.HostPID || pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("host PID on, API token off")
	}
	sa := find[*corev1.ServiceAccount](objs, NodeAgent)
	if sa == nil || sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Error("node agent ServiceAccount must not mount an API token")
	}
	hostPaths := map[string]string{}
	for _, v := range pod.Volumes {
		switch {
		case v.HostPath != nil:
			hostPaths[v.Name] = v.HostPath.Path
		case v.Projected != nil:
			tok := v.Projected.Sources[0].ServiceAccountToken
			if tok == nil || tok.Audience != protocol.NodeTokenAudience || tok.Path != "token" || *tok.ExpirationSeconds != 3600 {
				t.Errorf("identity token = %+v", tok)
			}
		default:
			t.Errorf("unexpected volume %s", v.Name)
		}
	}
	if len(hostPaths) != 3 || hostPaths["host-root"] != "/" || hostPaths["host-proc"] != "/proc" || hostPaths["host-sys"] != "/sys" {
		t.Errorf("host paths = %v", hostPaths)
	}
	for _, m := range c.VolumeMounts {
		if !m.ReadOnly {
			t.Errorf("mount %s is writable", m.Name)
		}
		// Recursively read-only (review-km-44 L5), which needs propagation
		// None: submounts such as /dev/shm must not stay writable.
		if _, host := hostPaths[m.Name]; host && (m.RecursiveReadOnly == nil || *m.RecursiveReadOnly != corev1.RecursiveReadOnlyIfPossible ||
			(m.MountPropagation != nil && *m.MountPropagation != corev1.MountPropagationNone)) {
			t.Errorf("mount %s must be recursively read-only with no propagation: %+v", m.Name, m)
		}
	}
	env := map[string]string{}
	for _, e := range c.Env {
		if e.ValueFrom != nil && e.ValueFrom.FieldRef != nil {
			env[e.Name] = "field:" + e.ValueFrom.FieldRef.FieldPath
		} else {
			env[e.Name] = e.Value
		}
	}
	if env["NODE_NAME"] != "field:spec.nodeName" || env["CLUSTER_ID"] != LocalClusterID ||
		env["PORTAL_URL"] != "ws://kubemap.kubemap-system.svc:8080/v1/connect" {
		t.Errorf("env = %v", env)
	}
	if c.Resources.Limits.Memory().String() != "64Mi" {
		t.Errorf("memory limit = %s, budget is 64Mi", c.Resources.Limits.Memory())
	}
	if len(pod.Tolerations) != 1 || pod.Tolerations[0].Operator != corev1.TolerationOpExists {
		t.Error("node agent must run on every node")
	}
}

func TestPullPolicy(t *testing.T) {
	b, _ := Bundle(Options{Image: "ghcr.io/example/kubemap:v1", PullPolicy: corev1.PullAlways})
	d := find[*appsv1.Deployment](decode(t, b), ServerName)
	if d.Spec.Template.Spec.Containers[0].ImagePullPolicy != corev1.PullAlways {
		t.Error("pull policy not applied")
	}
}

// The node agent finds the server through the Service the manifest creates.
func TestServiceMatchesNodeAgentURL(t *testing.T) {
	b, _ := Bundle(golden)
	objs := decode(t, b)
	svc := find[*corev1.Service](objs, ServerName)
	if svc == nil || svc.Namespace != SystemNamespace || svc.Spec.Ports[0].Port != ServerPort || svc.Spec.Type != "" && svc.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("service = %+v", svc)
	}
	if !strings.Contains(NodeConnectURL, ServerName+"."+SystemNamespace+".svc:") {
		t.Error(NodeConnectURL)
	}
	for _, ns := range []string{SystemNamespace, NodeNamespace} {
		if find[*corev1.Namespace](objs, ns) == nil {
			t.Errorf("namespace %s missing", ns)
		}
	}
	if find[*corev1.PersistentVolumeClaim](objs, DataClaim) == nil {
		t.Error("no data PVC")
	}
}
