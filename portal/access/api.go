// Package access serves the RBAC and Argo CD views (KM-37):
//
//   - an account's rights: its roles through its bindings (rbac.whocan chain);
//   - may this account do this (check), and who can do this (who-can);
//   - a namespace's permissions: who can do the risky actions there (the
//     monitoring namespace by default), and what its own accounts may do;
//   - Argo CD applications with their managed resources, sync and health.
//
// Every RBAC answer comes from the cluster agent's rbac.whocan op (KM-34),
// which reads the RBAC objects in its cache. Argo CD applications are read
// with the list op; the agent caches them when Argo CD is installed.
package access

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/agentcall"
	"kubemap/portal/httpjson"
	"kubemap/portal/resources"
	"kubemap/portal/store"
)

const (
	askTimeout      = 15 * time.Second
	namespaceBudget = 60 * time.Second
	parallel        = 4
	// maxAccounts bounds the chains a namespace view asks for.
	maxAccounts = 40
)

// API serves (behind the login):
//
//	GET /api/clusters/{id}/rbac?subjectKind=&subject=&subjectNamespace=&groups=&verb=&resource=&apiGroup=&namespace=&resourceName=
//	GET /api/clusters/{id}/rbac/accounts
//	GET /api/clusters/{id}/rbac/namespaces/{ns}
//	GET /api/clusters/{id}/argocd
type API struct {
	store store.Store
	agent agentcall.Requester
}

// NewAPI returns the RBAC and Argo CD API.
func NewAPI(st store.Store, agent agentcall.Requester) *API { return &API{store: st, agent: agent} }

// Routes registers the API; require wraps every route (auth.Service.Require).
func (a *API) Routes(require func(http.Handler) http.Handler) func(*http.ServeMux) {
	return func(mux *http.ServeMux) {
		mux.Handle("GET /api/clusters/{id}/rbac", require(http.HandlerFunc(a.query)))
		mux.Handle("GET /api/clusters/{id}/rbac/accounts", require(http.HandlerFunc(a.accounts)))
		mux.Handle("GET /api/clusters/{id}/rbac/namespaces/{ns}", require(http.HandlerFunc(a.namespace)))
		mux.Handle("GET /api/clusters/{id}/argocd", require(http.HandlerFunc(a.argocd)))
	}
}

// ask runs one rbac.whocan. An agent error is returned as *protocol.Error.
func (a *API) ask(ctx context.Context, clusterID string, q *protocol.RBACQuery) (*protocol.RBACResult, error) {
	req := &protocol.Request{Op: protocol.OpRBACWhoCan, RBAC: q}
	if err := req.Validate(); err != nil {
		return nil, protocol.AsError(err)
	}
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	resp, err := a.agent.Request(ctx, clusterID, req)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	var res protocol.RBACResult
	if len(resp.Object) == 0 || json.Unmarshal(resp.Object, &res) != nil {
		return nil, &protocol.Error{Code: protocol.CodeInternal, Message: "unreadable rbac.whocan answer"}
	}
	return &res, nil
}

var groupRE = regexp.MustCompile(`^[A-Za-z0-9:._@-]{1,253}$`)

// query answers one question. The mode follows from the fields, as in the
// op: a subject alone is its chain, a subject and an action is a check, an
// action alone is who-can.
func (a *API) query(w http.ResponseWriter, r *http.Request) {
	c, ok := agentcall.Cluster(w, r, a.store)
	if !ok {
		return
	}
	v := r.URL.Query()
	for _, k := range []string{"namespace", "subjectNamespace"} {
		if ns := v.Get(k); ns != "" && !nsRE.MatchString(ns) {
			httpjson.Error(w, http.StatusBadRequest, "bad_request", k+" is not a namespace name")
			return
		}
	}
	q := &protocol.RBACQuery{
		Verb: v.Get("verb"), Resource: v.Get("resource"), APIGroup: v.Get("apiGroup"),
		Namespace: v.Get("namespace"), ResourceName: v.Get("resourceName"),
	}
	if name := v.Get("subject"); name != "" || v.Get("subjectKind") != "" {
		s := &protocol.RBACSubject{Kind: v.Get("subjectKind"), Name: name, Namespace: v.Get("subjectNamespace")}
		for _, g := range strings.Split(v.Get("groups"), ",") {
			if g = strings.TrimSpace(g); g == "" {
				continue
			}
			if !groupRE.MatchString(g) {
				httpjson.Error(w, http.StatusBadRequest, "bad_request", "a group name has characters a group cannot have")
				return
			}
			s.Groups = append(s.Groups, g)
		}
		q.Subject = s
	}
	res, err := a.ask(r.Context(), c.ID, q)
	if err != nil {
		agentcall.Error(w, err, "narrow the question")
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"result": res, "at": time.Now().UTC()})
}

// Account is a ServiceAccount the pickers offer.
type Account struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (a *API) listAccounts(ctx context.Context, clusterID, ns string) ([]Account, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, askTimeout)
	defer cancel()
	items, truncated, err := resources.ListAll(ctx, a.agent, clusterID, protocol.Request{Resource: "serviceaccounts", Namespace: ns})
	if err != nil {
		return nil, false, err
	}
	out := make([]Account, 0, len(items))
	for _, raw := range items {
		var o struct {
			Metadata struct{ Namespace, Name string } `json:"metadata"`
		}
		if json.Unmarshal(raw, &o) == nil && o.Metadata.Name != "" {
			out = append(out, Account{Namespace: o.Metadata.Namespace, Name: o.Metadata.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, truncated, nil
}

func (a *API) accounts(w http.ResponseWriter, r *http.Request) {
	c, ok := agentcall.Cluster(w, r, a.store)
	if !ok {
		return
	}
	out, truncated, err := a.listAccounts(r.Context(), c.ID, "")
	if err != nil {
		agentcall.Error(w, err, "choose one namespace")
		return
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"accounts": out, "truncated": truncated})
}

// Action is one column of the namespace view.
type Action struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Verb     string `json:"verb"`
	Resource string `json:"resource"`
	APIGroup string `json:"apiGroup,omitempty"`
	// Why says what an account with this right can do.
	Why string `json:"why"`
}

// Actions are the questions the namespace view asks: the rights an attacker
// or a mistake would use first.
var Actions = []Action{
	{Key: "get-secrets", Label: "Read Secrets", Verb: "get", Resource: "secrets", Why: "reads passwords, tokens and keys"},
	{Key: "list-secrets", Label: "List Secrets", Verb: "list", Resource: "secrets", Why: "list returns every Secret with its data"},
	{Key: "create-pods", Label: "Create pods", Verb: "create", Resource: "pods", Why: "runs any image with any ServiceAccount of the namespace"},
	{Key: "exec", Label: "Exec into pods", Verb: "create", Resource: "pods/exec", Why: "a shell in a running container, with its token and files"},
	{Key: "delete-pods", Label: "Delete pods", Verb: "delete", Resource: "pods", Why: "stops workloads"},
	{Key: "patch-deployments", Label: "Change Deployments", Verb: "patch", Resource: "deployments", APIGroup: "apps", Why: "changes images and commands of running apps"},
	{Key: "update-configmaps", Label: "Change ConfigMaps", Verb: "update", Resource: "configmaps", Why: "changes app and dashboard configuration"},
	{Key: "create-rolebindings", Label: "Grant roles", Verb: "create", Resource: "rolebindings", APIGroup: "rbac.authorization.k8s.io", Why: "gives any account more rights"},
}

// Who is one row of the namespace view: a subject and the actions it may do.
type Who struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Everyone  bool   `json:"everyone,omitempty"`
	// Grants maps an action key to how the subject has it
	// ("ClusterRoleBinding//x → ClusterRole//y"; "only named: a, b").
	Grants map[string]Grant `json:"grants"`
}

// Grant is how a subject holds one action.
type Grant struct {
	Binding       string   `json:"binding"`
	Role          string   `json:"role"`
	Scope         string   `json:"scope"`
	ResourceNames []string `json:"resourceNames,omitempty"`
}

// AccountRights is one ServiceAccount of the namespace and its risky rights
// anywhere in the cluster.
type AccountRights struct {
	Name string `json:"name"`
	// Risky are the rights this account has through its own bindings (to
	// itself, or to its namespace's group).
	Risky []string `json:"risky"`
	// Everyone are risky rights this account has only because every
	// account has them (a grant to system:authenticated,
	// system:serviceaccounts …). They are the most dangerous kind: shown
	// loudly, and repeated at the top of the namespace view.
	Everyone []string `json:"everyone,omitempty"`
	// Platform are risky rights from known platform grants to everyone
	// (PlatformGrants), shown quietly with the reason.
	Platform []string `json:"platform,omitempty"`
	Grants   int      `json:"grants"`
	Error    string   `json:"error,omitempty"`
}

// everyoneGroups contain every account or user.
var everyoneGroups = map[string]bool{
	"group system:authenticated": true, "group system:unauthenticated": true, "group system:serviceaccounts": true,
}

// PlatformGrants are known grants to everyone that are not admin-equivalent,
// by binding, with the reason (review KM-37 N-1). Any other risky grant to
// everyone is reported loudly.
var PlatformGrants = map[string]string{
	"ClusterRoleBinding//calico-tiered-policy-passthrough": "Calico's tier passthrough: * on Calico's own policy resources; Calico's API server still checks tiers",
}

// splitRisky separates a chain's risky rights: the account's own, those it
// has because every account has them, and those from known platform grants.
func splitRisky(grants []protocol.RBACGrant) (own, everyone, platform []string) {
	ownSet, allSet, platSet := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, g := range grants {
		for _, r := range g.Risky {
			switch {
			case !everyoneGroups[g.Via]:
				ownSet[r] = true
			case PlatformGrants[g.Binding] != "":
				platSet[r] = true
			default:
				allSet[r] = true
			}
		}
	}
	list := func(set map[string]bool, skip ...map[string]bool) []string {
		var out []string
	next:
		for r := range set {
			for _, s := range skip {
				if s[r] {
					continue next
				}
			}
			out = append(out, r)
		}
		sort.Strings(out)
		return out
	}
	own = list(ownSet)
	if own == nil {
		own = []string{}
	}
	return own, list(allSet, ownSet), list(platSet, ownSet, allSet)
}

// NamespaceView answers GET .../rbac/namespaces/{ns}.
type NamespaceView struct {
	Namespace string            `json:"namespace"`
	Actions   []Action          `json:"actions"`
	Subjects  []Who             `json:"subjects"`
	Errors    map[string]string `json:"errors,omitempty"` // action key → why it has no answer
	Accounts  []AccountRights   `json:"accounts"`
	// EveryoneRisky are the risky rights every account in the cluster has
	// (not counting known platform grants): the first thing to fix.
	EveryoneRisky []string `json:"everyoneRisky,omitempty"`
	Notes         []string `json:"notes,omitempty"`
}

var nsRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

func (a *API) namespace(w http.ResponseWriter, r *http.Request) {
	c, ok := agentcall.Cluster(w, r, a.store)
	if !ok {
		return
	}
	ns := r.PathValue("ns")
	if !nsRE.MatchString(ns) {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", "not a namespace name")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), namespaceBudget)
	defer cancel()

	// The first question also tells whether the agent can answer at all.
	first, err := a.ask(ctx, c.ID, actionQuery(Actions[0], ns))
	if err != nil {
		agentcall.Error(w, err, "narrow the question")
		return
	}
	view := NamespaceView{Namespace: ns, Actions: Actions, Subjects: []Who{}, Accounts: []AccountRights{}, Notes: first.Notes}
	results := make([]*protocol.RBACResult, len(Actions))
	errs := make([]error, len(Actions))
	results[0] = first
	accts, _, accErr := a.listAccounts(ctx, c.ID, ns)
	if len(accts) > maxAccounts {
		view.Notes = append(view.Notes, "Only the first 40 ServiceAccounts of the namespace are shown.")
		accts = accts[:maxAccounts]
	}
	chains := make([]*protocol.RBACResult, len(accts))
	chainErrs := make([]error, len(accts))

	var wg sync.WaitGroup
	sem := make(chan struct{}, parallel)
	run := func(f func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			f()
		}()
	}
	for i := 1; i < len(Actions); i++ {
		run(func() { results[i], errs[i] = a.ask(ctx, c.ID, actionQuery(Actions[i], ns)) })
	}
	for i, acc := range accts {
		run(func() {
			chains[i], chainErrs[i] = a.ask(ctx, c.ID, &protocol.RBACQuery{Subject: &protocol.RBACSubject{
				Kind: protocol.SubjectServiceAccount, Name: acc.Name, Namespace: ns}})
		})
	}
	wg.Wait()

	rows := map[string]*Who{}
	var order []string
	for i, res := range results {
		if errs[i] != nil {
			if view.Errors == nil {
				view.Errors = map[string]string{}
			}
			view.Errors[Actions[i].Key] = errorText(errs[i])
			continue
		}
		for _, s := range res.Subjects {
			k := s.Kind + "/" + s.Namespace + "/" + s.Name
			row := rows[k]
			if row == nil {
				row = &Who{Kind: s.Kind, Name: s.Name, Namespace: s.Namespace, Everyone: s.Everyone, Grants: map[string]Grant{}}
				rows[k] = row
				order = append(order, k)
			}
			// The first grant the agent names is kept; an unrestricted one
			// wins over one for named objects only.
			if g, seen := row.Grants[Actions[i].Key]; !seen || (len(g.ResourceNames) > 0 && len(s.ResourceNames) == 0) {
				row.Grants[Actions[i].Key] = Grant{Binding: s.Binding, Role: s.Role, Scope: s.Scope, ResourceNames: s.ResourceNames}
			}
		}
	}
	for _, k := range order {
		view.Subjects = append(view.Subjects, *rows[k])
	}
	// Everyone-groups first, then the subjects with the most rights.
	sort.SliceStable(view.Subjects, func(i, j int) bool {
		a, b := view.Subjects[i], view.Subjects[j]
		if a.Everyone != b.Everyone {
			return a.Everyone
		}
		return len(a.Grants) > len(b.Grants)
	})
	if accErr != nil {
		view.Notes = append(view.Notes, "The namespace's ServiceAccounts could not be listed: "+errorText(accErr))
	}
	for i, acc := range accts {
		ar := AccountRights{Name: acc.Name, Risky: []string{}}
		if chainErrs[i] != nil {
			ar.Error = errorText(chainErrs[i])
		} else {
			ar.Grants = len(chains[i].Grants)
			ar.Risky, ar.Everyone, ar.Platform = splitRisky(chains[i].Grants)
		}
		view.Accounts = append(view.Accounts, ar)
	}
	every := map[string]bool{}
	for _, ar := range view.Accounts {
		for _, r := range ar.Everyone {
			if !every[r] {
				every[r] = true
				view.EveryoneRisky = append(view.EveryoneRisky, r)
			}
		}
	}
	sort.Strings(view.EveryoneRisky)
	sort.SliceStable(view.Accounts, func(i, j int) bool { return len(view.Accounts[i].Risky) > len(view.Accounts[j].Risky) })
	httpjson.Write(w, http.StatusOK, map[string]any{"view": view, "at": time.Now().UTC()})
}

func actionQuery(act Action, ns string) *protocol.RBACQuery {
	return &protocol.RBACQuery{Verb: act.Verb, Resource: act.Resource, APIGroup: act.APIGroup, Namespace: ns}
}

// errorText is the message a failed request shows in a view that reports
// several answers.
func errorText(err error) string {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		_, _, msg := agentcall.Status(pe, "narrow the question")
		return msg
	}
	_, _, msg := agentcall.TunnelStatus(err)
	return msg
}
