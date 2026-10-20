package main

import (
	"fmt"
	"slices"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
)

// Finding is one line of the audit.
type Finding struct {
	OK   bool
	What string
	Why  string
}

func (f Finding) String() string {
	mark := "ok  "
	if !f.OK {
		mark = "FAIL"
	}
	return fmt.Sprintf("%s  %s: %s", mark, f.What, f.Why)
}

var readVerbs = []string{"get", "list", "watch"}

// approvedSubresources are the only subresources a read-only rule may name.
// Others are not harmless reads: pods/proxy and services/proxy reach any pod
// or Service port through the API server, pods/exec and pods/attach enter
// containers, nodes/proxy and nodes/stats reach the kubelet.
var approvedSubresources = []string{"pods/log"}

// Audit checks every rule and binding in the files.
func Audit(o *Objects) []Finding {
	var out []Finding
	for _, name := range sortedKeys(o.ClusterRoles) {
		for i, r := range o.ClusterRoles[name].Rules {
			out = append(out, auditRule("ClusterRole "+name, i, r, ""))
		}
		if o.ClusterRoles[name].AggregationRule != nil {
			out = append(out, Finding{false, "ClusterRole " + name, "has an aggregationRule: its rules come from other roles and cannot be checked here"})
		}
	}
	for _, key := range sortedKeys(o.Roles) {
		ns, _, _ := strings.Cut(key, "/")
		for i, r := range o.Roles[key].Rules {
			out = append(out, auditRule("Role "+key, i, r, ns))
		}
	}
	allowed := func(s rbacv1.Subject, bindingNS string) bool {
		return slices.ContainsFunc(Agents, func(a Agent) bool {
			return s.Kind == rbacv1.ServiceAccountKind && subjectMatches(s, bindingNS, a)
		})
	}
	for _, b := range o.ClusterRoleBindings {
		out = append(out, auditBinding("ClusterRoleBinding "+b.Name, b.Subjects, "", b.RoleRef, o, allowed))
	}
	for _, b := range o.RoleBindings {
		out = append(out, auditBinding("RoleBinding "+b.Namespace+"/"+b.Name, b.Subjects, b.Namespace, b.RoleRef, o, allowed))
	}
	return out
}

func auditRule(owner string, i int, r rbacv1.PolicyRule, roleNS string) Finding {
	what := fmt.Sprintf("%s rule %d (%s on %s)", owner, i, strings.Join(r.Verbs, ","), describeResources(r))
	fail := func(why string) Finding { return Finding{false, what, why} }
	switch {
	case slices.Contains(r.Verbs, "*") || slices.Contains(r.APIGroups, "*") || slices.Contains(r.Resources, "*"):
		return fail("uses a wildcard")
	case len(r.NonResourceURLs) > 0:
		return fail("grants non-resource URLs")
	}
	for _, res := range r.Resources {
		if strings.Contains(res, "/") && !slices.Contains(approvedSubresources, res) {
			return fail("names subresource " + res + ", which is not approved (only " + strings.Join(approvedSubresources, ", ") + ")")
		}
	}
	readOnly := !slices.ContainsFunc(r.Verbs, func(v string) bool { return !slices.Contains(readVerbs, v) })
	secrets := slices.Contains(r.APIGroups, "") && slices.ContainsFunc(r.Resources, func(s string) bool {
		return s == "secrets" || strings.HasPrefix(s, "secrets/")
	})
	switch {
	case secrets:
		return fail("grants access to Secrets")
	case readOnly:
		return Finding{true, what, "read-only"}
	case slices.Equal(r.APIGroups, []string{"authentication.k8s.io"}) && slices.Equal(r.Resources, []string{"tokenreviews"}) &&
		slices.Equal(r.Verbs, []string{"create"}) && roleNS == "":
		return Finding{true, what, "approved exception: TokenReview for node agent identity (A8); stores nothing"}
	}
	return fail("has a write verb that is not an approved exception")
}

func auditBinding(what string, subjects []rbacv1.Subject, ns string, ref rbacv1.RoleRef, o *Objects,
	allowed func(rbacv1.Subject, string) bool) Finding {
	for _, s := range subjects {
		if !allowed(s, ns) {
			return Finding{false, what, fmt.Sprintf("binds %s %s, which is not one of the two agent ServiceAccounts", s.Kind, s.Name)}
		}
	}
	switch {
	case ref.Kind == "ClusterRole" && o.ClusterRoles[ref.Name] == nil:
		return Finding{false, what, "refers to ClusterRole " + ref.Name + ", which is not in the files (a cluster role could grant anything)"}
	case ref.Kind == "Role" && o.Roles[ns+"/"+ref.Name] == nil:
		return Finding{false, what, "refers to Role " + ns + "/" + ref.Name + ", which is not in the files"}
	}
	names := make([]string, 0, len(subjects))
	for _, s := range subjects {
		names = append(names, s.Namespace+"/"+s.Name)
	}
	return Finding{true, what, "binds " + ref.Kind + " " + ref.Name + " to " + strings.Join(names, ", ")}
}

func describeResources(r rbacv1.PolicyRule) string {
	var parts []string
	for _, g := range r.APIGroups {
		for _, res := range r.Resources {
			if g == "" {
				parts = append(parts, res)
			} else {
				parts = append(parts, res+"."+g)
			}
		}
	}
	s := strings.Join(parts, ",")
	if len(r.ResourceNames) > 0 {
		s += " names=" + strings.Join(r.ResourceNames, ",")
	}
	return s
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
