package query

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"kubemap/agent/cluster/cache"
	"kubemap/pkg/protocol"
)

// fakeCache serves typed objects like the informer cache: without TypeMeta.
type fakeCache struct {
	objs map[schema.GroupVersionResource][]runtime.Object
}

func (f *fakeCache) List(g schema.GroupVersionResource, ns string, sel labels.Selector) ([]runtime.Object, error) {
	list, ok := f.objs[g]
	if !ok {
		return nil, fmt.Errorf("%w: %s", cache.ErrNotCached, g)
	}
	if sel == nil {
		sel = labels.Everything()
	}
	var out []runtime.Object
	for _, o := range list {
		m := o.(metav1.Object)
		if (ns == "" || m.GetNamespace() == ns) && sel.Matches(labels.Set(m.GetLabels())) {
			out = append(out, o)
		}
	}
	return out, nil
}

func (f *fakeCache) Get(g schema.GroupVersionResource, ns, name string) (runtime.Object, error) {
	list, ok := f.objs[g]
	if !ok {
		return nil, fmt.Errorf("%w: %s", cache.ErrNotCached, g)
	}
	for _, o := range list {
		m := o.(metav1.Object)
		if m.GetNamespace() == ns && m.GetName() == name {
			return o, nil
		}
	}
	return nil, apierrors.NewNotFound(g.GroupResource(), name)
}

func (f *fakeCache) Generation() uint64 { return 1 }

func gvr(g, v, r string) schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: g, Version: v, Resource: r}
}

var (
	gPods   = gvr("", "v1", "pods")
	gCM     = gvr("", "v1", "configmaps")
	gDeploy = gvr("apps", "v1", "deployments")
	gCert   = gvr("cert-manager.io", "v1", "certificates") // not cached
	// The metrics API serves PodMetrics as resource "pods".
	gMetrics = gvr("metrics.k8s.io", "v1beta1", "pods")
)

func newHandler(t *testing.T, objs map[schema.GroupVersionResource][]runtime.Object, live ...runtime.Object) (*Handler, *kubefake.Clientset, *dynfake.FakeDynamicClient) {
	t.Helper()
	kube := kubefake.NewClientset()
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{gCert: "CertificateList", gCM: "ConfigMapList", gMetrics: "PodMetricsList"}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, live...)
	return New(&fakeCache{objs: objs}, kube, dyn, nil), kube, dyn
}

func pod(ns, name, node string, lbl map[string]string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: lbl}, Spec: corev1.PodSpec{NodeName: node}}
}

func do(h *Handler, req *protocol.Request) *protocol.Response {
	return h.Handle(context.Background(), req)
}

func code(r *protocol.Response) protocol.ErrorCode {
	if r.Error == nil {
		return ""
	}
	return r.Error.Code
}

func TestListFromCache(t *testing.T) {
	h, _, _ := newHandler(t, map[schema.GroupVersionResource][]runtime.Object{gPods: {
		pod("shop", "web-2", "k8server", map[string]string{"app": "web"}),
		pod("shop", "web-1", "node-2", map[string]string{"app": "web"}),
		pod("shop", "db-0", "k8server", map[string]string{"app": "db"}),
		pod("other", "web-9", "k8server", map[string]string{"app": "web"}),
	}})
	r := do(h, &protocol.Request{Op: protocol.OpList, Version: "v1", Resource: "pods", Namespace: "shop", LabelSelector: "app=web"})
	if r.Error != nil || len(r.Items) != 2 || r.Continue != "" {
		t.Fatalf("list = %+v", r)
	}
	var first map[string]any
	json.Unmarshal(r.Items[0], &first)
	// Sorted by namespace/name, with apiVersion and kind filled in.
	if first["kind"] != "Pod" || first["apiVersion"] != "v1" || first["metadata"].(map[string]any)["name"] != "web-1" {
		t.Errorf("first item = %v", first)
	}
	// Field selector for the process view (KM-59): pods on one node.
	r = do(h, &protocol.Request{Op: protocol.OpList, Version: "v1", Resource: "pods", FieldSelector: "spec.nodeName=k8server"})
	if len(r.Items) != 3 {
		t.Errorf("pods on k8server = %d", len(r.Items))
	}
	r = do(h, &protocol.Request{Op: protocol.OpList, Version: "v1", Resource: "pods", FieldSelector: "spec.nodeName=k8server,metadata.namespace!=other"})
	if len(r.Items) != 2 {
		t.Errorf("combined field selector = %d", len(r.Items))
	}
	for sel, want := range map[string]protocol.ErrorCode{"spec.foo=bar": protocol.CodeBadRequest, "===": protocol.CodeBadRequest} {
		if c := code(do(h, &protocol.Request{Op: protocol.OpList, Version: "v1", Resource: "pods", FieldSelector: sel})); c != want {
			t.Errorf("field selector %q: code %q", sel, c)
		}
	}
	if c := code(do(h, &protocol.Request{Op: protocol.OpList, Version: "v1", Resource: "pods", LabelSelector: "app in (("})); c != protocol.CodeBadRequest {
		t.Errorf("bad label selector: %q", c)
	}
	// Nothing matched: an empty, valid response.
	r = do(h, &protocol.Request{Op: protocol.OpList, Version: "v1", Resource: "pods", Namespace: "empty"})
	if r.Error != nil || len(r.Items) != 0 {
		t.Errorf("empty list = %+v", r)
	}
}

func TestListPaging(t *testing.T) {
	var pods []runtime.Object
	for i := range 250 {
		pods = append(pods, pod("load", fmt.Sprintf("p-%03d", i), "", nil))
	}
	h, _, _ := newHandler(t, map[schema.GroupVersionResource][]runtime.Object{gPods: pods})
	seen := map[string]bool{}
	req := &protocol.Request{Op: protocol.OpList, Version: "v1", Resource: "pods"}
	pages := 0
	for {
		r := do(h, req)
		if r.Error != nil {
			t.Fatal(r.Error)
		}
		pages++
		for _, raw := range r.Items {
			var m map[string]any
			json.Unmarshal(raw, &m)
			name := m["metadata"].(map[string]any)["name"].(string)
			if seen[name] {
				t.Fatalf("duplicate %s", name)
			}
			seen[name] = true
		}
		if r.Continue == "" {
			break
		}
		req.Continue = r.Continue
	}
	if len(seen) != 250 || pages != 3 {
		t.Errorf("seen %d in %d pages", len(seen), pages)
	}
	// Pages are also cut by size, so each response fits in one message.
	big := strings.Repeat("x", 300<<10)
	var fat []runtime.Object
	for i := range 5 {
		p := pod("fat", fmt.Sprintf("f-%d", i), "", nil)
		p.Annotations = map[string]string{"blob": big}
		fat = append(fat, p)
	}
	h, _, _ = newHandler(t, map[schema.GroupVersionResource][]runtime.Object{gPods: fat})
	r := do(h, &protocol.Request{Op: protocol.OpList, Version: "v1", Resource: "pods"})
	if r.Error != nil || len(r.Items) != 3 || r.Continue == "" {
		t.Fatalf("fat page: %d items, continue %q, err %v", len(r.Items), r.Continue, r.Error)
	}
	if msg, err := protocol.Encode("q", r); err != nil || len(msg) > protocol.MaxMessageSize {
		t.Errorf("fat page does not fit: %v", err)
	}
	if c := code(do(h, &protocol.Request{Op: protocol.OpList, Version: "v1", Resource: "pods", Continue: "not-a-token"})); c != protocol.CodeBadRequest {
		t.Errorf("bad continue: %q", c)
	}
	// One object larger than a message is refused, not sent broken.
	huge := pod("fat", "huge", "", nil)
	huge.Annotations = map[string]string{"blob": strings.Repeat("y", protocol.MaxMessageSize)}
	h, _, _ = newHandler(t, map[schema.GroupVersionResource][]runtime.Object{gPods: {huge}})
	if c := code(do(h, &protocol.Request{Op: protocol.OpList, Version: "v1", Resource: "pods"})); c != protocol.CodeTooLarge {
		t.Errorf("huge object: %q", c)
	}
	if c := code(do(h, &protocol.Request{Op: protocol.OpGet, Version: "v1", Resource: "pods", Namespace: "fat", Name: "huge"})); c != protocol.CodeTooLarge {
		t.Errorf("huge get: %q", c)
	}
}

func TestGet(t *testing.T) {
	cut := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "mon", Name: "dash",
		Annotations: map[string]string{cache.TruncatedAnnotation: "1 values cut"}}, Data: map[string]string{"d.json": "{\"cut"}}
	full := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"namespace": "mon", "name": "dash",
			"managedFields": []any{map[string]any{"manager": "helm"}}},
		"data": map[string]any{"d.json": "{\"full\": true}"}}}
	h, _, _ := newHandler(t, map[schema.GroupVersionResource][]runtime.Object{
		gDeploy: {&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"}}},
		gCM:     {cut},
	}, full)
	r := do(h, &protocol.Request{Op: protocol.OpGet, Group: "apps", Version: "v1", Resource: "deployments", Namespace: "shop", Name: "web"})
	var d map[string]any
	if r.Error != nil || json.Unmarshal(r.Object, &d) != nil || d["kind"] != "Deployment" || d["apiVersion"] != "apps/v1" {
		t.Fatalf("get deployment = %+v %v", r, d)
	}
	// A truncated ConfigMap is read live, in full, without managedFields.
	r = do(h, &protocol.Request{Op: protocol.OpGet, Version: "v1", Resource: "configmaps", Namespace: "mon", Name: "dash"})
	if r.Error != nil || !bytes.Contains(r.Object, []byte(`{\"full\": true}`)) || bytes.Contains(r.Object, []byte("managedFields")) {
		t.Errorf("truncated configmap = %s %v", r.Object, r.Error)
	}
	if c := code(do(h, &protocol.Request{Op: protocol.OpGet, Group: "apps", Version: "v1", Resource: "deployments", Namespace: "shop", Name: "nope"})); c != protocol.CodeNotFound {
		t.Errorf("missing object: %q", c)
	}
}

func TestLiveForUncachedResources(t *testing.T) {
	cert := func(name string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cert-manager.io/v1", "kind": "Certificate",
			"metadata": map[string]any{"namespace": "web", "name": name}, "spec": map[string]any{"secretName": name + "-tls"}}}
	}
	h, _, dyn := newHandler(t, map[schema.GroupVersionResource][]runtime.Object{}, cert("a"), cert("b"))
	r := do(h, &protocol.Request{Op: protocol.OpList, Group: "cert-manager.io", Version: "v1", Resource: "certificates", Namespace: "web"})
	if r.Error != nil || len(r.Items) != 2 {
		t.Fatalf("live list = %+v", r)
	}
	r = do(h, &protocol.Request{Op: protocol.OpGet, Group: "cert-manager.io", Version: "v1", Resource: "certificates", Namespace: "web", Name: "b"})
	if r.Error != nil || !bytes.Contains(r.Object, []byte("b-tls")) {
		t.Errorf("live get = %+v", r)
	}
	// Only get and list are ever sent to the API.
	for _, a := range dyn.Actions() {
		if v := a.GetVerb(); v != "get" && v != "list" {
			t.Errorf("live read used verb %q", v)
		}
	}
	// RBAC says no: forbidden.
	dyn.PrependReactor("list", "certificates", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(gCert.GroupResource(), "", errors.New("RBAC"))
	})
	if c := code(do(h, &protocol.Request{Op: protocol.OpList, Group: "cert-manager.io", Version: "v1", Resource: "certificates"})); c != protocol.CodeForbidden {
		t.Errorf("forbidden live list: %q", c)
	}
	// Without a dynamic client, uncached resources are unavailable.
	h.Dynamic = nil
	if c := code(do(h, &protocol.Request{Op: protocol.OpGet, Group: "cert-manager.io", Version: "v1", Resource: "certificates", Namespace: "web", Name: "a"})); c != protocol.CodeUnavailable {
		t.Errorf("no dynamic client: %q", c)
	}
}

func TestSecretsNeverServed(t *testing.T) {
	secret := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"namespace": "a", "name": "db"}, "data": map[string]any{"password": "c2VjcmV0"}}}
	h, _, dyn := newHandler(t, map[schema.GroupVersionResource][]runtime.Object{}, secret)
	for _, req := range []*protocol.Request{
		{Op: protocol.OpGet, Version: "v1", Resource: "secrets", Namespace: "a", Name: "db"},
		{Op: protocol.OpList, Version: "v1", Resource: "secrets"},
		{Op: protocol.OpGet, Group: "core", Version: "v1", Resource: "secrets", Namespace: "a", Name: "db"},
		{Op: protocol.OpList, Group: "core", Version: "v1", Resource: "secrets"},
	} {
		r := do(h, req)
		if code(r) != protocol.CodeForbidden || len(r.Object) > 0 || len(r.Items) > 0 {
			t.Errorf("%+v served a secret: %+v", req, r)
		}
	}
	if len(dyn.Actions()) != 0 {
		t.Errorf("secret request reached the API: %v", dyn.Actions())
	}
}

func TestLogs(t *testing.T) {
	h, kube, _ := newHandler(t, nil)
	r := do(h, &protocol.Request{Op: protocol.OpLogs, Namespace: "shop", Name: "web-1",
		Logs: &protocol.LogOptions{Container: "app", TailLines: 50000, Previous: true}})
	if r.Error != nil || string(r.Logs) != "fake logs" || r.Truncated {
		t.Fatalf("logs = %+v", r)
	}
	// The request asked for tail 50000; the agent caps it.
	for _, a := range kube.Actions() {
		if g, ok := a.(clienttesting.GenericAction); ok {
			if o, ok := g.GetValue().(*corev1.PodLogOptions); ok {
				if *o.TailLines != MaxTailLines || !o.Previous || o.Container != "app" || *o.LimitBytes != maxLogRead {
					t.Errorf("log options = %+v", o)
				}
			}
		}
	}
	h.Kube = nil
	if c := code(do(h, &protocol.Request{Op: protocol.OpLogs, Namespace: "a", Name: "b"})); c != protocol.CodeUnavailable {
		t.Errorf("no client: %q", c)
	}
}

func TestReadTail(t *testing.T) {
	var sb strings.Builder
	for i := range 100000 {
		fmt.Fprintf(&sb, "line %06d\n", i) // 12 bytes per line
	}
	data, cut, err := readTail(strings.NewReader(sb.String()), 1200)
	if err != nil || !cut {
		t.Fatalf("cut=%v err=%v", cut, err)
	}
	if len(data) > 1200 || !strings.HasPrefix(string(data), "line ") || !strings.HasSuffix(string(data), "line 099999\n") {
		t.Errorf("tail = %q…%q (%d bytes)", data[:20], data[len(data)-20:], len(data))
	}
	data, cut, _ = readTail(strings.NewReader("short\n"), 1200)
	if cut || string(data) != "short\n" {
		t.Errorf("short = %q %v", data, cut)
	}
	if _, _, err := readTail(errReader{}, 10); err == nil {
		t.Error("read error must be returned")
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestTokenReview(t *testing.T) {
	h, kube, _ := newHandler(t, nil)
	var sent *authv1.TokenReview
	kube.PrependReactor("create", "tokenreviews", func(a clienttesting.Action) (bool, runtime.Object, error) {
		sent = a.(clienttesting.CreateAction).GetObject().(*authv1.TokenReview)
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{
			Authenticated: true, Audiences: []string{protocol.NodeTokenAudience},
			User: authv1.UserInfo{Username: protocol.NodeAgentUsername, Extra: map[string]authv1.ExtraValue{
				extraPodName: {"kubemap-node-agent-x7k2p"}, extraPodUID: {"uid-1"}, extraNodeName: {"k8server"}}},
		}}, nil
	})
	r := do(h, &protocol.Request{Op: protocol.OpTokenReview, TokenReview: &protocol.TokenReviewRequest{Token: "satok_example", Audience: protocol.NodeTokenAudience}})
	if r.Error != nil || r.TokenReview == nil {
		t.Fatalf("tokenreview = %+v", r)
	}
	if err := r.TokenReview.AcceptNode("k8server"); err != nil {
		t.Errorf("valid node token rejected: %v (%+v)", err, r.TokenReview)
	}
	if sent.Spec.Token != "satok_example" || len(sent.Spec.Audiences) != 1 || sent.Spec.Audiences[0] != protocol.NodeTokenAudience {
		t.Errorf("sent review = %+v", sent.Spec)
	}
	// An API failure gives no token back.
	kube.PrependReactor("create", "tokenreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "authentication.k8s.io", Resource: "tokenreviews"}, "", errors.New("RBAC"))
	})
	r = do(h, &protocol.Request{Op: protocol.OpTokenReview, TokenReview: &protocol.TokenReviewRequest{Token: "satok_secret_value", Audience: protocol.NodeTokenAudience}})
	if code(r) != protocol.CodeUnavailable || strings.Contains(r.Error.Message, "satok_secret_value") {
		t.Errorf("failed review = %+v", r.Error)
	}
	// Another audience never reaches the API.
	if c := code(do(h, &protocol.Request{Op: protocol.OpTokenReview, TokenReview: &protocol.TokenReviewRequest{Token: "t", Audience: "https://kubernetes.default.svc"}})); c != protocol.CodeBadRequest {
		t.Errorf("other audience: %q", c)
	}
}

func TestInvalidRequestsAndErrors(t *testing.T) {
	h, _, _ := newHandler(t, nil)
	for _, req := range []*protocol.Request{
		{Op: "delete", Resource: "pods"},
		{Op: protocol.OpList},
		{Op: protocol.OpList, Version: "v1", Resource: "pods", Limit: -1},
	} {
		if r := do(h, req); r.Error == nil {
			t.Errorf("%+v accepted", req)
		}
	}
	for err, want := range map[error]protocol.ErrorCode{
		apierrors.NewNotFound(schema.GroupResource{}, "x"):         protocol.CodeNotFound,
		apierrors.NewForbidden(schema.GroupResource{}, "x", nil):   protocol.CodeForbidden,
		apierrors.NewTimeoutError("slow", 1):                       protocol.CodeTimeout,
		context.DeadlineExceeded:                                   protocol.CodeTimeout,
		apierrors.NewTooManyRequests("busy", 1):                    protocol.CodeUnavailable,
		apierrors.NewBadRequest("bad"):                             protocol.CodeBadRequest,
		errors.New("boom"):                                         protocol.CodeInternal,
		&protocol.Error{Code: protocol.CodeTooLarge, Message: "x"}: protocol.CodeTooLarge,
	} {
		if got := code(fail(err)); got != want {
			t.Errorf("fail(%v) = %q, want %q", err, got, want)
		}
	}
	// A request without a deadline gets one.
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	h2, kube, _ := newHandler(t, nil)
	kube.PrependReactor("*", "*", func(clienttesting.Action) (bool, runtime.Object, error) { return true, nil, ctx.Err() })
	if c := code(h2.Handle(ctx, &protocol.Request{Op: protocol.OpTokenReview, TokenReview: &protocol.TokenReviewRequest{Token: "t", Audience: protocol.NodeTokenAudience}})); c != protocol.CodeUnavailable {
		t.Errorf("expired context: %q", c)
	}
}

// review-km-44 M1: the path trick never reaches the API, and isSecret
// holds for every group.
func TestPathTrickNeverReachesTheAPI(t *testing.T) {
	h, kube, dyn := newHandler(t, nil)
	for _, req := range []*protocol.Request{
		{Op: protocol.OpGet, Group: "x", Version: "../../api/v1", Resource: "secrets", Namespace: "kubemap-system", Name: "kubemap"},
		{Op: protocol.OpGet, Group: "x", Version: "v1", Resource: "secrets", Namespace: "kubemap-system", Name: "kubemap"},
		{Op: protocol.OpList, Group: "x", Version: "../../api/v1", Resource: "secrets", Namespace: "kubemap-system"},
		{Op: protocol.OpGet, Group: "cert-manager.io", Version: "v1/../../../api/v1", Resource: "configmaps", Namespace: "a", Name: "b"},
	} {
		if c := code(do(h, req)); c != protocol.CodeBadRequest && c != protocol.CodeForbidden {
			t.Errorf("%+v: %q", req, c)
		}
	}
	if n := len(dyn.Actions()) + len(kube.Actions()); n != 0 {
		t.Errorf("the API was called %d times", n)
	}
	for _, g := range []string{"", "core", "x", "apps"} {
		if !isSecret(gvr(g, "v1", "secrets")) || !isSecret(gvr(g, "v1", " Secrets")) {
			t.Errorf("isSecret(%q) = false", g)
		}
	}
	if isSecret(gvr("", "v1", "configmaps")) {
		t.Error("configmaps are not secrets")
	}
}

// review-km-44 L7: an authenticated token's expiry comes back from its exp
// claim; an unauthenticated one gets none.
func TestTokenReviewExpiry(t *testing.T) {
	h, kube, _ := newHandler(t, nil)
	exp := time.Date(2026, 9, 28, 15, 0, 0, 0, time.UTC)
	payload := base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"aud":["kubemap-portal"],"exp":%d,"sub":"system:serviceaccount:kubemap-node:kubemap-node-agent"}`, exp.Unix()))
	token := "eyJhbGciOiJSUzI1NiJ9." + payload + ".sig"
	authenticated := true
	kube.PrependReactor("create", "tokenreviews", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{Authenticated: authenticated, Audiences: []string{protocol.NodeTokenAudience}}}, nil
	})
	r := do(h, &protocol.Request{Op: protocol.OpTokenReview, TokenReview: &protocol.TokenReviewRequest{Token: token, Audience: protocol.NodeTokenAudience}})
	if r.Error != nil || r.TokenReview.ExpiresAt == nil || !r.TokenReview.ExpiresAt.Equal(exp) {
		t.Fatalf("expiry = %+v %+v", r.Error, r.TokenReview)
	}
	authenticated = false
	r = do(h, &protocol.Request{Op: protocol.OpTokenReview, TokenReview: &protocol.TokenReviewRequest{Token: token, Audience: protocol.NodeTokenAudience}})
	if r.TokenReview == nil || r.TokenReview.ExpiresAt != nil {
		t.Errorf("an unauthenticated token's claims are not trusted: %+v", r.TokenReview)
	}
}
