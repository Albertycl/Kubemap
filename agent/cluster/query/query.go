// Package query answers the portal's on-demand requests (protocol.Request)
// inside the cluster agent: list and get objects, read pod logs, review a
// node agent's token, and build the application map. Nothing is copied to
// the portal in bulk; each answer is one bounded response.
//
// Reads come from the agent's cache. Resources the cache does not hold, and
// ConfigMaps whose values the cache cut short, are read live from the API,
// still only with get and list. Secrets are never served.
package query

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"kubemap/agent/cluster/cache"
	"kubemap/agent/cluster/graph"
	"kubemap/agent/cluster/health"
	"kubemap/agent/cluster/traces"
	"kubemap/pkg/protocol"
)

const (
	// DefaultPageSize is the page size of a list request without a limit.
	DefaultPageSize = 100
	// pageBudget is how many bytes of items one list response may carry,
	// leaving room for the envelope under protocol.MaxMessageSize.
	pageBudget = protocol.MaxMessageSize - 64<<10
	// DefaultTimeout bounds one request when the caller gives no deadline.
	DefaultTimeout = 15 * time.Second
)

// Cache is the part of *cache.Cache the handler uses.
type Cache interface {
	List(gvr schema.GroupVersionResource, namespace string, sel labels.Selector) ([]runtime.Object, error)
	Get(gvr schema.GroupVersionResource, namespace, name string) (runtime.Object, error)
	Generation() uint64
}

// Handler answers requests. Kube and Dynamic are used for logs, token
// reviews and live reads; either may be nil in tests, and then those
// requests get CodeUnavailable.
type Handler struct {
	Cache   Cache
	Kube    kubernetes.Interface
	Dynamic dynamic.Interface
	Graph   *graph.Builder // for op graph; built from Cache when nil
	// Thresholds are the kubelet eviction settings for health.check; the
	// zero value means the kubelet defaults.
	Thresholds health.Thresholds
	// LogStream opens a pod log for logs.search; nil uses Kube (tests
	// replace it, since the fake clientset serves one fixed text).
	LogStream func(ctx context.Context, namespace, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error)
	// Traces confirms app map links from Tempo (KM-54); nil means no
	// traces.
	Traces *traces.Source

	kinds map[schema.GroupVersionResource]string
	// searches holds a slot per running logs.search (searchesAtOnce).
	searches chan struct{}
}

// New returns a Handler over the cache and clients.
func New(c Cache, kube kubernetes.Interface, dyn dynamic.Interface, g *graph.Builder) *Handler {
	h := &Handler{Cache: c, Kube: kube, Dynamic: dyn, Graph: g, kinds: map[schema.GroupVersionResource]string{},
		searches: make(chan struct{}, searchesAtOnce)}
	for _, r := range cache.Resources() {
		h.kinds[r.GVR] = r.Kind
	}
	return h
}

// Handle answers one request. It never panics on bad input and always
// returns a valid response: a result, or an error with a code.
func (h *Handler) Handle(ctx context.Context, req *protocol.Request) *protocol.Response {
	if err := req.Validate(); err != nil {
		return fail(err)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultTimeout)
		defer cancel()
	}
	var resp *protocol.Response
	var err error
	switch req.Op {
	case protocol.OpList:
		resp, err = h.list(ctx, req)
	case protocol.OpGet:
		resp, err = h.get(ctx, req)
	case protocol.OpLogs:
		resp, err = h.logs(ctx, req)
	case protocol.OpTokenReview:
		resp, err = h.tokenReview(ctx, req)
	case protocol.OpGraph:
		resp, err = h.graph(req)
	case protocol.OpAppMap:
		resp, err = h.appMap(ctx, req)
	case protocol.OpNetpolCheck:
		resp, err = h.netpolCheck(req)
	case protocol.OpServiceCheck:
		resp, err = h.serviceCheck(req)
	case protocol.OpPodDiagnose:
		resp, err = h.podDiagnose(ctx, req)
	case protocol.OpNodePressure:
		resp, err = h.nodePressure(req)
	case protocol.OpPodDebug:
		resp, err = h.podDebug(ctx, req)
	case protocol.OpHealthCheck:
		resp, err = h.healthCheck(req)
	case protocol.OpRBACWhoCan:
		resp, err = h.rbacWhoCan(req)
	case protocol.OpPodUsage:
		resp, err = h.podUsage(ctx, req)
	case protocol.OpLogSearch:
		resp, err = h.logSearch(ctx, req)
	case protocol.OpChain:
		resp, err = h.objectChain(req)
	default:
		err = &protocol.Error{Code: protocol.CodeUnsupported, Message: "unknown op " + string(req.Op)}
	}
	if err != nil {
		return fail(err)
	}
	if err := resp.Validate(); err != nil {
		return fail(err)
	}
	return resp
}

// fail maps any error to an error response with a protocol code.
func fail(err error) *protocol.Response {
	var pe *protocol.Error
	switch {
	case errors.As(err, &pe):
		return &protocol.Response{Error: pe}
	case apierrors.IsNotFound(err):
		return errResp(protocol.CodeNotFound, err)
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return errResp(protocol.CodeForbidden, err)
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err), errors.Is(err, context.DeadlineExceeded):
		return errResp(protocol.CodeTimeout, err)
	case apierrors.IsTooManyRequests(err), apierrors.IsServiceUnavailable(err):
		return errResp(protocol.CodeUnavailable, err)
	case apierrors.IsBadRequest(err), apierrors.IsInvalid(err):
		return errResp(protocol.CodeBadRequest, err)
	}
	return errResp(protocol.CodeInternal, err)
}

func errResp(code protocol.ErrorCode, err error) *protocol.Response {
	return &protocol.Response{Error: &protocol.Error{Code: code, Message: err.Error()}}
}

func perr(code protocol.ErrorCode, format string, args ...any) error {
	return &protocol.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

func requestGVR(req *protocol.Request) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: req.Group, Version: req.Version, Resource: req.Resource}
}

// isSecret is a second guard next to protocol validation: the agent never
// serves Secrets, whatever the group spelling.
func isSecret(gvr schema.GroupVersionResource) bool {
	// Any group (review-km-44 M1): the agent may read its own credential
	// Secret, so no request may name a Secret by any path.
	return strings.EqualFold(strings.TrimSpace(gvr.Resource), "secrets")
}

// list serves a page of objects: from the cache when it holds the resource,
// else live. Pages are cut by count (Limit, default 100) and by encoded size,
// and the continue token is the offset of the next item in the sorted list.
func (h *Handler) list(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
	gvr := requestGVR(req)
	if isSecret(gvr) {
		return nil, perr(protocol.CodeForbidden, "secrets are never served")
	}
	sel, err := labels.Parse(req.LabelSelector)
	if err != nil {
		return nil, perr(protocol.CodeBadRequest, "labelSelector: %v", err)
	}
	fsel, err := fields.ParseSelector(req.FieldSelector)
	if err != nil {
		return nil, perr(protocol.CodeBadRequest, "fieldSelector: %v", err)
	}
	offset := 0
	if req.Continue != "" {
		if offset, err = decodeContinue(req.Continue); err != nil {
			return nil, err
		}
	}
	limit := int(req.Limit)
	if limit == 0 {
		limit = DefaultPageSize
	}

	objs, err := h.Cache.List(gvr, req.Namespace, sel)
	if errors.Is(err, cache.ErrNotCached) {
		objs, err = h.liveList(ctx, gvr, req.Namespace, req.LabelSelector)
	}
	if err != nil {
		return nil, err
	}
	var kept []runtime.Object
	for _, o := range objs {
		ok, err := matchFields(o, fsel)
		if err != nil {
			return nil, err
		}
		if ok {
			kept = append(kept, o)
		}
	}
	sort.Slice(kept, func(i, j int) bool { return objKey(kept[i]) < objKey(kept[j]) })

	resp := &protocol.Response{Items: []json.RawMessage{}}
	size := 0
	i := offset
	for ; i < len(kept) && len(resp.Items) < limit; i++ {
		raw, err := h.encode(gvr, kept[i])
		if err != nil {
			return nil, err
		}
		if size+len(raw) > pageBudget {
			if len(resp.Items) == 0 {
				return nil, perr(protocol.CodeTooLarge, "object %s is larger than one message", objKey(kept[i]))
			}
			break
		}
		size += len(raw)
		resp.Items = append(resp.Items, raw)
	}
	if i < len(kept) {
		resp.Continue = encodeContinue(i)
	}
	if len(resp.Items) == 0 {
		resp.Items = nil // an empty page is an empty response
	}
	return resp, nil
}

func objKey(o runtime.Object) string {
	acc, err := meta.Accessor(o)
	if err != nil {
		return ""
	}
	return acc.GetNamespace() + "/" + acc.GetName()
}

func encodeContinue(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("o:" + strconv.Itoa(offset)))
}

func decodeContinue(tok string) (int, error) {
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err == nil {
		if n, err := strconv.Atoi(strings.TrimPrefix(string(b), "o:")); err == nil && n >= 0 && strings.HasPrefix(string(b), "o:") {
			return n, nil
		}
	}
	return 0, perr(protocol.CodeBadRequest, "invalid continue token")
}

// get serves one object. ConfigMaps the cache cut short are read live, so
// the YAML view shows every value.
func (h *Handler) get(ctx context.Context, req *protocol.Request) (*protocol.Response, error) {
	gvr := requestGVR(req)
	if isSecret(gvr) {
		return nil, perr(protocol.CodeForbidden, "secrets are never served")
	}
	obj, err := h.Cache.Get(gvr, req.Namespace, req.Name)
	if errors.Is(err, cache.ErrNotCached) || (err == nil && truncated(obj)) {
		obj, err = h.liveGet(ctx, gvr, req.Namespace, req.Name)
	}
	if err != nil {
		return nil, err
	}
	raw, err := h.encode(gvr, obj)
	if err != nil {
		return nil, err
	}
	if len(raw) > pageBudget {
		return nil, perr(protocol.CodeTooLarge, "object is %d bytes, larger than one message", len(raw))
	}
	return &protocol.Response{Object: raw}, nil
}

func truncated(obj runtime.Object) bool {
	acc, err := meta.Accessor(obj)
	return err == nil && acc.GetAnnotations()[cache.TruncatedAnnotation] != ""
}

func (h *Handler) liveGet(ctx context.Context, gvr schema.GroupVersionResource, ns, name string) (runtime.Object, error) {
	if h.Dynamic == nil {
		return nil, perr(protocol.CodeUnavailable, "%s is not cached and live reads are off", gvr.GroupResource())
	}
	var ri dynamic.ResourceInterface = h.Dynamic.Resource(gvr)
	if ns != "" {
		ri = h.Dynamic.Resource(gvr).Namespace(ns)
	}
	u, err := ri.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	cache.StripMetadata(u)
	return u, nil
}

// maxLiveItems caps a live list: it is for resources the cache does not
// watch, which are rarely many; the portal pages through what it gets.
const maxLiveItems = 1000

func (h *Handler) liveList(ctx context.Context, gvr schema.GroupVersionResource, ns, selector string) ([]runtime.Object, error) {
	if h.Dynamic == nil {
		return nil, perr(protocol.CodeUnavailable, "%s is not cached and live reads are off", gvr.GroupResource())
	}
	var ri dynamic.ResourceInterface = h.Dynamic.Resource(gvr)
	if ns != "" {
		ri = h.Dynamic.Resource(gvr).Namespace(ns)
	}
	list, err := ri.List(ctx, metav1.ListOptions{LabelSelector: selector, Limit: maxLiveItems})
	if err != nil {
		return nil, err
	}
	out := make([]runtime.Object, 0, len(list.Items))
	for i := range list.Items {
		cache.StripMetadata(&list.Items[i])
		out = append(out, &list.Items[i])
	}
	return out, nil
}

// encode marshals an object with its apiVersion and kind. Objects from typed
// informers have an empty TypeMeta, which a YAML view needs.
func (h *Handler) encode(gvr schema.GroupVersionResource, obj runtime.Object) (json.RawMessage, error) {
	if k := h.kinds[gvr]; k != "" && obj.GetObjectKind().GroupVersionKind().Kind == "" {
		obj = obj.DeepCopyObject()
		obj.GetObjectKind().SetGroupVersionKind(gvr.GroupVersion().WithKind(k))
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", objKey(obj), err)
	}
	return raw, nil
}

// fieldGetters are the field selectors the cache can answer; the API server
// supports a similar small set per resource.
var fieldGetters = map[string]func(map[string]any) string{
	"metadata.name":      func(m map[string]any) string { return str(m, "metadata", "name") },
	"metadata.namespace": func(m map[string]any) string { return str(m, "metadata", "namespace") },
	"spec.nodeName":      func(m map[string]any) string { return str(m, "spec", "nodeName") },
	"status.phase":       func(m map[string]any) string { return str(m, "status", "phase") },
	"type":               func(m map[string]any) string { return str(m, "type") },
	"reason":             func(m map[string]any) string { return str(m, "reason") },
	"involvedObject.kind": func(m map[string]any) string {
		return str(m, "involvedObject", "kind")
	},
	"involvedObject.name": func(m map[string]any) string { return str(m, "involvedObject", "name") },
	"involvedObject.uid":  func(m map[string]any) string { return str(m, "involvedObject", "uid") },
}

func str(m map[string]any, path ...string) string {
	var cur any = m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[p]
	}
	s, _ := cur.(string)
	return s
}

func matchFields(obj runtime.Object, sel fields.Selector) (bool, error) {
	if sel.Empty() {
		return true, nil
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return false, err
	}
	set := fields.Set{}
	for _, r := range sel.Requirements() {
		get, ok := fieldGetters[r.Field]
		if !ok {
			return false, perr(protocol.CodeBadRequest, "field selector %q is not supported", r.Field)
		}
		set[r.Field] = get(m)
	}
	return sel.Matches(set), nil
}
