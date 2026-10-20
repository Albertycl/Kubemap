// Package resources serves a cluster's namespaces, pods and pod logs to the
// web UI (KM-80). Everything is asked from the cluster agent through the
// tunnel, with its list and logs requests and its log stream; nothing is
// stored in the portal.
package resources

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"time"

	"kubemap/pkg/protocol"
	"kubemap/portal/agentcall"
	"kubemap/portal/httpjson"
	"kubemap/portal/store"
	"kubemap/portal/tunnel"
)

// Agent is what the API needs from the tunnel (tunnel.Hub).
type Agent interface {
	Request(ctx context.Context, clusterID string, req *protocol.Request) (*protocol.Response, error)
	OpenStream(ctx context.Context, clusterID string, o *protocol.StreamOpen) (LogStream, error)
}

// LogStream is an open log stream (*tunnel.Stream).
type LogStream interface {
	Next(ctx context.Context) ([]byte, *protocol.StreamClose, error)
	Close()
}

// HubAgent adapts *tunnel.Hub to Agent.
type HubAgent struct{ *tunnel.Hub }

// OpenStream opens a stream on the hub.
func (h HubAgent) OpenStream(ctx context.Context, clusterID string, o *protocol.StreamOpen) (LogStream, error) {
	s, err := h.Hub.OpenStream(ctx, clusterID, o)
	if err != nil {
		return nil, err
	}
	return s, nil
}

const (
	// requestTimeout bounds one list or logs request through the tunnel.
	requestTimeout = 20 * time.Second
	// pageSize is the page size asked for; the agent also cuts pages that
	// would not fit in one message.
	pageSize = 500
	// maxItems caps a pods or namespaces answer. The UI filters by
	// namespace on bigger clusters.
	maxItems = 5000
	// Log tail defaults and limit (the agent caps at 10,000 too).
	defaultTailLines = 500
	maxTailLines     = 10000
	// debugTimeout bounds a pod.debug walk: it reads logs, events, the
	// node and the NetworkPolicies of the pod's dependencies.
	debugTimeout = 30 * time.Second
	// firstEventWait is how long a followed log waits for its first chunk
	// before answering 200: an error in that time (no such pod or
	// container) still gets a proper HTTP status.
	firstEventWait = 3 * time.Second
)

// API serves (behind the login):
//
//	GET /api/clusters/{id}/namespaces
//	GET /api/clusters/{id}/pods[?namespace=ns]
//	GET /api/clusters/{id}/services[?namespace=ns]
//	GET /api/clusters/{id}/namespaces/{ns}/pods/{pod}/logs[?container=&previous=&follow=&tailLines=&timestamps=]
//	GET /api/clusters/{id}/namespaces/{ns}/pods/{pod}/debug    guided debugging (pod.debug, KM-76)
//	GET /api/clusters/{id}/log-search?app=…                    log search across an app's pods (logs.search, KM-57)
type API struct {
	store store.Store
	agent Agent
}

// NewAPI returns the resources API.
func NewAPI(st store.Store, agent Agent) *API { return &API{store: st, agent: agent} }

// Routes registers the API; require wraps every route (auth.Service.Require).
func (a *API) Routes(require func(http.Handler) http.Handler) func(*http.ServeMux) {
	return func(mux *http.ServeMux) {
		mux.Handle("GET /api/clusters/{id}/namespaces", require(http.HandlerFunc(a.namespaces)))
		mux.Handle("GET /api/clusters/{id}/pods", require(http.HandlerFunc(a.pods)))
		mux.Handle("GET /api/clusters/{id}/services", require(http.HandlerFunc(a.services)))
		mux.Handle("GET /api/clusters/{id}/namespaces/{ns}/pods/{pod}/logs", require(http.HandlerFunc(a.logs)))
		mux.Handle("GET /api/clusters/{id}/namespaces/{ns}/pods/{pod}/debug", require(http.HandlerFunc(a.debug)))
		mux.Handle("GET /api/clusters/{id}/log-search", require(http.HandlerFunc(a.logSearch)))
	}
}

// ListAll asks an agent for every page of a list (Version v1 unless set),
// up to maxItems. It returns the items and whether it stopped at maxItems.
// An agent error is returned as *protocol.Error, a tunnel error as it is
// (write either with agentcall.Error).
func ListAll(ctx context.Context, agent agentcall.Requester, clusterID string, req protocol.Request) ([]json.RawMessage, bool, error) {
	req.Op, req.Limit = protocol.OpList, pageSize
	if req.Version == "" {
		req.Version = "v1"
	}
	if err := req.Validate(); err != nil {
		return nil, false, protocol.AsError(err)
	}
	var items []json.RawMessage
	for {
		resp, err := agent.Request(ctx, clusterID, &req)
		if err != nil {
			return nil, false, err
		}
		if resp.Error != nil {
			return nil, false, resp.Error
		}
		items = append(items, resp.Items...)
		if len(items) >= maxItems {
			return items[:maxItems], resp.Continue != "" || len(items) > maxItems, nil
		}
		if resp.Continue == "" {
			return items, false, nil
		}
		req.Continue = resp.Continue
	}
}

// listAll is ListAll for a handler: on failure it has written the answer
// and returns false.
func (a *API) listAll(w http.ResponseWriter, r *http.Request, clusterID string, req protocol.Request) ([]json.RawMessage, bool, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	items, truncated, err := ListAll(ctx, a.agent, clusterID, req)
	if err != nil {
		agentcall.Error(w, err, "choose one namespace")
		return nil, false, false
	}
	return items, truncated, true
}

func (a *API) namespaces(w http.ResponseWriter, r *http.Request) {
	c, ok := agentcall.Cluster(w, r, a.store)
	if !ok {
		return
	}
	items, truncated, ok := a.listAll(w, r, c.ID, protocol.Request{Resource: "namespaces"})
	if !ok {
		return
	}
	out := make([]Namespace, 0, len(items))
	for _, raw := range items {
		if n, err := namespaceView(raw); err == nil {
			out = append(out, n)
		}
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"namespaces": out, "truncated": truncated})
}

func (a *API) services(w http.ResponseWriter, r *http.Request) {
	c, ok := agentcall.Cluster(w, r, a.store)
	if !ok {
		return
	}
	items, truncated, ok := a.listAll(w, r, c.ID, protocol.Request{Resource: "services", Namespace: r.URL.Query().Get("namespace")})
	if !ok {
		return
	}
	out := make([]Service, 0, len(items))
	for _, raw := range items {
		if s, err := ServiceView(raw); err == nil {
			out = append(out, s)
		}
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"services": out, "truncated": truncated})
}

func (a *API) pods(w http.ResponseWriter, r *http.Request) {
	c, ok := agentcall.Cluster(w, r, a.store)
	if !ok {
		return
	}
	items, truncated, ok := a.listAll(w, r, c.ID, protocol.Request{Resource: "pods", Namespace: r.URL.Query().Get("namespace")})
	if !ok {
		return
	}
	out := make([]Pod, 0, len(items))
	for _, raw := range items {
		if p, err := PodView(raw); err == nil {
			out = append(out, p)
		}
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"pods": out, "truncated": truncated})
}

// logOptions reads the log query: container, previous, timestamps, and
// tailLines (default 500, at most 10,000).
func logOptions(r *http.Request) (*protocol.LogOptions, bool, error) {
	q := r.URL.Query()
	o := &protocol.LogOptions{Container: q.Get("container"), TailLines: defaultTailLines}
	if o.Container != "" && !containerName.MatchString(o.Container) {
		return nil, false, errors.New("container must be a container name (lowercase letters, digits and '-')")
	}
	var err error
	if o.Previous, err = boolParam(q.Get("previous")); err != nil {
		return nil, false, errors.New("previous must be true or false")
	}
	if o.Timestamps, err = boolParam(q.Get("timestamps")); err != nil {
		return nil, false, errors.New("timestamps must be true or false")
	}
	follow, err := boolParam(q.Get("follow"))
	if err != nil {
		return nil, false, errors.New("follow must be true or false")
	}
	if s := q.Get("tailLines"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 1 || n > maxTailLines {
			return nil, false, errors.New("tailLines must be 1 to 10000")
		}
		o.TailLines = n
	}
	return o, follow, nil
}

// containerName is a DNS-1123 label, as Kubernetes requires for containers.
var containerName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

func boolParam(s string) (bool, error) {
	if s == "" {
		return false, nil
	}
	return strconv.ParseBool(s)
}

func (a *API) logs(w http.ResponseWriter, r *http.Request) {
	c, ok := agentcall.Cluster(w, r, a.store)
	if !ok {
		return
	}
	ns, pod := r.PathValue("ns"), r.PathValue("pod")
	opts, follow, err := logOptions(r)
	if err != nil {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if follow {
		a.follow(w, r, c.ID, &protocol.StreamOpen{Kind: protocol.StreamLogs, Namespace: ns, Name: pod, Logs: opts})
		return
	}
	req := &protocol.Request{Op: protocol.OpLogs, Namespace: ns, Name: pod, Logs: opts}
	if err := req.Validate(); err != nil {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", protocol.AsError(err).Message)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	resp, err := a.agent.Request(ctx, c.ID, req)
	if err != nil {
		agentcall.TunnelError(w, err)
		return
	}
	if resp.Error != nil {
		agentcall.AgentError(w, resp.Error, "ask for fewer lines")
		return
	}
	// Plain text: nosniff (set for every response) keeps a browser from
	// treating log content as HTML.
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Kubemap-Truncated", strconv.FormatBool(resp.Truncated))
	w.WriteHeader(http.StatusOK)
	w.Write(resp.Logs)
}

// streamLine is one line of a followed log (application/x-ndjson): a chunk
// of log bytes (base64 in JSON), or the end of the stream.
type streamLine struct {
	Data []byte     `json:"data,omitempty"`
	End  *streamEnd `json:"end,omitempty"`
}

type streamEnd struct {
	Reason string          `json:"reason,omitempty"`
	Error  *protocol.Error `json:"error,omitempty"`
}

// follow streams a pod log as NDJSON until the log ends, the agent goes
// away, or the browser disconnects (which closes the stream on the agent).
func (a *API) follow(w http.ResponseWriter, r *http.Request, clusterID string, o *protocol.StreamOpen) {
	if err := o.Validate(); err != nil {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", protocol.AsError(err).Message)
		return
	}
	s, err := a.agent.OpenStream(r.Context(), clusterID, o)
	if err != nil {
		agentcall.TunnelError(w, err)
		return
	}
	defer s.Close()

	// Wait briefly for the first event, so that a stream that fails at once
	// (no such pod, no previous container) gets an error status.
	first, end, err := func() ([]byte, *protocol.StreamClose, error) {
		ctx, cancel := context.WithTimeout(r.Context(), firstEventWait)
		defer cancel()
		return s.Next(ctx)
	}()
	if err != nil && r.Context().Err() != nil {
		return // the browser went away
	}
	if end != nil && end.Error != nil {
		agentcall.AgentError(w, end.Error, "ask for fewer lines")
		return
	}

	h := w.Header()
	h.Set("Content-Type", "application/x-ndjson")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	write := func(l streamLine) bool {
		if enc.Encode(l) != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if first != nil && !write(streamLine{Data: first}) {
		return
	}
	for end == nil {
		var b []byte
		b, end, err = s.Next(r.Context())
		if err != nil {
			return // the browser went away; the deferred Close tells the agent
		}
		if b != nil && !write(streamLine{Data: b}) {
			return
		}
	}
	write(streamLine{End: &streamEnd{Reason: end.Reason, Error: end.Error}})
}

// debug walks the SRE checklist for one pod (pod.debug): status, events,
// logs, exit code, resources, node, Services and NetworkPolicy, each with
// evidence, a verdict and the kubectl commands to check it by hand.
func (a *API) debug(w http.ResponseWriter, r *http.Request) {
	c, ok := agentcall.Cluster(w, r, a.store)
	if !ok {
		return
	}
	req := &protocol.Request{Op: protocol.OpPodDebug, Namespace: r.PathValue("ns"), Name: r.PathValue("pod")}
	if err := req.Validate(); err != nil {
		httpjson.Error(w, http.StatusBadRequest, "bad_request", protocol.AsError(err).Message)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), debugTimeout)
	defer cancel()
	resp, err := a.agent.Request(ctx, c.ID, req)
	if err != nil {
		agentcall.TunnelError(w, err)
		return
	}
	if resp.Error != nil {
		agentcall.AgentError(w, resp.Error, "the walk is too large")
		return
	}
	var d protocol.PodDebug
	if len(resp.Object) == 0 || json.Unmarshal(resp.Object, &d) != nil {
		httpjson.Error(w, http.StatusBadGateway, "agent_error", "the cluster agent sent an unreadable debug walk")
		return
	}
	if d.Steps == nil {
		d.Steps = []protocol.DebugStep{}
	}
	for i := range d.Steps {
		if d.Steps[i].Commands == nil {
			d.Steps[i].Commands = []string{}
		}
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"debug": d, "at": time.Now().UTC()})
}
