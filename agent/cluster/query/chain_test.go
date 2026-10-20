package query

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"kubemap/pkg/protocol"
)

func chainOf(t *testing.T, h *Handler, c *protocol.ChainRequest) (*protocol.ObjectChain, *protocol.Response) {
	t.Helper()
	r := do(h, &protocol.Request{Op: protocol.OpChain, Chain: c})
	if r.Error != nil {
		return nil, r
	}
	dec := json.NewDecoder(bytes.NewReader(r.Object))
	dec.DisallowUnknownFields()
	var res protocol.ObjectChain
	if err := dec.Decode(&res); err != nil {
		t.Fatal(err)
	}
	return &res, r
}

// The op builds the chain from the same analyses as the app map (KM-35).
func TestObjectChainOp(t *testing.T) {
	h, _, _ := newHandler(t, twoApps())
	c, r := chainOf(t, h, &protocol.ChainRequest{ID: "Deployment/sre-lab-app/url-shortener"})
	if c == nil {
		t.Fatalf("chain: %+v", r.Error)
	}
	got := map[string][]string{}
	for _, s := range c.Sections {
		for _, it := range s.Items {
			got[s.Key] = append(got[s.Key], it.ID)
		}
	}
	if c.Object.ID != "Deployment/sre-lab-app/url-shortener" || strings.Join(got["owned"], ",") != "ReplicaSet/sre-lab-app/us-rs" ||
		strings.Join(got["pods"], ",") != "Pod/sre-lab-app/us-1" || strings.Join(got["calls"], ",") != "Service/sre-lab-app/redis" ||
		strings.Join(c.Apps, ",") != "sre-lab-app/url-shortener" {
		t.Errorf("chain = %+v, sections %v", c, got)
	}
	// From the Service's side: its pod, and the caller.
	c, _ = chainOf(t, h, &protocol.ChainRequest{ID: "Service/sre-lab-app/redis"})
	if c == nil || len(c.Sections) == 0 {
		t.Fatalf("redis chain empty")
	}
	var callers []string
	for _, s := range c.Sections {
		if s.Key == protocol.ChainCalledBy {
			for _, it := range s.Items {
				callers = append(callers, it.ID)
			}
		}
	}
	if strings.Join(callers, ",") != "Deployment/sre-lab-app/url-shortener" {
		t.Errorf("redis called by %v", callers)
	}
	// A query finds objects; an unknown ID is not found; a bad request is refused.
	if c, _ := chainOf(t, h, &protocol.ChainRequest{Query: "redis"}); c == nil || len(c.Matches) == 0 || c.Matches[0].Name != "redis" || c.Object != nil {
		t.Errorf("query = %+v", c)
	}
	if _, r := chainOf(t, h, &protocol.ChainRequest{ID: "Deployment/sre-lab-app/nope"}); code(r) != protocol.CodeNotFound {
		t.Errorf("unknown: %+v", r.Error)
	}
	if _, r := chainOf(t, h, &protocol.ChainRequest{}); code(r) != protocol.CodeBadRequest {
		t.Errorf("empty: %+v", r.Error)
	}
}
