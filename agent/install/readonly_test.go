package install

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Kubemap is read-only by design: it checks cluster status and never changes
// the cluster. This test reads every manifest in deploy/ and fails if a write
// verb, a wildcard, Secrets, exec, an operator role or binding, or an
// admission policy ever comes back. The only allowed non-read right is
// create on authentication.k8s.io/tokenreviews, which stores nothing: the
// server checks each node agent's token with it.
func TestDeployManifestsAreReadOnly(t *testing.T) {
	files, err := filepath.Glob("../../deploy/*.yaml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no deploy/*.yaml found: %v", err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		checkReadOnlyDocs(t, f, raw)
	}
}

// checkReadOnlyDocs applies the read-only rules to every YAML document in raw.
// The Helm chart test uses it on the rendered chart too.
func checkReadOnlyDocs(t *testing.T, f string, raw []byte) {
	t.Helper()
	for _, doc := range bytes.Split(raw, []byte("\n---")) {
		var o struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Rules []struct {
				APIGroups []string `json:"apiGroups"`
				Resources []string `json:"resources"`
				Verbs     []string `json:"verbs"`
			} `json:"rules"`
		}
		if err := yaml.Unmarshal(doc, &o); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		where := f + " " + o.Kind + " " + o.Metadata.Name
		switch o.Kind {
		case "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding", "MutatingAdmissionPolicy", "MutatingAdmissionPolicyBinding":
			t.Errorf("%s: no admission policies; they only existed to fence operator mode", where)
		case "ClusterRole", "Role", "ClusterRoleBinding", "RoleBinding":
			if strings.Contains(strings.ToLower(o.Metadata.Name), "operator") {
				t.Errorf("%s: operator roles and bindings are gone (read-only by design)", where)
			}
		}
		for _, r := range o.Rules {
			for _, v := range r.Verbs {
				isTokenReview := v == "create" && len(r.APIGroups) == 1 && r.APIGroups[0] == "authentication.k8s.io" &&
					len(r.Resources) == 1 && r.Resources[0] == "tokenreviews"
				if v != "get" && v != "list" && v != "watch" && !isTokenReview {
					t.Errorf("%s: verb %q on %v %v; only get, list, watch (and create tokenreviews) are allowed", where, v, r.APIGroups, r.Resources)
				}
			}
			for _, res := range r.Resources {
				if res == "*" || strings.Contains(res, "secret") || strings.HasSuffix(res, "/exec") || strings.HasSuffix(res, "/attach") ||
					strings.HasSuffix(res, "/portforward") || strings.HasPrefix(res, "nodes/") {
					t.Errorf("%s: forbidden resource %q", where, res)
				}
			}
			for _, g := range r.APIGroups {
				if g == "*" {
					t.Errorf("%s: wildcard API group", where)
				}
			}
		}
	}
}
