// Package identity is how a node agent proves which node it runs on (A8,
// KM-68). The kubelet mounts a projected ServiceAccount token into the node
// agent pod: audience "kubemap-portal", bound to the pod and its node, valid
// for about an hour and rotated by the kubelet. The node agent sends it in
// register; the portal asks the cluster agent to check it with a TokenReview
// and accepts only a token for this node (protocol.TokenReviewResult.AcceptNode).
// There is no shared secret to store, copy or rotate by hand.
//
// This package reads the token, checks it against the node name the pod
// was given, builds the register message, and says when to re-register
// before the token expires (transport rule 6 in docs/protocol.md).
package identity

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"kubemap/pkg/protocol"
)

// DefaultTokenPath is where the node agent DaemonSet mounts the projected
// token (see agent/install, the node DaemonSet).
const DefaultTokenPath = "/var/run/secrets/kubemap/token"

// maxTokenSize bounds the token file; real ones are about 1 KiB.
const maxTokenSize = 16 << 10

// Claims are the parts of a projected ServiceAccount token the node agent
// looks at. The node agent does not verify the signature: it only uses the
// claims to catch mistakes early and to plan re-registration. The portal's
// TokenReview is what proves the token.
type Claims struct {
	Audience  []string
	Subject   string
	IssuedAt  time.Time
	NotBefore time.Time
	Expiry    time.Time
	Namespace string
	Pod       string
	PodUID    string
	Node      string
	NodeUID   string
	SA        string
}

type rawClaims struct {
	Aud any    `json:"aud"`
	Sub string `json:"sub"`
	Iat int64  `json:"iat"`
	Nbf int64  `json:"nbf"`
	Exp int64  `json:"exp"`
	K8s struct {
		Namespace string `json:"namespace"`
		Pod       struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"pod"`
		Node struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"node"`
		ServiceAccount struct {
			Name string `json:"name"`
		} `json:"serviceaccount"`
	} `json:"kubernetes.io"`
}

// ParseClaims decodes a JWT's payload without checking its signature.
func ParseClaims(token string) (*Claims, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil, errors.New("token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, fmt.Errorf("token payload: %w", err)
	}
	var r rawClaims
	if err := json.Unmarshal(payload, &r); err != nil {
		return nil, fmt.Errorf("token payload: %w", err)
	}
	c := &Claims{
		Subject: r.Sub, Namespace: r.K8s.Namespace,
		Pod: r.K8s.Pod.Name, PodUID: r.K8s.Pod.UID,
		Node: r.K8s.Node.Name, NodeUID: r.K8s.Node.UID, SA: r.K8s.ServiceAccount.Name,
	}
	switch a := r.Aud.(type) {
	case string:
		c.Audience = []string{a}
	case []any:
		for _, v := range a {
			if s, ok := v.(string); ok {
				c.Audience = append(c.Audience, s)
			}
		}
	}
	unix := func(s int64) time.Time {
		if s == 0 {
			return time.Time{}
		}
		return time.Unix(s, 0).UTC()
	}
	c.IssuedAt, c.NotBefore, c.Expiry = unix(r.Iat), unix(r.Nbf), unix(r.Exp)
	return c, nil
}

// Identity is a node agent's identity for one registration.
type Identity struct {
	ClusterID string
	NodeName  string
	Token     string
	Claims    *Claims
}

// Config says where the identity comes from. In the pod, ClusterID and
// NodeName come from environment variables set by the manifest: CLUSTER_ID
// from the kubemap-node-agent ConfigMap and NODE_NAME from the downward API
// (spec.nodeName).
type Config struct {
	TokenPath string // default DefaultTokenPath
	ClusterID string
	NodeName  string
}

// Load reads the token and checks it before it is sent: right audience, not
// expired, issued to the node agent's ServiceAccount, and bound to the node
// the pod is scheduled on. A mismatch here would only fail later at the
// portal with a bare "unauthorized", so it is reported with its reason now.
func Load(cfg Config, now time.Time) (*Identity, error) {
	if cfg.NodeName == "" {
		return nil, errors.New("NODE_NAME is not set: the node agent must get it from the downward API (spec.nodeName)")
	}
	if cfg.ClusterID == "" {
		return nil, errors.New("CLUSTER_ID is not set: it comes from the kubemap-node-agent ConfigMap of the import manifest")
	}
	path := cfg.TokenPath
	if path == "" {
		path = DefaultTokenPath
	}
	token, err := readToken(path)
	if err != nil {
		return nil, err
	}
	c, err := ParseClaims(token)
	if err != nil {
		return nil, err
	}
	if err := check(c, cfg.NodeName, now); err != nil {
		return nil, err
	}
	return &Identity{ClusterID: cfg.ClusterID, NodeName: cfg.NodeName, Token: token, Claims: c}, nil
}

func readToken(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("node identity token: %w (is the projected token volume mounted?)", err)
	}
	defer f.Close()
	b := make([]byte, maxTokenSize+1)
	n, _ := f.Read(b)
	if n == 0 {
		return "", fmt.Errorf("node identity token %s is empty", path)
	}
	if n > maxTokenSize {
		return "", fmt.Errorf("node identity token %s is larger than %d bytes", path, maxTokenSize)
	}
	return strings.TrimSpace(string(b[:n])), nil
}

// check applies the same rules as the portal's AcceptNode, as far as the
// token itself can tell, plus time validity.
func check(c *Claims, nodeName string, now time.Time) error {
	hasAudience := false
	for _, a := range c.Audience {
		if a == protocol.NodeTokenAudience {
			hasAudience = true
		}
	}
	switch {
	case !hasAudience:
		return fmt.Errorf("token audience is %v, want %q: the projected volume needs audience: %s", c.Audience, protocol.NodeTokenAudience, protocol.NodeTokenAudience)
	case c.Subject != protocol.NodeAgentUsername:
		return fmt.Errorf("token belongs to %q, want %q: wrong ServiceAccount or namespace", c.Subject, protocol.NodeAgentUsername)
	case c.Node == "":
		return errors.New("token has no node claim: Kubernetes 1.30 or newer binds projected tokens to the node")
	case c.Node != nodeName:
		return fmt.Errorf("token is bound to node %q but NODE_NAME is %q", c.Node, nodeName)
	case !c.Expiry.IsZero() && !now.Before(c.Expiry):
		return fmt.Errorf("token expired at %s: the kubelet should have rotated it", c.Expiry.Format(time.RFC3339))
	case !c.NotBefore.IsZero() && now.Add(time.Minute).Before(c.NotBefore):
		return fmt.Errorf("token not valid before %s: check the node clock", c.NotBefore.Format(time.RFC3339))
	}
	return nil
}

// Register returns the register message for this identity.
func (id *Identity) Register(agentVersion string) *protocol.Register {
	return &protocol.Register{
		Role: protocol.RoleNode, AgentVersion: agentVersion,
		ClusterID: id.ClusterID, NodeName: id.NodeName, ServiceAccountToken: id.Token,
	}
}

// RenewAt is when the node agent should open a new connection with a fresh
// token: at 80% of the token's lifetime, and at least 5 minutes before it
// expires, but never before half of its lifetime (so a short token cannot
// cause a reconnect loop). The kubelet rotates the file after 80% of the lifetime too, so
// the file then holds a new token. Without an expiry claim it is an hour
// from now.
func (id *Identity) RenewAt(now time.Time) time.Time {
	c := id.Claims
	if c == nil || c.Expiry.IsZero() {
		return now.Add(time.Hour)
	}
	start := c.IssuedAt
	if start.IsZero() || start.After(c.Expiry) {
		start = now
	}
	life := c.Expiry.Sub(start)
	at := start.Add(life * 8 / 10)
	latest := c.Expiry.Add(-5 * time.Minute)
	if half := start.Add(life / 2); latest.Before(half) {
		latest = half // a short token still gets half its life
	}
	if at.After(latest) {
		at = latest
	}
	if at.Before(now) {
		at = now
	}
	return at
}

// String describes the identity for logs, without the token.
func (id *Identity) String() string {
	exp := "none"
	if id.Claims != nil && !id.Claims.Expiry.IsZero() {
		exp = id.Claims.Expiry.Format(time.RFC3339)
	}
	return fmt.Sprintf("node %s in cluster %s (token expires %s)", id.NodeName, id.ClusterID, exp)
}
