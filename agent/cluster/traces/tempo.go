// Package traces confirms app map links from traces (KM-54). The cluster
// agent asks Tempo's HTTP search API, in the cluster, which spans show one
// service calling another, and marks the map's links that really carry
// calls. It needs no Kubernetes permission: Tempo is an HTTP service, and
// the agent only sends GET searches to it.
//
// Two kinds of span say who calls whom:
//   - a server span in service B, with the caller's address (net.peer.ip or
//     client.address) and the Host it was asked for (http.host or
//     server.address): the caller pod, or the route whose host it is,
//     called B;
//   - a client span in service A, with the target (server.address,
//     net.peer.name, peer.service or the URL's host): A called that
//     Service.
//
// Kubelet health probes also produce server spans. They come from a node's
// own address or with a kube-probe user agent, and are ignored: they prove
// nothing about the application's calls.
package traces

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Span is one span reduced to what confirms a call.
type Span struct {
	Kind       string // server or client
	Service    string // resource service.name
	Namespace  string // resource k8s.namespace.name, if the collector adds it
	Deployment string // resource k8s.deployment.name, likewise
	PeerIP     string // server: the caller's address
	Host       string // server: the Host asked for, without the port
	Target     string // client: the called host or service name
	UserAgent  string
	Start      time.Time
}

// The searches. select() asks Tempo to return these attributes of each
// matched span; the others stay in Tempo.
//
// The server search leaves kubelet probes out in Tempo itself: they come
// every few seconds per pod, so the newest traces are nearly all probes
// and would crowd real calls out of the search limit (seen on the lab: 200
// of 200). TraceQL 2.9 has no "attribute missing" test, so a server span
// without a user agent attribute is not returned; probes that slip
// through (another user agent) are still dropped by Index.probe.
const (
	serverQuery = `{ kind = server && (span.http.user_agent !~ "kube-probe/.*" || span.user_agent.original !~ "kube-probe/.*") } | select(resource.service.name, resource.k8s.namespace.name, resource.k8s.deployment.name, ` +
		`span.net.peer.ip, span.client.address, span.http.host, span.server.address, span.http.user_agent, span.user_agent.original)`
	clientQuery = `{ kind = client } | select(resource.service.name, resource.k8s.namespace.name, resource.k8s.deployment.name, ` +
		`span.server.address, span.net.peer.name, span.peer.service, span.db.system, span.url.full, span.http.url)`
)

// Client asks one Tempo. It keeps the last answer for CacheFor, so a busy
// app map page does not load Tempo.
type Client struct {
	URL      string // http://tempo.observability.svc:3200
	HTTP     *http.Client
	Window   time.Duration // how far back to search
	Limit    int           // traces per search
	CacheFor time.Duration
	Now      func() time.Time

	mu      sync.Mutex
	at      time.Time
	spans   []Span
	lastErr error
}

// NewClient returns a client with the defaults: the last hour, 200 traces
// per search, a 5 s timeout, answers kept for a minute.
func NewClient(baseURL string) *Client {
	return &Client{URL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: 5 * time.Second},
		Window: time.Hour, Limit: 200, CacheFor: time.Minute, Now: time.Now}
}

// maxBody bounds one search answer.
const maxBody = 8 << 20

// Spans returns the spans of the window, from the cache when fresh.
func (c *Client) Spans(ctx context.Context) ([]Span, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.Now()
	if !c.at.IsZero() && now.Sub(c.at) < c.CacheFor {
		return c.spans, c.lastErr
	}
	var all []Span
	var err error
	for _, q := range []string{serverQuery, clientQuery} {
		var s []Span
		if s, err = c.search(ctx, q, now); err != nil {
			break
		}
		all = append(all, s...)
	}
	c.at, c.spans, c.lastErr = now, all, err
	if err != nil {
		c.spans = nil
	}
	return c.spans, err
}

func (c *Client) search(ctx context.Context, q string, now time.Time) ([]Span, error) {
	v := url.Values{}
	v.Set("q", q)
	v.Set("limit", strconv.Itoa(c.Limit))
	v.Set("spss", "20")
	v.Set("start", strconv.FormatInt(now.Add(-c.Window).Unix(), 10))
	v.Set("end", strconv.FormatInt(now.Unix(), 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/api/search?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tempo: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("tempo: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tempo: search answered %s: %s", resp.Status, strings.TrimSpace(string(body[:min(len(body), 200)])))
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("tempo: search answer over %d bytes", maxBody)
	}
	return Parse(body)
}

// searchResponse is Tempo's /api/search answer (v2).
type searchResponse struct {
	Traces []struct {
		StartTimeUnixNano string    `json:"startTimeUnixNano"`
		SpanSet           *spanSet  `json:"spanSet"`
		SpanSets          []spanSet `json:"spanSets"`
	} `json:"traces"`
}

type spanSet struct {
	Spans []struct {
		SpanID            string `json:"spanID"`
		StartTimeUnixNano string `json:"startTimeUnixNano"`
		Attributes        []struct {
			Key   string `json:"key"`
			Value struct {
				StringValue string `json:"stringValue"`
				IntValue    string `json:"intValue"`
			} `json:"value"`
		} `json:"attributes"`
	} `json:"spans"`
}

// Parse reads a Tempo search answer. A span set appears in spanSets and,
// for older Tempo versions, in spanSet; each span counts once.
func Parse(body []byte) ([]Span, error) {
	var r searchResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("tempo: search answer: %w", err)
	}
	var out []Span
	seen := map[string]bool{}
	for _, t := range r.Traces {
		sets := t.SpanSets
		if len(sets) == 0 && t.SpanSet != nil {
			sets = []spanSet{*t.SpanSet}
		}
		for _, ss := range sets {
			for _, sp := range ss.Spans {
				if seen[sp.SpanID] {
					continue
				}
				seen[sp.SpanID] = true
				a := map[string]string{}
				for _, at := range sp.Attributes {
					a[at.Key] = at.Value.StringValue
					if a[at.Key] == "" {
						a[at.Key] = at.Value.IntValue
					}
				}
				s := Span{Kind: a["kind"], Service: a["service.name"], Namespace: a["k8s.namespace.name"], Deployment: a["k8s.deployment.name"],
					PeerIP: first(a["net.peer.ip"], a["client.address"]), UserAgent: first(a["http.user_agent"], a["user_agent.original"])}
				if ns, err := strconv.ParseInt(sp.StartTimeUnixNano, 10, 64); err == nil {
					s.Start = time.Unix(0, ns).UTC()
				}
				switch s.Kind {
				case "server":
					s.Host = stripPort(first(a["http.host"], a["server.address"]))
				case "client":
					s.Target = first(a["peer.service"], a["server.address"], a["net.peer.name"], urlHost(a["url.full"]), urlHost(a["http.url"]))
					s.Target = stripPort(s.Target)
				}
				out = append(out, s)
			}
		}
	}
	return out, nil
}

func first(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func stripPort(h string) string {
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		if _, err := strconv.Atoi(h[i+1:]); err == nil {
			return h[:i]
		}
	}
	return h
}

func urlHost(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
