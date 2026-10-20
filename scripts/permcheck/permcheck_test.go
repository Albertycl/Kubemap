package main

import (
	"bytes"
	"strings"
	"testing"

	"kubemap/agent/install"
)

const checksFile = "../agent-permission-checks.txt"

func bundle(t *testing.T) []byte {
	t.Helper()
	b, err := install.Bundle(install.Options{Image: "localhost/kubemap:test"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func objects(t *testing.T, docs ...[]byte) *Objects {
	t.Helper()
	o := &Objects{}
	for i, d := range docs {
		if err := o.Read(bytes.NewReader(d), "doc"+string(rune('0'+i))); err != nil {
			t.Fatal(err)
		}
	}
	return o
}

// The real bundle passes the audit and every check.
func TestBundlePasses(t *testing.T) {
	checks, err := readChecks(checksFile)
	if err != nil {
		t.Fatal(err)
	}
	o := objects(t, bundle(t))
	if len(o.ClusterRoles) != 2 || len(o.Roles) != 0 || len(o.ClusterRoleBindings) != 2 || len(o.RoleBindings) != 0 {
		t.Fatalf("unexpected bundle RBAC: %d CR, %d R, %d CRB, %d RB", len(o.ClusterRoles), len(o.Roles), len(o.ClusterRoleBindings), len(o.RoleBindings))
	}
	for _, f := range Audit(o) {
		if !f.OK {
			t.Errorf("audit: %s", f)
		}
	}
	for _, a := range Agents {
		for _, c := range checks {
			if !c.AppliesTo(a) {
				continue
			}
			for _, ns := range c.Scopes() {
				if got, by := o.Allowed(a, c, ns); got != (c.Expect == "allow") {
					t.Errorf("line %d, %s in %q: allowed=%v (%s), want %s", c.Line, a.Short, ns, got, by, c.Expect)
				}
			}
		}
	}
}

// The full run on the bundle prints PASS and exits 0 (the bundle has no
// nodes/stats since the no-kubelet decision, km-60-no-kubelet).
func TestRunOnBundle(t *testing.T) {
	path := t.TempDir() + "/bundle.yaml"
	if err := writeFile(path, bundle(t)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := run(&out, checksFile, []string{path}); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "RESULT: PASS") || !strings.Contains(out.String(), "approved exception: TokenReview") {
		t.Fatalf("output:\n%s", out.String())
	}
}

// Each way of weakening the bundle is caught, by the audit, a check, or both.
func TestWeakenedBundlesFail(t *testing.T) {
	cases := map[string]string{
		"secrets in the read-only role": `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: kubemap-agent-readonly}
rules:
- {apiGroups: [""], resources: [pods, secrets], verbs: [get, list, watch]}`,
		"a wildcard": `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: kubemap-agent-readonly}
rules:
- {apiGroups: ["*"], resources: ["*"], verbs: [get, list, watch]}`,
		"a write verb": `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: kubemap-agent-readonly}
rules:
- {apiGroups: [apps], resources: [deployments], verbs: [get, list, watch, patch]}`,
		"exec": `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: kubemap-agent-readonly}
rules:
- {apiGroups: [""], resources: [pods/exec], verbs: [create]}`,
		"pods/proxy as a read": `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: kubemap-agent-readonly}
rules:
- {apiGroups: [""], resources: [pods, pods/log, pods/proxy], verbs: [get, list, watch]}`,
		"services/proxy as a read": `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: kubemap-agent-readonly}
rules:
- {apiGroups: [""], resources: [services, services/proxy], verbs: [get, list, watch]}`,
		"pods/attach and portforward as reads": `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: kubemap-agent-readonly}
rules:
- {apiGroups: [""], resources: [pods/attach, pods/portforward], verbs: [get]}`,
		"eviction": `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: kubemap-agent-readonly}
rules:
- {apiGroups: [""], resources: [pods/eviction], verbs: [create]}`,
		"ephemeral containers": `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: kubemap-agent-readonly}
rules:
- {apiGroups: [""], resources: [pods/ephemeralcontainers], verbs: [patch]}`,
		"job create": `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: kubemap-agent-readonly}
rules:
- {apiGroups: [batch], resources: [jobs, cronjobs], verbs: [create]}`,
		"a Secret rule": `
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: kubemap-agent-credential, namespace: kubemap-system}
rules:
- {apiGroups: [""], resources: [secrets], verbs: [get, update]}`,
		"a binding to every ServiceAccount": `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: kubemap-agent-readonly}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: kubemap-agent-readonly}
subjects:
- {kind: Group, name: system:serviceaccounts}`,
		"a binding to a role outside the files": `
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: kubemap-agent-admin}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: cluster-admin}
subjects:
- {kind: ServiceAccount, name: kubemap, namespace: kubemap-system}`,
		"node agent given pod create in its namespace": `
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata: {name: extra, namespace: kubemap-node}
rules:
- {apiGroups: [""], resources: [pods], verbs: [create]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: extra, namespace: kubemap-node}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: Role, name: extra}
subjects:
- {kind: ServiceAccount, name: kubemap-node-agent}`,
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			base := dir + "/bundle.yaml"
			// The bundle with the extra objects replacing same-named ones.
			if err := writeFile(base, append(bundle(t), []byte("\n---\n"+strings.TrimSpace(extra)+"\n")...)); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if code := run(&out, checksFile, []string{base}); code != 1 {
				t.Fatalf("exit %d, want 1:\n%s", code, out.String())
			}
			if !strings.Contains(out.String(), "FAIL") {
				t.Fatalf("no FAIL line:\n%s", out.String())
			}
		})
	}
}

// A RoleBinding grants only in its namespace, never cluster-scoped objects.
func TestRoleBindingScope(t *testing.T) {
	o := objects(t, []byte(`
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata: {name: edit-ish}
rules:
- {apiGroups: [""], resources: [pods, nodes], verbs: [delete]}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: x, namespace: default}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: edit-ish}
subjects:
- {kind: ServiceAccount, name: kubemap, namespace: kubemap-system}`))
	a := Agents[0]
	pods := Check{Verb: "delete", Resource: "pods"}
	nodes := Check{Verb: "delete", Resource: "nodes"}
	if ok, _ := o.Allowed(a, pods, "default"); !ok {
		t.Error("delete pods in default should be allowed by the RoleBinding")
	}
	if ok, _ := o.Allowed(a, pods, "kube-system"); ok {
		t.Error("delete pods in kube-system must not be allowed")
	}
	if ok, _ := o.Allowed(a, pods, ""); ok {
		t.Error("delete pods in every namespace must not be allowed")
	}
	if ok, _ := o.Allowed(a, nodes, "default"); ok {
		t.Error("a RoleBinding cannot grant cluster-scoped nodes")
	}
}

func TestReadChecksErrors(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"columns": "deny * * get\n",
		"expect":  "maybe * * get pods - -\n",
		"agent":   "deny portal * get pods - -\n",
		"empty":   "# nothing\n",
	} {
		p := dir + "/" + name
		if err := writeFile(p, []byte(body)); err != nil {
			t.Fatal(err)
		}
		if _, err := readChecks(p); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
