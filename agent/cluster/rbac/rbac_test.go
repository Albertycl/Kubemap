package rbac

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/pkg/protocol"
)

type fakeSource map[schema.GroupVersionResource][]runtime.Object

func (f fakeSource) List(g schema.GroupVersionResource, _ string, _ labels.Selector) ([]runtime.Object, error) {
	l, ok := f[g]
	if !ok {
		return nil, os.ErrNotExist
	}
	return l, nil
}

func load[T any, P interface {
	*T
	runtime.Object
}](t *testing.T, file string) []runtime.Object {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", file))
	if err != nil {
		t.Fatal(err)
	}
	var list struct{ Items []T }
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	out := make([]runtime.Object, len(list.Items))
	for i := range list.Items {
		out[i] = P(&list.Items[i])
	}
	return out
}

// lab is this cluster's RBAC, exported read-only on 2026-09-28 (rules,
// role references and subjects only): 115 ClusterRoles, 92
// ClusterRoleBindings, 21 Roles, 24 RoleBindings.
func lab(t *testing.T) *Snapshot {
	t.Helper()
	s, err := Load(fakeSource{
		rolesGVR:               load[rbacv1.Role](t, "roles.json"),
		clusterRolesGVR:        load[rbacv1.ClusterRole](t, "clusterroles.json"),
		roleBindingsGVR:        load[rbacv1.RoleBinding](t, "rolebindings.json"),
		clusterRoleBindingsGVR: load[rbacv1.ClusterRoleBinding](t, "clusterrolebindings.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func sa(ns, name string) *protocol.RBACSubject {
	return &protocol.RBACSubject{Kind: protocol.SubjectServiceAccount, Namespace: ns, Name: name}
}

// The same questions were asked of the real API server with
// "kubectl auth can-i … --as=system:serviceaccount:…" on 2026-09-28; the
// engine must give the API server's answer every time.
func TestAgreesWithTheAPIServer(t *testing.T) {
	s := lab(t)
	for _, tc := range []struct {
		sub                       *protocol.RBACSubject
		verb, resource, group, ns string
		want                      bool
	}{
		{sa("kubemap-demo", "reporter"), "list", "pods", "", "sre-lab-app", false},
		{sa("kubemap-demo", "reporter"), "list", "pods", "", "kubemap-demo", false},
		{sa("kubemap-demo", "reporter"), "create", "networkpolicies", "projectcalico.org", "sre-lab-app", true},
		{sa("kubemap-demo", "reporter"), "create", "selfsubjectaccessreviews", "authorization.k8s.io", "", true},
		{sa("argocd", "argocd-application-controller"), "get", "secrets", "", "sre-lab-app", true},
		{sa("argocd", "argocd-application-controller"), "create", "pods", "", "sre-lab-app", true},
		{sa("sre-lab-app", "default"), "get", "secrets", "", "sre-lab-app", false},
		{sa("monitoring", "kube-prometheus-stack-kube-state-metrics"), "list", "nodes", "", "", true},
		{sa("monitoring", "kube-prometheus-stack-kube-state-metrics"), "list", "secrets", "", "sre-lab-app", true},
		{sa("monitoring", "kube-prometheus-stack-kube-state-metrics"), "list", "secrets", "", "", true},
		{sa("argocd", "argocd-server"), "get", "secrets", "", "argocd", true},
		{sa("argocd", "argocd-server"), "create", "pods/exec", "", "argocd", false},
		{sa("kube-system", "coredns"), "get", "configmaps", "", "kube-system", false},
		{sa("kube-system", "coredns"), "list", "endpointslices", "discovery.k8s.io", "", true},
		{sa("cnpg-system", "cnpg-manager"), "get", "secrets", "", "cnpg-system", true},
		{sa("kube-system", "traefik"), "impersonate", "users", "", "", false},
		{sa("kube-system", "traefik"), "get", "secrets", "", "kube-system", true},
		{sa("kube-system", "traefik"), "get", "secrets", "", "", true},
	} {
		r := s.Query(&protocol.RBACQuery{Subject: tc.sub, Verb: tc.verb, Resource: tc.resource, APIGroup: tc.group, Namespace: tc.ns})
		if r.Mode != protocol.RBACCheck || r.Allowed == nil || *r.Allowed != tc.want {
			t.Errorf("%s/%s %s %s.%s in %q: allowed %v, the API server says %v (grants %v)", tc.sub.Namespace, tc.sub.Name, tc.verb, tc.resource, tc.group, tc.ns, deref(r.Allowed), tc.want, r.Grants)
		}
		if tc.want && len(r.Grants) == 0 {
			t.Errorf("%s %s: allowed with no grant named", tc.sub.Name, tc.verb)
		}
	}
}

func deref(b *bool) bool { return b != nil && *b }

func TestChainOfTheLabReporter(t *testing.T) {
	r := lab(t).Query(&protocol.RBACQuery{Subject: sa("kubemap-demo", "reporter")})
	if r.Mode != protocol.RBACChain || !slices.Contains(r.Groups, "system:serviceaccounts:kubemap-demo") {
		t.Fatalf("result = %+v", r)
	}
	var via []string
	for _, g := range r.Grants {
		via = append(via, g.Binding+" via "+g.Via)
		if !strings.HasPrefix(g.Via, "group ") {
			t.Errorf("reporter has only group grants, got %s via %s", g.Binding, g.Via)
		}
	}
	for _, want := range []string{"ClusterRoleBinding//calico-tiered-policy-passthrough via group system:authenticated",
		"ClusterRoleBinding//system:discovery via group system:authenticated"} {
		if !slices.Contains(via, want) {
			t.Errorf("missing %q in %v", want, via)
		}
	}
	if !slices.Contains(r.Risky, "wildcard") {
		t.Errorf("Calico's passthrough grants verbs *: risky = %v", r.Risky)
	}
}

func TestChainFindsAdminEquivalentRights(t *testing.T) {
	r := lab(t).Query(&protocol.RBACQuery{Subject: sa("argocd", "argocd-application-controller")})
	for _, want := range []string{"read secrets", "create pods", "wildcard"} {
		if !slices.Contains(r.Risky, want) {
			t.Errorf("argocd-application-controller risky = %v, want %q", r.Risky, want)
		}
	}
	r = lab(t).Query(&protocol.RBACQuery{Subject: sa("kube-system", "traefik")})
	if !slices.Contains(r.Risky, "read secrets") || slices.Contains(r.Risky, "create pods") {
		t.Errorf("traefik risky = %v", r.Risky)
	}
}

func TestWhoCanReadSecrets(t *testing.T) {
	r := lab(t).Query(&protocol.RBACQuery{Verb: "get", Resource: "secrets", Namespace: "sre-lab-app"})
	if r.Mode != protocol.RBACWhoCan || len(r.Subjects) == 0 {
		t.Fatalf("result = %+v", r)
	}
	names := map[string]bool{}
	for _, w := range r.Subjects {
		names[w.Namespace+"/"+w.Name] = true
		if w.Everyone {
			t.Errorf("an everyone-group can read secrets: %+v", w)
		}
	}
	for _, want := range []string{"argocd/argocd-application-controller", "kube-system/traefik", "/system:masters"} {
		if !names[want] {
			t.Errorf("who can get secrets in sre-lab-app lacks %s (have %d subjects)", want, len(names))
		}
	}
	if names["kubemap-demo/reporter"] {
		t.Error("the reporter cannot read secrets")
	}
}

func TestWhoCanEveryoneFirst(t *testing.T) {
	r := lab(t).Query(&protocol.RBACQuery{Verb: "create", Resource: "networkpolicies", APIGroup: "projectcalico.org", Namespace: "sre-lab-app"})
	if len(r.Subjects) == 0 || !r.Subjects[0].Everyone || r.Subjects[0].Name != "system:authenticated" {
		t.Fatalf("first = %+v", r.Subjects)
	}
	if !strings.Contains(strings.Join(r.Notes, " "), "contains everyone (system:authenticated)") {
		t.Errorf("notes = %v", r.Notes)
	}
}

// Synthetic RBAC for the rules the real cluster does not exercise.
func syn() *Snapshot {
	role := func(ns, name string, rules ...rbacv1.PolicyRule) *rbacv1.Role {
		return &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Rules: rules}
	}
	s := &Snapshot{
		Roles: map[string]*rbacv1.Role{
			"app/pod-reader": role("app", "pod-reader", rbacv1.PolicyRule{Verbs: []string{"get", "list"}, APIGroups: []string{""}, Resources: []string{"pods"}}),
			"app/one-secret": role("app", "one-secret", rbacv1.PolicyRule{Verbs: []string{"get"}, APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"db"}}),
			"app/any-scale":  role("app", "any-scale", rbacv1.PolicyRule{Verbs: []string{"patch"}, APIGroups: []string{"*"}, Resources: []string{"*/scale"}}),
		},
		ClusterRoles: map[string]*rbacv1.ClusterRole{
			"view-pods": {ObjectMeta: metav1.ObjectMeta{Name: "view-pods"}, Rules: []rbacv1.PolicyRule{{Verbs: []string{"list"}, APIGroups: []string{""}, Resources: []string{"pods"}}}},
			"urls":      {ObjectMeta: metav1.ObjectMeta{Name: "urls"}, Rules: []rbacv1.PolicyRule{{Verbs: []string{"get"}, NonResourceURLs: []string{"/healthz"}}}},
		},
	}
	rb := func(ns, name, kind, role string, subs ...rbacv1.Subject) *rbacv1.RoleBinding {
		return &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Subjects: subs,
			RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: kind, Name: role}}
	}
	saSub := func(ns, name string) rbacv1.Subject {
		return rbacv1.Subject{Kind: "ServiceAccount", Namespace: ns, Name: name}
	}
	s.RoleBindings = []*rbacv1.RoleBinding{
		rb("app", "read", "Role", "pod-reader", saSub("app", "web")),
		rb("app", "secret", "Role", "one-secret", saSub("app", "web")),
		rb("app", "scale", "Role", "any-scale", rbacv1.Subject{Kind: "Group", Name: "ops"}),
		rb("app", "cluster-role-here", "ClusterRole", "view-pods", rbacv1.Subject{Kind: "User", Name: "alice"}),
		rb("app", "gone", "Role", "deleted-role", saSub("app", "web")),
		rb("app", "as-user", "ClusterRole", "view-pods", rbacv1.Subject{Kind: "User", Name: "system:serviceaccount:app:batch"}),
	}
	s.ClusterRoleBindings = []*rbacv1.ClusterRoleBinding{{ObjectMeta: metav1.ObjectMeta{Name: "urls"},
		RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: "urls"}, Subjects: []rbacv1.Subject{{Kind: "Group", Name: "system:authenticated"}}}}
	return s
}

func check(s *Snapshot, sub *protocol.RBACSubject, verb, res, group, ns, name string) bool {
	r := s.Query(&protocol.RBACQuery{Subject: sub, Verb: verb, Resource: res, APIGroup: group, Namespace: ns, ResourceName: name})
	return deref(r.Allowed)
}

func TestRules(t *testing.T) {
	s := syn()
	web := sa("app", "web")
	alice := &protocol.RBACSubject{Kind: protocol.SubjectUser, Name: "alice"}
	bob := &protocol.RBACSubject{Kind: protocol.SubjectUser, Name: "bob", Groups: []string{"ops"}}
	for _, tc := range []struct {
		name string
		got  bool
		want bool
	}{
		{"Role in its namespace", check(s, web, "list", "pods", "", "app", ""), true},
		{"Role not in another namespace", check(s, web, "list", "pods", "", "other", ""), false},
		{"Role not cluster-wide", check(s, web, "list", "pods", "", "", ""), false},
		{"verb not granted", check(s, web, "delete", "pods", "", "app", ""), false},
		{"resourceNames: the named Secret", check(s, web, "get", "secrets", "", "app", "db"), true},
		{"resourceNames: another Secret", check(s, web, "get", "secrets", "", "app", "api-key"), false},
		{"resourceNames: no name asked", check(s, web, "get", "secrets", "", "app", ""), false},
		{"ClusterRole through a RoleBinding: its namespace", check(s, alice, "list", "pods", "", "app", ""), true},
		{"ClusterRole through a RoleBinding: not elsewhere", check(s, alice, "list", "pods", "", "other", ""), false},
		{"group from the user's groups, */scale", check(s, bob, "patch", "deployments/scale", "apps", "app", ""), true},
		{"*/scale is not the deployment itself", check(s, bob, "patch", "deployments", "apps", "app", ""), false},
		{"a ServiceAccount bound as a user", check(s, sa("app", "batch"), "list", "pods", "", "app", ""), true},
		{"non-resource rules never grant resources", check(s, alice, "get", "healthz", "", "", ""), false},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, tc.got, tc.want)
		}
	}
	// The limited-by-name note, and the missing role.
	r := s.Query(&protocol.RBACQuery{Subject: web, Verb: "get", Resource: "secrets", Namespace: "app"})
	if deref(r.Allowed) || len(r.Grants) != 1 || !strings.Contains(strings.Join(r.Notes, " "), "resourceNames") {
		t.Errorf("limited = %+v", r)
	}
	r = s.Query(&protocol.RBACQuery{Subject: web})
	missing := false
	for _, g := range r.Grants {
		missing = missing || g.RoleMissing
	}
	if !missing || !strings.Contains(strings.Join(r.Notes, " "), "Role/app/deleted-role, which does not exist") {
		t.Errorf("missing role = %+v", r.Notes)
	}
	// Who can, with names only.
	r = s.Query(&protocol.RBACQuery{Verb: "get", Resource: "secrets", Namespace: "app"})
	if len(r.Subjects) != 1 || !slices.Equal(r.Subjects[0].ResourceNames, []string{"db"}) {
		t.Errorf("who can get secrets = %+v", r.Subjects)
	}
	// A group subject queried directly.
	r = s.Query(&protocol.RBACQuery{Subject: &protocol.RBACSubject{Kind: protocol.SubjectGroup, Name: "ops"}})
	if len(r.Grants) != 1 || r.Grants[0].Via != "direct" || r.Groups != nil {
		t.Errorf("group chain = %+v", r)
	}
}

func TestLoadNeedsAllFour(t *testing.T) {
	if _, err := Load(fakeSource{rolesGVR: nil, clusterRolesGVR: nil, roleBindingsGVR: nil}); err == nil {
		t.Error("a missing resource must fail: a partial who-can would be wrong in the dangerous direction")
	}
}

// review-km-34 B1: each admin-equivalent right is named.
func TestRiskyRights(t *testing.T) {
	r := func(group, res string, verbs ...string) rbacv1.PolicyRule {
		return rbacv1.PolicyRule{APIGroups: []string{group}, Resources: []string{res}, Verbs: verbs}
	}
	for _, tc := range []struct {
		rules []rbacv1.PolicyRule
		want  string
	}{
		{[]rbacv1.PolicyRule{r("apps", "deployments", "patch")}, "change workloads"},
		{[]rbacv1.PolicyRule{r("apps", "daemonsets", "update")}, "change workloads"},
		{[]rbacv1.PolicyRule{r("", "pods", "patch")}, "change workloads"},
		{[]rbacv1.PolicyRule{r("", "pods/ephemeralcontainers", "patch")}, "change workloads"},
		{[]rbacv1.PolicyRule{r("batch", "cronjobs", "update")}, "change workloads"},
		{[]rbacv1.PolicyRule{r("admissionregistration.k8s.io", "mutatingwebhookconfigurations", "create")}, "change admission control"},
		{[]rbacv1.PolicyRule{r("admissionregistration.k8s.io", "validatingadmissionpolicies", "delete")}, "change admission control"},
		{[]rbacv1.PolicyRule{r("", "persistentvolumes", "create")}, "create persistentvolumes"},
		{[]rbacv1.PolicyRule{r("certificates.k8s.io", "certificatesigningrequests/approval", "update"), r("certificates.k8s.io", "signers", "approve")}, "approve certificates"},
		{[]rbacv1.PolicyRule{r("", "nodes", "patch")}, "change nodes"},
		{[]rbacv1.PolicyRule{r("authentication.k8s.io", "userextras", "impersonate")}, "impersonate"},
		// A role that scales, restarts and deletes pods and patches nodes (what an operator role would hold).
		{[]rbacv1.PolicyRule{
			{APIGroups: []string{"apps"}, Resources: []string{"deployments/scale", "statefulsets/scale"}, Verbs: []string{"get", "patch"}},
			{APIGroups: []string{"apps"}, Resources: []string{"deployments", "statefulsets", "daemonsets"}, Verbs: []string{"patch"}},
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"delete"}},
			{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"patch"}},
		}, "change workloads"},
	} {
		if got := riskyRights(tc.rules); !slices.Contains(got, tc.want) {
			t.Errorf("%v: risky %v, want %q", tc.rules, got, tc.want)
		}
	}
	// Not risky on their own: reads, scale only, approval without a signer.
	for _, rules := range [][]rbacv1.PolicyRule{
		{r("apps", "deployments", "get", "list", "watch")},
		{r("apps", "deployments/scale", "patch")},
		{r("certificates.k8s.io", "certificatesigningrequests/approval", "update")},
	} {
		if got := riskyRights(rules); len(got) != 0 {
			t.Errorf("%v: risky %v, want none", rules, got)
		}
	}
}

// review-km-34 N-1: anonymous is unauthenticated, not authenticated.
func TestAnonymousGroups(t *testing.T) {
	g := Groups(&protocol.RBACSubject{Kind: protocol.SubjectUser, Name: "system:anonymous"})
	if !slices.Contains(g, "system:unauthenticated") || slices.Contains(g, "system:authenticated") {
		t.Errorf("anonymous groups = %v", g)
	}
	s := syn()
	s.ClusterRoleBindings[0].Subjects = []rbacv1.Subject{{Kind: "Group", Name: "system:unauthenticated"}}
	r := s.Query(&protocol.RBACQuery{Subject: &protocol.RBACSubject{Kind: protocol.SubjectUser, Name: "system:anonymous"}})
	if len(r.Grants) != 1 || r.Grants[0].Via != "group system:unauthenticated" {
		t.Errorf("anonymous chain = %+v", r.Grants)
	}
}

// review-km-34 N-2: a binding to every ServiceAccount of a namespace is
// flagged and named in the notes.
func TestWhoCanNamespaceWide(t *testing.T) {
	s := syn()
	s.RoleBindings = append(s.RoleBindings, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "all-sa"},
		RoleRef:  rbacv1.RoleRef{Kind: "Role", Name: "pod-reader"},
		Subjects: []rbacv1.Subject{{Kind: "Group", Name: "system:serviceaccounts:app"}}})
	r := s.Query(&protocol.RBACQuery{Verb: "get", Resource: "pods", Namespace: "app"})
	var flagged bool
	for _, w := range r.Subjects {
		flagged = flagged || (w.NamespaceWide && w.Name == "system:serviceaccounts:app")
	}
	if !flagged || !slices.ContainsFunc(r.Notes, func(n string) bool { return strings.Contains(n, "Every ServiceAccount in namespace app") }) {
		t.Errorf("subjects %+v notes %v", r.Subjects, r.Notes)
	}
}
