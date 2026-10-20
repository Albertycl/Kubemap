// Package rbac evaluates Kubernetes RBAC from the objects in the cluster
// agent's cache (KM-34): which bindings and roles apply to a ServiceAccount,
// user or group (the chain), whether a subject may do an action and which
// binding allows it, and who may do an action. It follows the RBAC
// authorizer's rules; it does not ask the API server, so it needs no extra
// permission, and it cannot see other authorizers (Node, webhooks) or
// admission.
package rbac

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"kubemap/pkg/protocol"
)

// Source is the agent's cache (or a test fake).
type Source interface {
	List(gvr schema.GroupVersionResource, namespace string, sel labels.Selector) ([]runtime.Object, error)
}

func gvr(r string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: r}
}

var (
	rolesGVR               = gvr("roles")
	roleBindingsGVR        = gvr("rolebindings")
	clusterRolesGVR        = gvr("clusterroles")
	clusterRoleBindingsGVR = gvr("clusterrolebindings")
)

// Snapshot is the cluster's RBAC at one moment.
type Snapshot struct {
	Roles               map[string]*rbacv1.Role        // "ns/name"
	ClusterRoles        map[string]*rbacv1.ClusterRole // name
	RoleBindings        []*rbacv1.RoleBinding
	ClusterRoleBindings []*rbacv1.ClusterRoleBinding
}

// Load reads the four RBAC resources; all are required, because a partial
// answer to "who can" would be wrong in the dangerous direction.
func Load(src Source) (*Snapshot, error) {
	s := &Snapshot{Roles: map[string]*rbacv1.Role{}, ClusterRoles: map[string]*rbacv1.ClusterRole{}}
	list := func(g schema.GroupVersionResource, add func(runtime.Object)) error {
		objs, err := src.List(g, "", nil)
		if err != nil {
			return fmt.Errorf("rbac: %s: %w", g.Resource, err)
		}
		for _, o := range objs {
			add(o)
		}
		return nil
	}
	for _, step := range []func() error{
		func() error {
			return list(rolesGVR, func(o runtime.Object) { r := o.(*rbacv1.Role); s.Roles[r.Namespace+"/"+r.Name] = r })
		},
		func() error {
			return list(clusterRolesGVR, func(o runtime.Object) { r := o.(*rbacv1.ClusterRole); s.ClusterRoles[r.Name] = r })
		},
		func() error {
			return list(roleBindingsGVR, func(o runtime.Object) { s.RoleBindings = append(s.RoleBindings, o.(*rbacv1.RoleBinding)) })
		},
		func() error {
			return list(clusterRoleBindingsGVR, func(o runtime.Object) {
				s.ClusterRoleBindings = append(s.ClusterRoleBindings, o.(*rbacv1.ClusterRoleBinding))
			})
		},
	} {
		if err := step(); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(s.RoleBindings, func(a, b *rbacv1.RoleBinding) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	slices.SortFunc(s.ClusterRoleBindings, func(a, b *rbacv1.ClusterRoleBinding) int { return cmp.Compare(a.Name, b.Name) })
	return s, nil
}

// Note that every answer carries.
const scopeNote = "Evaluated from the RBAC objects in the agent's cache with the RBAC authorizer's rules; " +
	"other authorizers (Node, webhooks) and admission (PodSecurity, quota, webhooks) are not included. " +
	"Node identities (kubelets, system:nodes) get their rights from the Node authorizer and are not shown."

// binding is a RoleBinding or ClusterRoleBinding, flattened.
type binding struct {
	id       string // "RoleBinding/ns/name" or "ClusterRoleBinding//name"
	scope    string // namespace, or "cluster"
	subjects []rbacv1.Subject
	roleID   string
	rules    []rbacv1.PolicyRule
	missing  bool
}

func (s *Snapshot) bindings() []binding {
	var out []binding
	for _, rb := range s.RoleBindings {
		b := binding{id: "RoleBinding/" + rb.Namespace + "/" + rb.Name, scope: rb.Namespace, subjects: rb.Subjects}
		switch rb.RoleRef.Kind {
		case "Role":
			b.roleID = "Role/" + rb.Namespace + "/" + rb.RoleRef.Name
			if r, ok := s.Roles[rb.Namespace+"/"+rb.RoleRef.Name]; ok {
				b.rules = r.Rules
			} else {
				b.missing = true
			}
		default:
			b.roleID = "ClusterRole//" + rb.RoleRef.Name
			if r, ok := s.ClusterRoles[rb.RoleRef.Name]; ok {
				b.rules = r.Rules
			} else {
				b.missing = true
			}
		}
		out = append(out, b)
	}
	for _, crb := range s.ClusterRoleBindings {
		b := binding{id: "ClusterRoleBinding//" + crb.Name, scope: "cluster", subjects: crb.Subjects, roleID: "ClusterRole//" + crb.RoleRef.Name}
		if r, ok := s.ClusterRoles[crb.RoleRef.Name]; ok {
			b.rules = r.Rules
		} else {
			b.missing = true
		}
		out = append(out, b)
	}
	return out
}

// everyone are the groups that contain every account or every user.
var everyone = map[string]bool{"system:authenticated": true, "system:unauthenticated": true, "system:serviceaccounts": true}

// Groups returns the groups a subject is evaluated with.
func Groups(sub *protocol.RBACSubject) []string {
	switch sub.Kind {
	case protocol.SubjectServiceAccount:
		return []string{"system:serviceaccounts", "system:serviceaccounts:" + sub.Namespace, "system:authenticated"}
	case protocol.SubjectUser:
		// An anonymous request is in system:unauthenticated, never in
		// system:authenticated.
		implicit := "system:authenticated"
		if sub.Name == "system:anonymous" {
			implicit = "system:unauthenticated"
		}
		g := slices.Clone(sub.Groups)
		if !slices.Contains(g, implicit) {
			g = append(g, implicit)
		}
		return g
	}
	return nil
}

// matches says how a binding subject applies to the queried subject: ""
// when it does not, "direct", or "group <name>".
func matches(bs rbacv1.Subject, sub *protocol.RBACSubject, groups []string) string {
	switch bs.Kind {
	case rbacv1.ServiceAccountKind:
		if sub.Kind == protocol.SubjectServiceAccount && bs.Name == sub.Name && bs.Namespace == sub.Namespace {
			return "direct"
		}
	case rbacv1.UserKind:
		if sub.Kind == protocol.SubjectUser && bs.Name == sub.Name {
			return "direct"
		}
		// A ServiceAccount is also the user system:serviceaccount:<ns>:<name>.
		if sub.Kind == protocol.SubjectServiceAccount && bs.Name == "system:serviceaccount:"+sub.Namespace+":"+sub.Name {
			return "direct (as user " + bs.Name + ")"
		}
	case rbacv1.GroupKind:
		if sub.Kind == protocol.SubjectGroup && bs.Name == sub.Name {
			return "direct"
		}
		if slices.Contains(groups, bs.Name) {
			return "group " + bs.Name
		}
	}
	return ""
}

// action is what a check or who-can asks about.
type action struct {
	verb, group, resource, subresource, name, namespace string
}

func newAction(q *protocol.RBACQuery) action {
	res, sub, _ := strings.Cut(q.Resource, "/")
	return action{verb: q.Verb, group: q.APIGroup, resource: res, subresource: sub, name: q.ResourceName, namespace: q.Namespace}
}

// allows reports whether a rule allows the action; limited is true when it
// would, but only for the rule's resourceNames and no name was asked.
func allows(r rbacv1.PolicyRule, a action) (ok, limited bool) {
	if len(r.NonResourceURLs) > 0 && len(r.Resources) == 0 {
		return false, false
	}
	if !has(r.Verbs, a.verb) || !has(r.APIGroups, a.group) {
		return false, false
	}
	full := a.resource
	if a.subresource != "" {
		full += "/" + a.subresource
	}
	resOK := false
	for _, res := range r.Resources {
		if res == "*" || res == full || (a.subresource != "" && res == "*/"+a.subresource) {
			resOK = true
		}
	}
	if !resOK {
		return false, false
	}
	if len(r.ResourceNames) == 0 {
		return true, false
	}
	if a.name == "" {
		return false, true
	}
	return slices.Contains(r.ResourceNames, a.name), false
}

func has(list []string, v string) bool {
	return slices.Contains(list, v) || slices.Contains(list, "*")
}

// applies reports whether a grant's scope covers the queried namespace.
// A cluster-wide question ("" namespace) is covered only by
// ClusterRoleBindings.
func applies(scope, namespace string) bool {
	return scope == "cluster" || (namespace != "" && scope == namespace)
}

// Query answers one rbac.whocan request.
func (s *Snapshot) Query(q *protocol.RBACQuery) *protocol.RBACResult {
	res := &protocol.RBACResult{Mode: q.Mode(), Notes: []string{scopeNote}}
	switch res.Mode {
	case protocol.RBACWhoCan:
		s.whoCan(q, res)
	default:
		s.chain(q, res)
	}
	return res
}

func (s *Snapshot) chain(q *protocol.RBACQuery, res *protocol.RBACResult) {
	sub := q.Subject
	res.Subject, res.Groups = sub, Groups(sub)
	a := newAction(q)
	allowed := false
	risky := map[string]bool{}
	for _, b := range s.bindings() {
		via := ""
		for _, bs := range b.subjects {
			if v := matches(bs, sub, res.Groups); v != "" {
				via = v
				break
			}
		}
		if via == "" {
			continue
		}
		g := protocol.RBACGrant{Binding: b.id, Role: b.roleID, Scope: b.scope, Via: via, RoleMissing: b.missing}
		if b.missing {
			res.Notes = append(res.Notes, fmt.Sprintf("%s refers to %s, which does not exist: it grants nothing (and would grant whatever a new role of that name holds)", b.id, b.roleID))
		}
		if res.Mode == protocol.RBACCheck {
			if !applies(b.scope, a.namespace) {
				continue
			}
			for _, r := range b.rules {
				if ok, limited := allows(r, a); ok || limited {
					g.Rules = append(g.Rules, rule(r))
					if ok {
						allowed = true
					}
				}
			}
			if len(g.Rules) == 0 {
				continue
			}
		} else {
			for _, r := range b.rules {
				g.Rules = append(g.Rules, rule(r))
			}
			g.Risky = riskyRights(b.rules)
			for _, r := range g.Risky {
				risky[r] = true
			}
		}
		res.Grants = append(res.Grants, g)
	}
	if res.Mode == protocol.RBACCheck {
		res.Allowed = &allowed
		if !allowed && len(res.Grants) > 0 {
			res.Notes = append(res.Notes, "Rules below allow it only for the named objects (resourceNames); ask with resourceName to check one.")
		}
		return
	}
	for r := range risky {
		res.Risky = append(res.Risky, r)
	}
	slices.Sort(res.Risky)
}

func (s *Snapshot) whoCan(q *protocol.RBACQuery, res *protocol.RBACResult) {
	a := newAction(q)
	seen := map[string]bool{}
	for _, b := range s.bindings() {
		if !applies(b.scope, a.namespace) {
			continue
		}
		var names []string
		ok := false
		for _, r := range b.rules {
			yes, limited := allows(r, a)
			ok = ok || yes
			if limited {
				names = append(names, r.ResourceNames...)
			}
		}
		if !ok && len(names) == 0 {
			continue
		}
		if ok {
			names = nil
		}
		for _, bs := range b.subjects {
			w := protocol.RBACWho{Kind: bs.Kind, Name: bs.Name, Namespace: bs.Namespace, Everyone: bs.Kind == rbacv1.GroupKind && everyone[bs.Name],
				NamespaceWide: bs.Kind == rbacv1.GroupKind && strings.HasPrefix(bs.Name, "system:serviceaccounts:"),
				Binding:       b.id, Role: b.roleID, Scope: b.scope, ResourceNames: names}
			key := w.Kind + "|" + w.Namespace + "|" + w.Name + "|" + w.Binding
			if !seen[key] {
				seen[key] = true
				res.Subjects = append(res.Subjects, w)
			}
		}
	}
	slices.SortFunc(res.Subjects, func(x, y protocol.RBACWho) int {
		if x.Everyone != y.Everyone {
			if x.Everyone {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(b2i(len(x.ResourceNames) > 0), b2i(len(y.ResourceNames) > 0)), cmp.Compare(x.Kind, y.Kind),
			cmp.Compare(x.Namespace, y.Namespace), cmp.Compare(x.Name, y.Name), cmp.Compare(x.Binding, y.Binding))
	})
	if len(res.Subjects) > 0 && res.Subjects[0].Everyone {
		res.Notes = append(res.Notes, "A group that contains everyone ("+res.Subjects[0].Name+") can do this.")
	}
	for _, w := range res.Subjects {
		if w.NamespaceWide {
			res.Notes = append(res.Notes, "Every ServiceAccount in namespace "+strings.TrimPrefix(w.Name, "system:serviceaccounts:")+" can do this (group "+w.Name+").")
		}
	}
}

func rule(r rbacv1.PolicyRule) protocol.RBACRule {
	return protocol.RBACRule{Verbs: r.Verbs, APIGroups: r.APIGroups, Resources: r.Resources, ResourceNames: r.ResourceNames, NonResourceURLs: r.NonResourceURLs}
}

// riskyRights names the rights in rules that are as strong as admin in
// their scope: they reveal every Secret, run code as other identities, or
// change RBAC itself.
func riskyRights(rules []rbacv1.PolicyRule) []string {
	found := map[string]bool{}
	can := func(verb, group, resource string) bool {
		res, sub, _ := strings.Cut(resource, "/")
		for _, r := range rules {
			if ok, _ := allows(r, action{verb: verb, group: group, resource: res, subresource: sub}); ok {
				return true
			}
		}
		return false
	}
	for _, r := range rules {
		if slices.Contains(r.Verbs, "*") || slices.Contains(r.Resources, "*") {
			found["wildcard"] = true
		}
	}
	checks := []struct {
		name   string
		verbs  []string
		group  string
		things []string
	}{
		{"read secrets", []string{"get", "list", "watch"}, "", []string{"secrets"}},
		{"create pods", []string{"create"}, "", []string{"pods"}},
		{"create pods", []string{"create"}, "apps", []string{"deployments", "daemonsets", "statefulsets", "replicasets"}},
		{"create pods", []string{"create"}, "batch", []string{"jobs", "cronjobs"}},
		{"exec into pods", []string{"create", "get"}, "", []string{"pods/exec", "pods/attach"}},
		{"change RBAC", []string{"create", "update", "patch"}, "rbac.authorization.k8s.io", []string{"roles", "rolebindings", "clusterroles", "clusterrolebindings"}},
		{"bind or escalate", []string{"bind", "escalate"}, "rbac.authorization.k8s.io", []string{"roles", "clusterroles"}},
		{"impersonate", []string{"impersonate"}, "", []string{"users", "groups", "serviceaccounts"}},
		{"nodes/proxy", []string{"get", "create"}, "", []string{"nodes/proxy"}},
		{"create tokens", []string{"create"}, "", []string{"serviceaccounts/token"}},
		{"impersonate", []string{"impersonate"}, "authentication.k8s.io", []string{"userextras", "uids"}},
		// A changed pod template or pod runs any image as any ServiceAccount
		// of the namespace and mounts any Secret: as strong as create pods.
		{"change workloads", []string{"update", "patch"}, "", []string{"pods", "pods/ephemeralcontainers"}},
		{"change workloads", []string{"update", "patch"}, "apps", []string{"deployments", "daemonsets", "statefulsets", "replicasets"}},
		{"change workloads", []string{"update", "patch"}, "batch", []string{"jobs", "cronjobs"}},
		{"change admission control", []string{"create", "update", "patch", "delete"}, "admissionregistration.k8s.io",
			[]string{"mutatingwebhookconfigurations", "validatingwebhookconfigurations", "validatingadmissionpolicies", "validatingadmissionpolicybindings"}},
		{"create persistentvolumes", []string{"create"}, "", []string{"persistentvolumes"}},
		{"change nodes", []string{"update", "patch"}, "", []string{"nodes"}},
	}
	for _, c := range checks {
		for _, v := range c.verbs {
			for _, t := range c.things {
				if can(v, c.group, t) {
					found[c.name] = true
				}
			}
		}
	}
	// Approving a CSR needs both the approval subresource and approve on
	// the signer; then any client certificate, for any user or group.
	if can("update", "certificates.k8s.io", "certificatesigningrequests/approval") && can("approve", "certificates.k8s.io", "signers") {
		found["approve certificates"] = true
	}
	out := make([]string, 0, len(found))
	for k := range found {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
