package protocol

import "strings"

// The object.chain op (KM-35) answers "what is this object connected to?"
// for any object in the relationship graph: its owners and what it owns,
// the pods it comes down to, and around those pods the Services, routes,
// configuration, volumes, ServiceAccounts, NetworkPolicies, autoscalers and
// nodes, plus the Services it calls and the workloads that call it. It is
// built from the agent's cache and the relationship graph (KM-32 to KM-52),
// with no Kubernetes call.

// Chain bounds.
const (
	// MaxChainItems bounds one section; the rest is counted in More.
	MaxChainItems = 30
	// MaxChainMatches bounds the answer to a query.
	MaxChainMatches = 20
	maxChainQuery   = 200
)

// Chain section keys, in the order the agent returns them.
const (
	ChainOwners          = "owners"          // owner references upwards, the top one first
	ChainOwned           = "owned"           // what it owns, downwards (ReplicaSets, Jobs ...), without pods
	ChainPods            = "pods"            // the pods the object comes down to
	ChainServices        = "services"        // Services that select those pods
	ChainRoutes          = "routes"          // Ingress and IngressRoute rules to those Services
	ChainConfig          = "config"          // ConfigMaps the pods use
	ChainSecrets         = "secrets"         // Secrets the pods refer to (names only; values are never read)
	ChainVolumes         = "volumes"         // PersistentVolumeClaims and their PersistentVolumes
	ChainServiceAccounts = "serviceaccounts" // what the pods run as
	ChainPolicies        = "policies"        // NetworkPolicies that isolate the pods
	ChainAutoscalers     = "autoscalers"     // HorizontalPodAutoscalers
	ChainNodes           = "nodes"           // where the pods run
	ChainCalls           = "calls"           // Services the workloads call (found in their configuration)
	ChainCalledBy        = "calledBy"        // workloads that call the Services
)

// ChainRequest names the object (a graph node ID: "Kind/namespace/name",
// "Kind//name" for cluster-scoped objects), or a text to find objects by.
type ChainRequest struct {
	ID    string `json:"id,omitempty"`
	Query string `json:"query,omitempty"`
}

func (c *ChainRequest) validate() error {
	switch {
	case c == nil || (c.ID == "") == (c.Query == ""):
		return errorf(CodeBadRequest, "request object.chain: needs exactly one of id and query")
	case len(c.ID) > maxAppIDLen || strings.ContainsAny(c.ID, " \t\n") || (c.ID != "" && strings.Count(c.ID, "/") != 2):
		return errorf(CodeBadRequest, "request object.chain: id must be Kind/namespace/name")
	case len(c.Query) > maxChainQuery || strings.ContainsAny(c.Query, "\n\r"):
		return errorf(CodeBadRequest, "request object.chain: query must be one line of at most %d bytes", maxChainQuery)
	}
	return nil
}

// ObjectChain is the response object of an object.chain request: the
// chain of one object, or, for a query, the objects that match.
type ObjectChain struct {
	Object   *ChainItem     `json:"object,omitempty"`
	Sections []ChainSection `json:"sections,omitempty"`
	// Apps are the app map apps the object or its workloads belong to.
	Apps []string `json:"apps,omitempty"`
	// Matches answer a query: objects whose ID contains the text.
	Matches []ChainItem `json:"matches,omitempty"`
	Notes   []string    `json:"notes,omitempty"`
}

// ChainSection is one kind of relation. Empty sections are left out.
type ChainSection struct {
	Key   string      `json:"key"`
	Items []ChainItem `json:"items"`
	More  int         `json:"more,omitempty"` // items left out beyond MaxChainItems
}

// ChainItem is one object in a chain.
type ChainItem struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Status    string `json:"status,omitempty"`
	Missing   bool   `json:"missing,omitempty"` // referred to, but not in the cluster
	// Detail says how it is related: "env DB_HOST", "shop.example.com/api",
	// "ingress, egress", "via ReplicaSet web-7d9f".
	Detail string `json:"detail,omitempty"`
}
