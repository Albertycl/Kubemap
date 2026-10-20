package hints

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestMatch(t *testing.T) {
	cases := []struct {
		name string
		s    Signals
		want string // first slug; "" means none
	}{
		{"pending", Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "Pending"}, "pod-pending-unschedulable"},
		{"creating", Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "ContainerCreating"}, "pod-containercreating-stuck"},
		{"init stuck", Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "Init:0/1"}, "pod-init-container-stuck"},
		{"init crash", Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "Init:CrashLoopBackOff"}, "pod-crashloopbackoff"},
		{"imagepull", Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "ImagePullBackOff"}, "pod-image-pull-failure"},
		{"errimagepull", Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "ErrImagePull"}, "pod-image-pull-failure"},
		{"image rule", Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "Running", Rules: []string{"pod.image-pull"}}, "pod-image-pull-failure"},
		{"crashloop", Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "CrashLoopBackOff"}, "pod-crashloopbackoff"},
		{"oom", Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "OOMKilled"}, "container-oomkilled"},
		{"evicted", Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "Evicted"}, "pod-evicted"},
		{"config error", Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "CreateContainerConfigError"}, "container-config-error"},
		{"terminating", Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "Terminating"}, "statefulset-pod-stuck-unknown-terminating"},
		{"running not ready", Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "Running", NotReady: true}, "pod-running-not-ready"},
		{"healthy", Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "Running"}, ""},
		{"qos", Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "Running", Rules: []string{"pod.no-memory-limit"}}, "pod-qos-class-risk"},
		{"node not ready", Signals{Kind: "Node", Name: "n1", Rules: []string{"node.not-ready"}}, "node-notready"},
		{"node pressure", Signals{Kind: "Node", Name: "n1", Rules: []string{"node.pressure.memory"}}, "node-pressure"},
		{"node disk", Signals{Kind: "Node", Name: "n1", Rules: []string{"node.disk.eviction"}}, "node-pressure"},
		// graph statuses (chain and app map pages)
		{"graph pending", Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "Pending (Unschedulable)"}, "pod-pending-unschedulable"},
		{"graph oom", Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "CrashLoopBackOff (OOMKilled)"}, "container-oomkilled"},
		{"graph not ready", Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "Running, not ready"}, "pod-running-not-ready"},
		// Service
		{"svc selector matches nothing", Signals{Kind: "Service", Namespace: "a", Name: "web", Status: "no pods match selector"}, "service-no-ready-endpoints"},
		{"svc missing", Signals{Kind: "Service", Namespace: "a", Name: "web", Missing: true}, "ingress-backend-misconfigured"},
		{"svc fine", Signals{Kind: "Service", Namespace: "a", Name: "web"}, ""},
		// Deployment
		{"deploy partly ready", Signals{Kind: "Deployment", Namespace: "a", Name: "web", Status: "2/3 ready"}, "deploy-rollout-stuck-old-pods-serving"},
		{"deploy none ready", Signals{Kind: "Deployment", Namespace: "a", Name: "web", Status: "0/3 ready"}, "deploy-unavailable"},
		{"deploy all ready", Signals{Kind: "Deployment", Namespace: "a", Name: "web", Status: "3/3 ready"}, ""},
		{"deploy scaled to zero", Signals{Kind: "Deployment", Namespace: "a", Name: "web", Status: "0/0 ready"}, ""},
		// StatefulSet
		{"sts partly ready", Signals{Kind: "StatefulSet", Namespace: "a", Name: "db", Status: "1/3 ready"}, "statefulset-ordered-ready-blocked"},
		{"sts ready", Signals{Kind: "StatefulSet", Namespace: "a", Name: "db", Status: "3/3 ready"}, ""},
		// PVC
		{"pvc pending", Signals{Kind: "PersistentVolumeClaim", Namespace: "a", Name: "data", Status: "Pending"}, "pvc-pending"},
		{"pvc bound", Signals{Kind: "PersistentVolumeClaim", Namespace: "a", Name: "data", Status: "Bound"}, ""},
		{"service ignores pod status", Signals{Kind: "Service", Namespace: "a", Name: "web", Status: "CrashLoopBackOff"}, ""},
		{"node ignores pod status", Signals{Kind: "Node", Name: "n1", Status: "Pending"}, ""},
	}
	for _, c := range cases {
		got := Match(c.s)
		if c.want == "" {
			if len(got) != 0 {
				t.Errorf("%s: got %v, want none", c.name, got)
			}
			continue
		}
		if len(got) == 0 || got[0].Slug != c.want {
			t.Errorf("%s: got %v, want %s", c.name, got, c.want)
		}
	}
}

func TestEveryWiredSlugIsInCatalogue(t *testing.T) {
	for _, s := range Wired() {
		e, ok := Catalogue()[s]
		if !ok || e.Meaning == "" || len(e.Check) == 0 || len(e.Notes) == 0 {
			t.Errorf("%s: missing or thin catalogue entry", s)
		}
	}
}

var readOnly = regexp.MustCompile(`^(kubectl( -n \S+| --namespace \S+| -A)* (get|describe|logs|top|events|auth can-i|version|api-resources|explain)|helm (get|list|status|history))\b`)

// A line a user may copy: a read-only command, or the assignment of a shell
// variable that a command uses.
var setVar = regexp.MustCompile(`^[A-Z][A-Z_]*=CHANGE_ME$`)
var quotedVar = regexp.MustCompile(`"\$[A-Z][A-Z_]*"`)

// Hints never act. The text a user would copy (after the placeholders are
// filled or turned into variables) is a read-only verb with no shell
// chaining, no redirection (< or >), no substitution. The check runs on the
// final text, for signals that fill nothing and for signals that fill the
// names, because a "<node>" left in a command is a redirection when pasted.
func TestCommandsAreReadOnly(t *testing.T) {
	signals := []Signals{
		{Kind: "Pod", Namespace: "a", Name: "p"},
		{Kind: "Pod", Namespace: "a", Name: "p", Node: "n1", Container: "c1", InitContainer: "i1"},
		{Kind: "Node", Name: "n1"},
		{Kind: "Service", Namespace: "a", Name: "s"},
		{Kind: "Deployment", Namespace: "a", Name: "d"},
		{Kind: "StatefulSet", Namespace: "a", Name: "d"},
		{Kind: "PersistentVolumeClaim", Namespace: "a", Name: "d"},
	}
	for slug, e := range Catalogue() {
		for _, s := range signals {
			for _, c := range fill(e, s).Check {
				if setVar.MatchString(c) {
					continue
				}
				final := quotedVar.ReplaceAllString(c, "x")
				if !readOnly.MatchString(c) || strings.ContainsAny(final, ";&|><`$\n") {
					t.Errorf("%s (%s): not a plain read-only command: %q", slug, s.Kind, c)
				}
			}
		}
	}
}

func TestUnknownPlaceholdersBecomeVariables(t *testing.T) {
	h := Match(Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "Init:0/1"})
	if len(h) != 1 {
		t.Fatal(h)
	}
	want := []string{
		"kubectl describe pod p -n a",
		"INIT_CONTAINER=CHANGE_ME",
		`kubectl logs p -n a -c "$INIT_CONTAINER"`,
		"kubectl get pod p -n a -o jsonpath='{.status.initContainerStatuses[*].state}'",
	}
	if strings.Join(h[0].Check, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s", strings.Join(h[0].Check, "\n"))
	}
	// With the name from the pod row nothing is left to set.
	h = Match(Signals{Kind: "Pod", Namespace: "a", Name: "p", Status: "Init:0/1", InitContainer: "wait-db"})
	if got := strings.Join(h[0].Check, "\n"); !strings.Contains(got, "-c wait-db") || strings.Contains(got, "CHANGE_ME") {
		t.Errorf("init container not filled:\n%s", got)
	}
	// A variable is declared once, before its first use.
	for _, hh := range Match(Signals{Kind: "Deployment", Namespace: "a", Name: "web", Status: "0/3 ready"}) {
		seen := map[string]bool{}
		for _, c := range hh.Check {
			if setVar.MatchString(c) {
				if seen[c] {
					t.Errorf("%s: %s declared twice", hh.Slug, c)
				}
				seen[c] = true
			}
			for _, v := range quotedVar.FindAllString(c, -1) {
				if !seen[strings.Trim(v, `"$`)+"=CHANGE_ME"] {
					t.Errorf("%s: %s used before it is set: %q", hh.Slug, v, c)
				}
			}
		}
	}
}

// Every new slug fills the names of its object into the commands.
func TestNewSlugsFillTheirObject(t *testing.T) {
	cases := []struct {
		s    Signals
		slug string
		want []string
	}{
		{Signals{Kind: "Service", Namespace: "shop", Name: "web", Status: "no pods match selector"}, "service-no-ready-endpoints", []string{"kubernetes.io/service-name=web", "-n shop"}},
		{Signals{Kind: "Deployment", Namespace: "shop", Name: "web", Status: "1/3 ready"}, "deploy-rollout-stuck-old-pods-serving", []string{"describe deployment web -n shop"}},
		{Signals{Kind: "Deployment", Namespace: "shop", Name: "web", Status: "0/3 ready"}, "deploy-unavailable", []string{"describe deployment web -n shop"}},
		{Signals{Kind: "StatefulSet", Namespace: "shop", Name: "db", Status: "1/3 ready"}, "statefulset-ordered-ready-blocked", []string{"describe sts db"}},
		{Signals{Kind: "PersistentVolumeClaim", Namespace: "shop", Name: "data", Status: "Pending"}, "pvc-pending", []string{"describe pvc data"}},
		{Signals{Kind: "Service", Namespace: "shop", Name: "web", Missing: true}, "ingress-backend-misconfigured", []string{"get svc,endpointslices -n shop"}},
	}
	for _, c := range cases {
		var got *Hint
		for _, h := range Match(c.s) {
			if h.Slug == c.slug {
				got = &h
			}
		}
		if got == nil {
			t.Errorf("%s: not returned for %+v", c.slug, c.s)
			continue
		}
		joined := strings.Join(got.Check, "\n")
		for _, w := range c.want {
			if !strings.Contains(joined, w) {
				t.Errorf("%s: %q missing in\n%s", c.slug, w, joined)
			}
		}
		if strings.ContainsAny(joined, "<>") {
			t.Errorf("%s: placeholder left:\n%s", c.slug, joined)
		}
	}
}

func TestPlaceholdersFilled(t *testing.T) {
	h := Match(Signals{Kind: "Pod", Namespace: "shop", Name: "web-1", Status: "CrashLoopBackOff"})
	if len(h) != 1 {
		t.Fatal(h)
	}
	joined := strings.Join(h[0].Check, "\n")
	if !strings.Contains(joined, "web-1") || !strings.Contains(joined, "shop") || strings.Contains(joined, "<pod>") || strings.Contains(joined, "<ns>") {
		t.Errorf("placeholders not filled:\n%s", joined)
	}
	n := Match(Signals{Kind: "Node", Name: "worker-2", Rules: []string{"node.not-ready"}})
	if !strings.Contains(strings.Join(n[0].Check, "\n"), "worker-2") {
		t.Errorf("node name missing: %v", n[0].Check)
	}
}

func TestAPI(t *testing.T) {
	mux := http.NewServeMux()
	API{}.Routes(func(h http.Handler) http.Handler { return h })(mux)
	get := func(q string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/hints?"+q, nil))
		return rec
	}
	if rec := get("kind=Pod&namespace=a&name=p&status=Evicted"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "pod-evicted") {
		t.Errorf("evicted: %d %s", rec.Code, rec.Body)
	}
	if rec := get("kind=Pod&namespace=a&name=p&status=Running"); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"hints":[]`) {
		t.Errorf("healthy: %d %s", rec.Code, rec.Body)
	}
	if rec := get("kind=Service&namespace=a&name=web&status=no+pods+match+selector"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "service-no-ready-endpoints") {
		t.Errorf("service: %d %s", rec.Code, rec.Body)
	}
	if rec := get("kind=PersistentVolumeClaim&namespace=a&name=data&status=Pending"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "pvc-pending") {
		t.Errorf("pvc: %d %s", rec.Code, rec.Body)
	}
	if rec := get("kind=Pod&namespace=a&name=p&status=Init:0/1&initContainer=wait-db"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "-c wait-db") {
		t.Errorf("init container: %d %s", rec.Code, rec.Body)
	}
	for _, q := range []string{"kind=Pod&namespace=a&name=p;rm", "kind=Pod&namespace=a&name=p&node=%24(x)", "kind=Pod&namespace=a&name=p&container=a%3Eb", "kind=Pod&namespace=a&name=p&initContainer=a%20b", "kind=Service&namespace=&name=s", "kind=Secret&namespace=a&name=s", "kind=Pod&namespace=&name=p", "kind=Job&name=p", "kind=Pod&namespace=a&name=%24(x)"} {
		if rec := get(q); rec.Code != 400 {
			t.Errorf("%s: %d, want 400", q, rec.Code)
		}
	}
}
