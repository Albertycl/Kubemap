package identity

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kubemap/pkg/protocol"
)

var now = time.Date(2026, 9, 27, 16, 0, 0, 0, time.UTC)

// token builds an unsigned JWT with the claims a kubelet-issued projected
// token carries. The node agent never checks signatures; the portal's
// TokenReview does.
func token(t *testing.T, edit func(m map[string]any)) string {
	t.Helper()
	claims := map[string]any{
		"aud": []any{protocol.NodeTokenAudience},
		"sub": protocol.NodeAgentUsername,
		"iat": now.Add(-10 * time.Minute).Unix(),
		"nbf": now.Add(-10 * time.Minute).Unix(),
		"exp": now.Add(50 * time.Minute).Unix(),
		"kubernetes.io": map[string]any{
			"namespace":      protocol.NodeAgentNamespace,
			"node":           map[string]any{"name": "k8server", "uid": "node-uid"},
			"pod":            map[string]any{"name": "kubemap-node-agent-x7k2p", "uid": "pod-uid"},
			"serviceaccount": map[string]any{"name": protocol.NodeAgentServiceAccount, "uid": "sa-uid"},
		},
	}
	if edit != nil {
		edit(claims)
	}
	payload, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256","kid":"example"}`)) + "." + enc(payload) + ".c2lnbmF0dXJlX2V4YW1wbGU"
}

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func cfg(path string) Config {
	return Config{TokenPath: path, ClusterID: "c-example", NodeName: "k8server"}
}

func TestLoadAndRegister(t *testing.T) {
	tok := token(t, nil)
	id, err := Load(cfg(write(t, tok+"\n")), now)
	if err != nil {
		t.Fatal(err)
	}
	c := id.Claims
	if c.Node != "k8server" || c.Pod != "kubemap-node-agent-x7k2p" || c.PodUID != "pod-uid" || c.SA != protocol.NodeAgentServiceAccount ||
		c.Namespace != protocol.NodeAgentNamespace || !c.Expiry.Equal(now.Add(50*time.Minute)) {
		t.Errorf("claims = %+v", c)
	}
	reg := id.Register("v0.1.0")
	if err := reg.Validate(); err != nil {
		t.Fatalf("register invalid: %v", err)
	}
	if reg.Role != protocol.RoleNode || reg.ServiceAccountToken != tok || reg.NodeName != "k8server" || reg.ClusterID != "c-example" {
		t.Errorf("register = %+v", reg)
	}
	// What the portal will do with the TokenReview result for this token.
	res := &protocol.TokenReviewResult{Authenticated: true, Audiences: c.Audience, Username: c.Subject, NodeName: c.Node}
	if err := res.AcceptNode(reg.NodeName); err != nil {
		t.Errorf("portal would reject: %v", err)
	}
	if s := id.String(); strings.Contains(s, tok) || !strings.Contains(s, "k8server") {
		t.Errorf("String leaks or lacks data: %s", s)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]struct {
		edit    func(m map[string]any)
		config  func(c *Config)
		content string
		want    string
	}{
		"no node name":     {config: func(c *Config) { c.NodeName = "" }, want: "NODE_NAME"},
		"no cluster id":    {config: func(c *Config) { c.ClusterID = "" }, want: "CLUSTER_ID"},
		"missing file":     {config: func(c *Config) { c.TokenPath = "/nonexistent/token" }, want: "projected token volume"},
		"empty file":       {content: "", want: "empty"},
		"too big":          {content: strings.Repeat("a", maxTokenSize+10), want: "larger than"},
		"not a jwt":        {content: "tok_example", want: "not a JWT"},
		"bad payload":      {content: "a.!!!.c", want: "payload"},
		"payload not json": {content: "a." + base64.RawURLEncoding.EncodeToString([]byte("[")) + ".c", want: "payload"},
		"default audience": {edit: func(m map[string]any) { m["aud"] = "https://kubernetes.default.svc" }, want: "audience"},
		"other sa":         {edit: func(m map[string]any) { m["sub"] = "system:serviceaccount:default:default" }, want: "wrong ServiceAccount"},
		"no node claim": {edit: func(m map[string]any) {
			delete(m["kubernetes.io"].(map[string]any), "node")
		}, want: "no node claim"},
		"other node": {edit: func(m map[string]any) {
			m["kubernetes.io"].(map[string]any)["node"] = map[string]any{"name": "node-2"}
		}, want: `bound to node "node-2"`},
		"expired": {edit: func(m map[string]any) { m["exp"] = now.Add(-time.Second).Unix() }, want: "expired"},
		"not yet": {edit: func(m map[string]any) { m["nbf"] = now.Add(time.Hour).Unix() }, want: "node clock"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			content := c.content
			if content == "" && c.edit != nil {
				content = token(t, c.edit)
			} else if content == "" && name != "empty file" {
				content = token(t, nil)
			}
			conf := cfg(write(t, content))
			if c.config != nil {
				c.config(&conf)
			}
			_, err := Load(conf, now)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want %q", err, c.want)
			}
			if err != nil && strings.Contains(err.Error(), "eyJ") {
				t.Errorf("error leaks the token: %v", err)
			}
		})
	}
}

func TestSingleStringAudience(t *testing.T) {
	c, err := ParseClaims(token(t, func(m map[string]any) { m["aud"] = protocol.NodeTokenAudience }))
	if err != nil || len(c.Audience) != 1 || c.Audience[0] != protocol.NodeTokenAudience {
		t.Errorf("claims = %+v, %v", c, err)
	}
	c, _ = ParseClaims(token(t, func(m map[string]any) { delete(m, "iat"); delete(m, "exp") }))
	if !c.IssuedAt.IsZero() || !c.Expiry.IsZero() {
		t.Error("missing times must stay zero")
	}
}

func TestRenewAt(t *testing.T) {
	id := func(iat, exp time.Duration) *Identity {
		c := &Claims{Expiry: now.Add(exp)}
		if iat != 0 {
			c.IssuedAt = now.Add(iat)
		}
		return &Identity{Claims: c}
	}
	cases := []struct {
		name string
		id   *Identity
		want time.Time
	}{
		// 1 h token issued 10 min ago: 80% is 48 min after issue = 38 min from now.
		{"1h token", id(-10*time.Minute, 50*time.Minute), now.Add(38 * time.Minute)},
		// 10 min token: 80% = 8 min, but at least 5 min before expiry = 5 min.
		{"short token", id(-0, 10*time.Minute), now.Add(5 * time.Minute)},
		// Already past the renew point: now.
		{"late", id(-55*time.Minute, 5*time.Minute), now},
		// 1 s token: 5 min before expiry is in the past, so half its life.
		{"tiny token", id(-0, time.Second), now.Add(500 * time.Millisecond)},
		// No expiry: an hour.
		{"no expiry", &Identity{Claims: &Claims{}}, now.Add(time.Hour)},
		{"no claims", &Identity{}, now.Add(time.Hour)},
	}
	for _, c := range cases {
		if got := c.id.RenewAt(now); !got.Equal(c.want) {
			t.Errorf("%s: RenewAt = %s, want %s", c.name, got.Sub(now), c.want.Sub(now))
		}
	}
}

func TestDefaultPath(t *testing.T) {
	_, err := Load(Config{ClusterID: "c", NodeName: "n"}, now)
	if err == nil || !strings.Contains(err.Error(), DefaultTokenPath) {
		t.Errorf("err = %v", err)
	}
}
