package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// Agent is one agent identity, with the groups its token carries.
type Agent struct {
	Short, Namespace, Name string
}

// Username is the ServiceAccount's user name.
func (a Agent) Username() string { return "system:serviceaccount:" + a.Namespace + ":" + a.Name }

// Groups are the groups every ServiceAccount token carries.
func (a Agent) Groups() []string {
	return []string{"system:serviceaccounts", "system:serviceaccounts:" + a.Namespace, "system:authenticated"}
}

// Agents are the two identities the bundle creates.
var Agents = []Agent{
	{Short: "cluster", Namespace: "kubemap-system", Name: "kubemap"},
	{Short: "node", Namespace: "kubemap-node", Name: "kubemap-node-agent"},
}

// Check is one line of agent-permission-checks.txt.
type Check struct {
	Expect, Agent, Namespace, Verb, Resource, Subresource, Name string
	Line                                                        int
}

// scopeNamespaces are the namespaces a "*" check is also asked in, besides
// every namespace at once: a RoleBinding only grants in its own namespace.
var scopeNamespaces = []string{"kubemap-system", "kubemap-node", "default", "kube-system"}

// Scopes returns the namespaces to check in; "" means every namespace.
func (c Check) Scopes() []string {
	if c.Namespace != "*" {
		return []string{c.Namespace}
	}
	if clusterScoped(c.Group(), c.Plural()) {
		return []string{""}
	}
	return append([]string{""}, scopeNamespaces...)
}

// AppliesTo reports whether the check is for this agent.
func (c Check) AppliesTo(a Agent) bool { return c.Agent == "*" || c.Agent == a.Short }

// Plural and Group split "deployments.apps" into "deployments" and "apps".
func (c Check) Plural() string { p, _, _ := strings.Cut(c.Resource, "."); return p }
func (c Check) Group() string  { _, g, _ := strings.Cut(c.Resource, "."); return g }

// Target is the resource as one word, for output.
func (c Check) Target() string {
	t := c.Resource
	if c.Subresource != "" {
		t += "/" + c.Subresource
	}
	if c.Name != "" {
		t += " name=" + c.Name
	}
	return t
}

func readChecks(path string) ([]Check, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Check
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fs := strings.Fields(line)
		if len(fs) != 7 {
			return nil, fmt.Errorf("%s:%d: want 7 columns, got %d", path, n, len(fs))
		}
		dash := func(s string) string {
			if s == "-" {
				return ""
			}
			return s
		}
		c := Check{Expect: fs[0], Agent: fs[1], Namespace: fs[2], Verb: fs[3], Resource: fs[4],
			Subresource: dash(fs[5]), Name: dash(fs[6]), Line: n}
		if c.Expect != "deny" && c.Expect != "allow" {
			return nil, fmt.Errorf("%s:%d: expect must be deny or allow", path, n)
		}
		if c.Agent != "*" && c.Agent != "cluster" && c.Agent != "node" {
			return nil, fmt.Errorf("%s:%d: agent must be cluster, node or *", path, n)
		}
		out = append(out, c)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, errors.New(path + ": no checks")
	}
	return out, nil
}

// clusterScoped lists the cluster-scoped resources the checks use. A
// RoleBinding can never grant these.
func clusterScoped(group, plural string) bool {
	switch group + "/" + plural {
	case "/nodes", "/namespaces", "/persistentvolumes", "/users", "/groups",
		"rbac.authorization.k8s.io/clusterroles", "rbac.authorization.k8s.io/clusterrolebindings",
		"apiextensions.k8s.io/customresourcedefinitions",
		"admissionregistration.k8s.io/mutatingwebhookconfigurations",
		"admissionregistration.k8s.io/validatingwebhookconfigurations",
		"authentication.k8s.io/tokenreviews":
		return true
	}
	return false
}

// Objects are the RBAC objects read from the files.
type Objects struct {
	ClusterRoles        map[string]*rbacv1.ClusterRole
	Roles               map[string]*rbacv1.Role // "ns/name"
	ClusterRoleBindings []*rbacv1.ClusterRoleBinding
	RoleBindings        []*rbacv1.RoleBinding
	ServiceAccounts     []string // "ns/name"
}

// ReadFile adds the RBAC objects of one multi-document YAML file.
func (o *Objects) ReadFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return o.Read(f, path)
}

// Read adds the RBAC objects of a multi-document YAML stream.
func (o *Objects) Read(r io.Reader, name string) error {
	if o.ClusterRoles == nil {
		o.ClusterRoles, o.Roles = map[string]*rbacv1.ClusterRole{}, map[string]*rbacv1.Role{}
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(r, 4096)
	for doc := 1; ; doc++ {
		var u unstructured.Unstructured
		if err := dec.Decode(&u.Object); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("%s: document %d: %w", name, doc, err)
		}
		if len(u.Object) == 0 {
			continue
		}
		conv := func(into any) error {
			if err := runtime.DefaultUnstructuredConverter.FromUnstructuredWithValidation(u.Object, into, true); err != nil {
				return fmt.Errorf("%s: document %d (%s %s): %w", name, doc, u.GetKind(), u.GetName(), err)
			}
			return nil
		}
		switch u.GetKind() {
		case "ClusterRole":
			var x rbacv1.ClusterRole
			if err := conv(&x); err != nil {
				return err
			}
			o.ClusterRoles[x.Name] = &x
		case "Role":
			var x rbacv1.Role
			if err := conv(&x); err != nil {
				return err
			}
			o.Roles[x.Namespace+"/"+x.Name] = &x
		case "ClusterRoleBinding":
			var x rbacv1.ClusterRoleBinding
			if err := conv(&x); err != nil {
				return err
			}
			o.ClusterRoleBindings = append(o.ClusterRoleBindings, &x)
		case "RoleBinding":
			var x rbacv1.RoleBinding
			if err := conv(&x); err != nil {
				return err
			}
			o.RoleBindings = append(o.RoleBindings, &x)
		case "ServiceAccount":
			o.ServiceAccounts = append(o.ServiceAccounts, u.GetNamespace()+"/"+u.GetName())
		}
	}
}

// subjectMatches applies the RBAC subject rules to an agent's identity.
func subjectMatches(s rbacv1.Subject, bindingNS string, a Agent) bool {
	switch s.Kind {
	case rbacv1.ServiceAccountKind:
		ns := s.Namespace
		if ns == "" {
			ns = bindingNS
		}
		return ns == a.Namespace && s.Name == a.Name
	case rbacv1.UserKind:
		return s.Name == a.Username()
	case rbacv1.GroupKind:
		return slices.Contains(a.Groups(), s.Name)
	}
	return false
}

type grant struct {
	rule      rbacv1.PolicyRule
	namespace string // "" = every namespace
	by        string // "ClusterRoleBinding x -> ClusterRole y"
}

// grants are the rules that apply to the agent, with where they apply.
func (o *Objects) grants(a Agent) []grant {
	var out []grant
	for _, b := range o.ClusterRoleBindings {
		if !slices.ContainsFunc(b.Subjects, func(s rbacv1.Subject) bool { return subjectMatches(s, "", a) }) {
			continue
		}
		if cr := o.ClusterRoles[b.RoleRef.Name]; cr != nil && b.RoleRef.Kind == "ClusterRole" {
			for _, r := range cr.Rules {
				out = append(out, grant{r, "", "ClusterRoleBinding " + b.Name + " -> ClusterRole " + cr.Name})
			}
		}
	}
	for _, b := range o.RoleBindings {
		if !slices.ContainsFunc(b.Subjects, func(s rbacv1.Subject) bool { return subjectMatches(s, b.Namespace, a) }) {
			continue
		}
		var rules []rbacv1.PolicyRule
		switch b.RoleRef.Kind {
		case "Role":
			if r := o.Roles[b.Namespace+"/"+b.RoleRef.Name]; r != nil {
				rules = r.Rules
			}
		case "ClusterRole":
			if r := o.ClusterRoles[b.RoleRef.Name]; r != nil {
				rules = r.Rules
			}
		}
		for _, r := range rules {
			out = append(out, grant{r, b.Namespace, "RoleBinding " + b.Namespace + "/" + b.Name + " -> " + b.RoleRef.Kind + " " + b.RoleRef.Name})
		}
	}
	return out
}

// Allowed evaluates one check in one namespace ("" = every namespace at once)
// and says which binding allowed it.
func (o *Objects) Allowed(a Agent, c Check, ns string) (bool, string) {
	cs := clusterScoped(c.Group(), c.Plural())
	for _, g := range o.grants(a) {
		if g.namespace != "" && (cs || ns == "" || g.namespace != ns) {
			continue // a RoleBinding grants only namespaced resources in its own namespace
		}
		if ruleMatches(g.rule, c) {
			return true, g.by
		}
	}
	return false, ""
}

func ruleMatches(r rbacv1.PolicyRule, c Check) bool {
	if !has(r.Verbs, c.Verb) || !has(r.APIGroups, c.Group()) {
		return false
	}
	res := c.Plural()
	if c.Subresource != "" {
		res += "/" + c.Subresource
	}
	if !slices.Contains(r.Resources, "*") && !slices.Contains(r.Resources, res) &&
		!(c.Subresource != "" && slices.Contains(r.Resources, "*/"+c.Subresource)) {
		return false
	}
	if len(r.ResourceNames) > 0 {
		return c.Name != "" && slices.Contains(r.ResourceNames, c.Name)
	}
	return true
}

func has(list []string, v string) bool { return slices.Contains(list, "*") || slices.Contains(list, v) }

func writeFile(path string, b []byte) error { return os.WriteFile(path, b, 0o644) }
