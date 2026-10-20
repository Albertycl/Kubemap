package yamlview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"sigs.k8s.io/yaml"

	"kubemap/pkg/protocol"
	"kubemap/portal/store"
	"kubemap/portal/tunnel"
)

// web is a Deployment as the agent returns it: server fields, a status,
// kubectl's last-applied copy, and env values of every kind.
const web = `{
 "apiVersion": "apps/v1", "kind": "Deployment",
 "metadata": {"name": "web", "namespace": "shop", "resourceVersion": "4711", "uid": "u-1", "generation": 3,
  "creationTimestamp": "2026-09-28T01:00:00Z",
  "managedFields": [{"manager": "kubectl", "operation": "Apply"}],
  "labels": {"app": "web"},
  "annotations": {"kubectl.kubernetes.io/last-applied-configuration": "{\"spec\":{\"env\":\"DB_PASSWORD=hunter2\"}}"}},
 "spec": {"replicas": 2, "selector": {"matchLabels": {"app": "web"}},
  "template": {"metadata": {"labels": {"app": "web"}},
   "spec": {"containers": [
    {"name": "app", "image": "shop/web:1.0", "env": [
      {"name": "DB_PASSWORD", "value": "hunter2"},
      {"name": "DATABASE_URL", "value": "postgres://shop:s3cret@db:5432/shop"},
      {"name": "LOG_LEVEL", "value": "info"},
      {"name": "API_TOKEN", "valueFrom": {"secretKeyRef": {"name": "shop", "key": "token"}}}]},
    {"name": "sidecar", "image": "envoy:1.30"}]}}},
 "status": {"readyReplicas": 2}
}`

func mustView(t *testing.T, raw string) (*View, map[string]any) {
	t.Helper()
	v, red, err := NewView([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return v, red
}

func TestRedactDeployment(t *testing.T) {
	v, _ := mustView(t, web)
	for _, secret := range []string{"hunter2", "s3cret", "managedFields"} {
		if strings.Contains(v.YAML, secret) {
			t.Errorf("the view shows %q:\n%s", secret, v.YAML)
		}
	}
	for _, kept := range []string{"LOG_LEVEL", "value: info", "secretKeyRef", "image: shop/web:1.0", "readyReplicas: 2"} {
		if !strings.Contains(v.YAML, kept) {
			t.Errorf("the view lost %q", kept)
		}
	}
	want := []string{
		"metadata.annotations.kubectl.kubernetes.io/last-applied-configuration",
		"spec.template.spec.containers[app].env[DATABASE_URL]",
		"spec.template.spec.containers[app].env[DB_PASSWORD]",
	}
	if strings.Join(v.Redacted, "|") != strings.Join(want, "|") {
		t.Errorf("redacted = %v", v.Redacted)
	}
	if v.Kind != "Deployment" || v.APIVersion != "apps/v1" || v.ResourceVersion != "4711" {
		t.Errorf("view = %+v", v)
	}
}

func TestRedactOtherKinds(t *testing.T) {
	cm, _ := mustView(t, `{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c","namespace":"n"},
	  "data":{"api_token":"abc","smtp_password":"p","greeting":"hello","redis_url":"redis://:pw@redis:6379"},"binaryData":{"keystore":"AAAA"}}`)
	if strings.Contains(cm.YAML, "abc") || strings.Contains(cm.YAML, ": p\n") || strings.Contains(cm.YAML, "AAAA") || !strings.Contains(cm.YAML, "hello") {
		t.Errorf("configmap view:\n%s", cm.YAML)
	}
	// redis://:pw@ has no user name and is caught too (review km-39 N-2).
	if len(cm.Redacted) != 4 || strings.Contains(cm.YAML, "redis://:pw@") {
		t.Errorf("configmap redacted = %v", cm.Redacted)
	}
	sec, _ := mustView(t, `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"s","namespace":"n"},"data":{"a":"c2VjcmV0"},"stringData":{"b":"x"}}`)
	if strings.Contains(sec.YAML, "c2VjcmV0") || len(sec.Redacted) != 2 {
		t.Errorf("secret view:\n%s %v", sec.YAML, sec.Redacted)
	}
	pod, _ := mustView(t, `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p","namespace":"n"},
	  "spec":{"initContainers":[{"name":"init","env":[{"name":"GITHUB_TOKEN","value":"ghp_x"}]}],"containers":[{"name":"c"}]}}`)
	if strings.Contains(pod.YAML, "ghp_x") {
		t.Errorf("pod view:\n%s", pod.YAML)
	}
	cj, _ := mustView(t, `{"apiVersion":"batch/v1","kind":"CronJob","metadata":{"name":"r","namespace":"n"},
	  "spec":{"jobTemplate":{"spec":{"template":{"spec":{"containers":[{"name":"c","env":[{"name":"AWS_SECRET_ACCESS_KEY","value":"k"}]}]}}}}}}`)
	if strings.Contains(cj.YAML, "value: k") || len(cj.Redacted) != 1 {
		t.Errorf("cronjob view:\n%s", cj.YAML)
	}
	argo, _ := mustView(t, `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"u","namespace":"n","labels":{"argocd.argoproj.io/instance":"url-shortener"}},"spec":{}}`)
	if argo.ArgoApp != "url-shortener" {
		t.Errorf("argo app = %q", argo.ArgoApp)
	}
}

var webTarget = Target{Group: "apps", Version: "v1", Resource: "deployments", Namespace: "shop", Name: "web"}

// edit takes the view's YAML and replaces old by new.
func edit(t *testing.T, v *View, pairs ...string) string {
	t.Helper()
	s := v.YAML
	for i := 0; i < len(pairs); i += 2 {
		if !strings.Contains(s, pairs[i]) {
			t.Fatalf("edit: %q not in the view", pairs[i])
		}
		s = strings.Replace(s, pairs[i], pairs[i+1], 1)
	}
	return s
}

// The central property: the patch, applied to the real object (with its
// real secret values), makes exactly the edit and keeps every secret.
func TestPatchKeepsRedactedValues(t *testing.T) {
	v, _ := mustView(t, web)
	res, err := Diff(webTarget, []byte(web), edit(t, v, "image: shop/web:1.0", "image: shop/web:1.1", "replicas: 2", "replicas: 3"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.PatchType != "strategic" {
		t.Fatalf("result = %+v", res)
	}
	if strings.Contains(res.Patch, "hunter2") || strings.Contains(res.Patch, Placeholder) || !strings.Contains(res.Patch, `"resourceVersion":"4711"`) {
		t.Errorf("patch = %s", res.Patch)
	}
	merged, err := strategicpatch.StrategicMergePatch([]byte(web), []byte(res.Patch), appsv1.Deployment{})
	if err != nil {
		t.Fatal(err)
	}
	var d appsv1.Deployment
	json.Unmarshal(merged, &d)
	c := d.Spec.Template.Spec.Containers
	if *d.Spec.Replicas != 3 || len(c) != 2 || c[0].Image != "shop/web:1.1" || c[1].Image != "envoy:1.30" {
		t.Fatalf("merged = %+v", d.Spec)
	}
	env := map[string]string{}
	for _, e := range c[0].Env {
		env[e.Name] = e.Value
	}
	if env["DB_PASSWORD"] != "hunter2" || env["DATABASE_URL"] != "postgres://shop:s3cret@db:5432/shop" || env["LOG_LEVEL"] != "info" {
		t.Errorf("the patch changed a redacted value: %v", env)
	}
	if !strings.Contains(d.Annotations["kubectl.kubernetes.io/last-applied-configuration"], "hunter2") {
		t.Error("the patch changed the last-applied annotation")
	}
	// The diff shows just the two changed lines, each as - and +.
	var changed []string
	for _, l := range res.Diff {
		if l.Op != " " {
			changed = append(changed, l.Op+strings.TrimSpace(l.Text))
		}
	}
	if strings.Join(changed, "|") != "-replicas: 2|+replicas: 3|-image: shop/web:1.0|+image: shop/web:1.1" {
		t.Errorf("diff = %v", changed)
	}
	if len(res.Commands) != 2 ||
		!strings.HasPrefix(res.Commands[0].Command, "kubectl -n 'shop' patch 'deployments.apps' 'web' --type strategic -p '{") ||
		!strings.HasSuffix(res.Commands[0].Command, "' --dry-run=server -o yaml") ||
		strings.Contains(res.Commands[1].Command, "dry-run") {
		t.Errorf("commands = %+v", res.Commands)
	}
}

// Setting a new literal for a redacted value is an edit like any other; the
// patch holds the new value, never the placeholder.
func TestReplaceRedactedValue(t *testing.T) {
	v, _ := mustView(t, web)
	// DB_PASSWORD is the first redacted env value in the view.
	ed := edit(t, v, "value: "+Placeholder, "value: new-one")
	res, err := Diff(webTarget, []byte(web), ed)
	if err != nil || !strings.Contains(res.Patch, "new-one") || strings.Contains(res.Patch, "s3cret") {
		t.Fatalf("%v %+v", err, res)
	}
}

func TestEditRefusals(t *testing.T) {
	v, _ := mustView(t, web)
	for name, tc := range map[string]struct{ edited, want string }{
		"not yaml":      {"a: [", "not valid YAML"},
		"a list":        {"- a\n- b\n", "one object"},
		"other name":    {edit(t, v, "name: web\n  namespace", "name: web2\n  namespace"), "metadata.name changed"},
		"other ns":      {edit(t, v, "namespace: shop", "namespace: prod"), "metadata.namespace changed"},
		"other kind":    {edit(t, v, "kind: Deployment", "kind: StatefulSet"), "kind changed"},
		"other version": {edit(t, v, "apiVersion: apps/v1", "apiVersion: apps/v1beta1"), "apiVersion changed"},
	} {
		_, err := Diff(webTarget, []byte(web), tc.edited)
		var ee *ErrEdit
		if !errors.As(err, &ee) || !strings.Contains(ee.Msg, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A custom resource gets a JSON merge patch, which replaces a list
	// whole: an image change would write the redacted env back. Refused.
	rollout := strings.Replace(strings.Replace(web, `"apiVersion": "apps/v1", "kind": "Deployment"`, `"apiVersion": "argoproj.io/v1alpha1", "kind": "Rollout"`, 1), `"replicas": 2`, `"replicas": 2`, 1)
	rv, _ := mustView(t, rollout)
	_, err := Diff(Target{Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts", Namespace: "shop", Name: "web"}, []byte(rollout),
		edit(t, rv, "image: shop/web:1.0", "image: shop/web:1.1"))
	var ee *ErrEdit
	if !errors.As(err, &ee) || !strings.Contains(ee.Msg, "redacted value") {
		t.Errorf("merge patch over a redacted list: %v", err)
	}
	// The same CR with a change outside the list is fine, as a merge patch.
	res, err := Diff(Target{Group: "argoproj.io", Version: "v1alpha1", Resource: "rollouts", Namespace: "shop", Name: "web"}, []byte(rollout),
		edit(t, rv, "replicas: 2", "replicas: 4"))
	if err != nil || res.PatchType != "merge" || res.Patch != `{"metadata":{"resourceVersion":"4711"},"spec":{"replicas":4}}` {
		t.Errorf("merge patch: %v %+v", err, res)
	}
}

func TestNoChangeAndNotes(t *testing.T) {
	v, _ := mustView(t, web)
	// Only server fields and status differ: no change, status note.
	ed := edit(t, v, "resourceVersion: \"4711\"", "resourceVersion: \"1\"", "readyReplicas: 2", "readyReplicas: 9")
	res, err := Diff(webTarget, []byte(web), ed)
	if err != nil || res.Changed || len(res.Commands) != 0 || !strings.Contains(strings.Join(res.Notes, " "), "status changes are ignored") {
		t.Errorf("%v %+v", err, res)
	}
	// Argo CD-managed and Pod notes.
	argo := strings.Replace(web, `"labels": {"app": "web"},`, `"labels": {"app": "web", "argocd.argoproj.io/instance": "shop"},`, 1)
	av, _ := mustView(t, argo)
	res, err = Diff(webTarget, []byte(argo), edit(t, av, "replicas: 2", "replicas: 3"))
	if err != nil || !strings.Contains(strings.Join(res.Notes, " "), "Managed by Argo CD (shop)") {
		t.Errorf("argo: %v %+v", err, res)
	}
}

func TestLineDiff(t *testing.T) {
	d, _ := lineDiff("a\nb\nc\n", "a\nx\nc\nd\n")
	var got []string
	for _, l := range d {
		got = append(got, l.Op+l.Text)
	}
	if strings.Join(got, ",") != " a,-b,+x, c,+d" {
		t.Errorf("diff = %v", got)
	}
	if _, err := lineDiff(strings.Repeat("x\n", maxLines+1), "x"); err == nil {
		t.Error("a huge object must be refused")
	}
}

// fakeAgent answers get with web, and refuses Secrets like the agent.
type fakeAgent struct {
	down bool
	reqs []*protocol.Request
}

func (f *fakeAgent) Request(_ context.Context, _ string, req *protocol.Request) (*protocol.Response, error) {
	f.reqs = append(f.reqs, req)
	if f.down {
		return nil, tunnel.ErrNotConnected
	}
	if req.Op != protocol.OpGet {
		return &protocol.Response{Error: &protocol.Error{Code: protocol.CodeBadRequest, Message: "only get"}}, nil
	}
	if req.Name != "web" {
		return &protocol.Response{Error: &protocol.Error{Code: protocol.CodeNotFound, Message: "not found"}}, nil
	}
	return &protocol.Response{Object: json.RawMessage(web)}, nil
}

func newAPI(t *testing.T) (*http.ServeMux, *fakeAgent, store.Cluster) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "portal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	c, _ := st.EnsureCluster(ctx, "c-k3s-home", "k3s-home")
	f := &fakeAgent{}
	mux := http.NewServeMux()
	require := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Test-Login") != "yes" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
	NewAPI(st, f).Routes(require)(mux)
	return mux, f, c
}

func call(mux *http.ServeMux, method, path string, body any) (int, map[string]any) {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("X-Test-Login", "yes")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestAPI(t *testing.T) {
	mux, f, c := newAPI(t)
	base := "/api/clusters/" + c.ID + "/yaml"
	q := "?group=apps&version=v1&resource=deployments&namespace=shop&name=web"
	code, v := call(mux, "GET", base+q, nil)
	if code != http.StatusOK || strings.Contains(v["yaml"].(string), "hunter2") || v["resourceVersion"] != "4711" {
		t.Fatalf("view: %d %v", code, v)
	}
	// Secrets are refused before the agent is asked (protocol validation).
	n := len(f.reqs)
	if code, _ := call(mux, "GET", base+"?version=v1&resource=secrets&namespace=shop&name=db", nil); code != http.StatusForbidden || len(f.reqs) != n {
		t.Errorf("secret view: %d, agent asked %d times", code, len(f.reqs)-n)
	}
	// review-km-44 M1: the path trick and other spellings are refused
	// before the agent is asked.
	for _, q := range []string{"?group=x&version=../../api/v1&resource=secrets&namespace=kubemap-system&name=kubemap-agent",
		"?version=v1&resource=Secrets&namespace=a&name=b", "?group=x&version=v1&resource=secrets&namespace=a&name=b"} {
		n := len(f.reqs)
		if code, _ := call(mux, "GET", base+q, nil); code != http.StatusForbidden || len(f.reqs) != n {
			t.Errorf("%s: %d, agent asked %d times", q, code, len(f.reqs)-n)
		}
	}
	if code, _ := call(mux, "GET", base+"?group=apps&version=../../api/v1&resource=configmaps&namespace=a&name=b", nil); code != http.StatusBadRequest {
		t.Errorf("version trick: %d", code)
	}
	if code, _ := call(mux, "GET", base+"?group=apps&version=v1&resource=deployments&namespace=shop&name=nope", nil); code != http.StatusNotFound {
		t.Errorf("missing object: %d", code)
	}

	edited := strings.Replace(v["yaml"].(string), "replicas: 2", "replicas: 5", 1)
	body := map[string]any{"group": "apps", "version": "v1", "resource": "deployments", "namespace": "shop", "name": "web", "edited": edited}
	code, res := call(mux, "POST", base+"/diff", body)
	if code != http.StatusOK || res["changed"] != true || !strings.Contains(res["patch"].(string), `"replicas":5`) {
		t.Fatalf("diff: %d %v", code, res)
	}
	// Only gets reach the agent: the portal never sends a write.
	for _, r := range f.reqs {
		if r.Op != protocol.OpGet {
			t.Errorf("the agent got %s", r.Op)
		}
	}
	body["edited"] = strings.Replace(edited, "name: web\n", "name: other\n", 1)
	if code, res := call(mux, "POST", base+"/diff", body); code != http.StatusUnprocessableEntity || res["error"] != "bad_edit" {
		t.Errorf("bad edit: %d %v", code, res)
	}
	body["edited"] = strings.Repeat("x", maxEdited+1)
	if code, _ := call(mux, "POST", base+"/diff", body); code != http.StatusRequestEntityTooLarge && code != http.StatusBadRequest {
		t.Errorf("huge edit: %d", code)
	}
	f.down = true
	if code, _ := call(mux, "GET", base+q, nil); code != http.StatusServiceUnavailable && code != http.StatusBadGateway {
		t.Errorf("agent down: %d", code)
	}
}

// The view is valid YAML that round-trips to the redacted object.
func TestViewIsYAML(t *testing.T) {
	v, red := mustView(t, web)
	var back map[string]any
	if err := yaml.Unmarshal([]byte(v.YAML), &back); err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(back)
	b, _ := json.Marshal(red)
	if !bytes.Equal(a, b) {
		t.Error("the YAML does not round-trip")
	}
}

// Review km-39 N-1 and N-2: GitOps values are hidden as a whole, and a URL
// password without a user name is caught.
func TestRedactChartValuesAndBareURLPassword(t *testing.T) {
	app := map[string]any{"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": "shop", "namespace": "argocd"},
		"spec": map[string]any{
			"source": map[string]any{"repoURL": "https://git", "helm": map[string]any{
				"values": "db:\n  password: hunter2\n", "parameters": []any{map[string]any{"name": "a", "value": "b"}}, "releaseName": "shop"}},
			"sources": []any{map[string]any{"repoURL": "https://git2",
				"helm":   map[string]any{"valuesObject": map[string]any{"token": "x"}},
				"plugin": map[string]any{"env": []any{map[string]any{"name": "K", "value": "v"}}}}},
		}}
	out, paths := Redact(app)
	want := []string{"spec.source.helm.parameters", "spec.source.helm.values", "spec.sources[0].helm.valuesObject", "spec.sources[0].plugin.env"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("paths %v, want %v", paths, want)
	}
	src := out["spec"].(map[string]any)["source"].(map[string]any)
	h := src["helm"].(map[string]any)
	if h["values"] != Placeholder || h["releaseName"] != "shop" || src["repoURL"] != "https://git" {
		t.Fatalf("source %v", src)
	}
	if app["spec"].(map[string]any)["source"].(map[string]any)["helm"].(map[string]any)["values"] == Placeholder {
		t.Fatal("the input was changed")
	}
	hr := map[string]any{"kind": "HelmRelease", "spec": map[string]any{"values": map[string]any{"a": 1}, "chart": "x"}}
	if _, paths := Redact(hr); strings.Join(paths, ",") != "spec.values" {
		t.Fatalf("HelmRelease paths %v", paths)
	}
	for _, v := range []string{"redis://:hunter2@redis:6379", "postgres://u:p@db/x"} {
		if !secretLooking("URL", v) {
			t.Errorf("%s not caught", v)
		}
	}
	if secretLooking("URL", "https://example.com/path") || secretLooking("URL", "http://host:8080/") {
		t.Error("a URL without a password was caught")
	}
}
