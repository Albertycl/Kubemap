package protocol

import "strings"

// The rbac.whocan op (KM-34) answers RBAC questions from the RBAC objects in
// the cluster agent's cache, without asking the API server (no
// SubjectAccessReview, so it needs no extra permission):
//
//   - chain: a subject (ServiceAccount, user or group) → every binding that
//     applies to it, directly or through a group → the Role or ClusterRole →
//     its rules, with the rights that are as strong as admin flagged;
//   - check: may this subject do this verb on this resource here, and which
//     binding allows it;
//   - who can: which subjects may do this verb on this resource here.
//
// Request.RBAC selects the mode: a subject without a verb is a chain, a
// subject with a verb is a check, a verb without a subject is who-can.

// RBAC query modes.
const (
	RBACChain  = "chain"
	RBACCheck  = "check"
	RBACWhoCan = "whocan"
)

// RBAC subject kinds, as in RoleBinding subjects.
const (
	SubjectServiceAccount = "ServiceAccount"
	SubjectUser           = "User"
	SubjectGroup          = "Group"
)

// RBACQuery is Request.RBAC.
type RBACQuery struct {
	Subject *RBACSubject `json:"subject,omitempty"`
	// Verb and Resource ask about one action: "list" "pods",
	// "create" "pods/exec". APIGroup is "" for the core group.
	Verb     string `json:"verb,omitempty"`
	Resource string `json:"resource,omitempty"`
	APIGroup string `json:"apiGroup,omitempty"`
	// Namespace is where; empty means cluster-wide (cluster-scoped
	// resources, or a namespaced resource in all namespaces).
	Namespace string `json:"namespace,omitempty"`
	// ResourceName narrows to one object (rules with resourceNames).
	ResourceName string `json:"resourceName,omitempty"`
}

// RBACSubject is who is asking.
type RBACSubject struct {
	Kind      string `json:"kind"` // ServiceAccount, User or Group
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"` // ServiceAccount only
	// Groups are a user's groups, which only the authenticator knows; every
	// authenticated user is also in system:authenticated.
	Groups []string `json:"groups,omitempty"`
}

// Mode returns the query's mode.
func (q *RBACQuery) Mode() string {
	switch {
	case q.Subject != nil && q.Verb == "":
		return RBACChain
	case q.Subject != nil:
		return RBACCheck
	}
	return RBACWhoCan
}

func (q *RBACQuery) validate() error {
	switch {
	case q == nil:
		return errorf(CodeBadRequest, "request rbac.whocan: needs rbac")
	case q.Subject == nil && q.Verb == "":
		return errorf(CodeBadRequest, "request rbac.whocan: needs a subject, a verb and resource, or both")
	case (q.Verb == "") != (q.Resource == ""):
		return errorf(CodeBadRequest, "request rbac.whocan: verb and resource go together")
	case !validName(q.Namespace) || len(q.ResourceName) > 253 || len(q.Verb) > 64 || len(q.Resource) > 253 || len(q.APIGroup) > 253:
		return errorf(CodeBadRequest, "request rbac.whocan: invalid namespace, name, verb, resource or group")
	case strings.Count(q.Resource, "/") > 1 || strings.ContainsAny(q.Verb+q.Resource+q.APIGroup, " \t\n"):
		return errorf(CodeBadRequest, "request rbac.whocan: resource is \"resource\" or \"resource/subresource\"")
	}
	if s := q.Subject; s != nil {
		switch {
		case s.Kind != SubjectServiceAccount && s.Kind != SubjectUser && s.Kind != SubjectGroup:
			return errorf(CodeBadRequest, "request rbac.whocan: subject kind must be ServiceAccount, User or Group")
		case s.Name == "" || len(s.Name) > 253:
			return errorf(CodeBadRequest, "request rbac.whocan: subject needs a name")
		case s.Kind == SubjectServiceAccount && (s.Namespace == "" || !validName(s.Namespace) || !validName(s.Name)):
			return errorf(CodeBadRequest, "request rbac.whocan: a ServiceAccount needs a valid namespace and name")
		case len(s.Groups) > 64:
			return errorf(CodeBadRequest, "request rbac.whocan: at most 64 groups")
		}
	}
	return nil
}

// RBACResult is the response object of an rbac.whocan request.
type RBACResult struct {
	Mode    string       `json:"mode"` // chain, check or whocan
	Subject *RBACSubject `json:"subject,omitempty"`
	// Groups are the groups the subject is evaluated with: for a
	// ServiceAccount system:serviceaccounts, system:serviceaccounts:<ns> and
	// system:authenticated; for a user the given groups and
	// system:authenticated.
	Groups []string `json:"groups,omitempty"`
	// Allowed answers a check.
	Allowed *bool `json:"allowed,omitempty"`
	// Grants are the bindings that apply to the subject (chain), or those
	// that allow the action (check).
	Grants []RBACGrant `json:"grants,omitempty"`
	// Subjects answer who-can, sorted with everyone-groups first.
	Subjects []RBACWho `json:"subjects,omitempty"`
	// Risky summarises the admin-equivalent rights of a chain.
	Risky []string `json:"risky,omitempty"`
	// Notes say what the answer does not cover and name bindings to roles
	// that do not exist.
	Notes []string `json:"notes,omitempty"`
}

// RBACGrant is one binding and the role it grants.
type RBACGrant struct {
	Binding string `json:"binding"` // "RoleBinding/ns/name" or "ClusterRoleBinding//name"
	Role    string `json:"role"`    // "Role/ns/name" or "ClusterRole//name"
	// Scope is the namespace the grant applies in, or "cluster" for a
	// ClusterRoleBinding.
	Scope string `json:"scope"`
	// Via is "direct", or the group the subject matched ("group
	// system:authenticated").
	Via         string     `json:"via"`
	RoleMissing bool       `json:"roleMissing,omitempty"`
	Rules       []RBACRule `json:"rules,omitempty"`
	// Risky lists this grant's admin-equivalent rights: "read secrets",
	// "create pods", "exec into pods", "change RBAC", "bind or escalate",
	// "impersonate", "nodes/proxy", "wildcard".
	Risky []string `json:"risky,omitempty"`
}

// RBACRule is one PolicyRule.
type RBACRule struct {
	Verbs           []string `json:"verbs"`
	APIGroups       []string `json:"apiGroups,omitempty"`
	Resources       []string `json:"resources,omitempty"`
	ResourceNames   []string `json:"resourceNames,omitempty"`
	NonResourceURLs []string `json:"nonResourceURLs,omitempty"`
}

// RBACWho is one subject that may do the action.
type RBACWho struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	// Everyone marks the groups that contain every account or user:
	// system:authenticated, system:unauthenticated, system:serviceaccounts.
	Everyone bool `json:"everyone,omitempty"`
	// NamespaceWide marks system:serviceaccounts:<ns>, every ServiceAccount
	// of one namespace.
	NamespaceWide bool   `json:"namespaceWide,omitempty"`
	Binding       string `json:"binding"`
	Role          string `json:"role"`
	Scope         string `json:"scope"`
	// ResourceNames is set when the rule allows only these objects.
	ResourceNames []string `json:"resourceNames,omitempty"`
}
