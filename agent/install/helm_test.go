package install

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const chartDir = "../../deploy/helm/kubemap"

func helmTemplate(t *testing.T, args ...string) []byte {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		if os.Getenv("KUBEMAP_REQUIRE_HELM") != "" {
			t.Fatal("helm is not on PATH")
		}
		t.Skip("helm is not on PATH")
	}
	cmd := exec.Command("helm", append([]string{"template", "kubemap", chartDir, "-n", "kubemap-system"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, stderr.String())
	}
	return out
}

// idsOf lists "Kind namespace/name" for every document.
func idsOf(t *testing.T, raw []byte) []string {
	t.Helper()
	var ids []string
	for _, doc := range bytes.Split(raw, []byte("\n---")) {
		var o struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(doc, &o); err != nil {
			t.Fatal(err)
		}
		if o.Kind != "" {
			ids = append(ids, o.Kind+" "+o.Metadata.Namespace+"/"+o.Metadata.Name)
		}
	}
	sort.Strings(ids)
	return ids
}

// The chart with its defaults must pass the same read-only rules as deploy/*.yaml.
func TestHelmChartIsReadOnly(t *testing.T) {
	checkReadOnlyDocs(t, "helm template (defaults)", helmTemplate(t))
}

// The chart and the manifests must not drift: same kinds, namespaces and names.
// The only difference allowed is the Namespace kubemap-system, which
// `helm install --create-namespace` creates and the chart does not.
func TestHelmChartMatchesManifests(t *testing.T) {
	rendered := idsOf(t, helmTemplate(t))
	var want []string
	for _, f := range []string{"kubemap.yaml", "networkpolicy.yaml", "ingress.yaml"} {
		raw, err := os.ReadFile(filepath.Join("../../deploy", f))
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, idsOf(t, raw)...)
	}
	var wantNoNS []string
	for _, id := range want {
		if id != "Namespace /kubemap-system" {
			wantNoNS = append(wantNoNS, id)
		}
	}
	sort.Strings(wantNoNS)
	if strings.Join(rendered, "\n") != strings.Join(wantNoNS, "\n") {
		t.Errorf("chart and deploy manifests differ.\nchart:\n%s\nmanifests:\n%s", strings.Join(rendered, "\n"), strings.Join(wantNoNS, "\n"))
	}
}

// Every object of the chart must equal the manifest object, field by field,
// except: the image tag (chart pins a tag, manifest uses :dev), status, the
// PVC keep annotation, and empty spec/annotations noise.
func TestHelmChartObjectsMatchManifests(t *testing.T) {
	load := func(raw []byte) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, doc := range bytes.Split(raw, []byte("\n---")) {
			var o map[string]any
			if err := yaml.Unmarshal(doc, &o); err != nil || o["kind"] == nil {
				continue
			}
			delete(o, "status")
			md, _ := o["metadata"].(map[string]any)
			if o["kind"] == "Namespace" {
				delete(o, "spec")
			}
			if o["kind"] == "PersistentVolumeClaim" {
				delete(md, "annotations")
			}
			b, _ := yaml.Marshal(o)
			var norm map[string]any
			_ = yaml.Unmarshal([]byte(regexp.MustCompile(`localhost/kubemap:[^"\s]+`).ReplaceAllString(string(b), "localhost/kubemap:IMG")), &norm)
			ns, _ := md["namespace"].(string)
			out[o["kind"].(string)+" "+ns+"/"+md["name"].(string)] = norm
		}
		return out
	}
	want := map[string]map[string]any{}
	for _, f := range []string{"kubemap.yaml", "networkpolicy.yaml", "ingress.yaml"} {
		raw, err := os.ReadFile(filepath.Join("../../deploy", f))
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range load(raw) {
			want[k] = v
		}
	}
	delete(want, "Namespace /kubemap-system")
	got := load(helmTemplate(t))
	for k, w := range want {
		gb, _ := yaml.Marshal(got[k])
		wb, _ := yaml.Marshal(w)
		if string(gb) != string(wb) {
			t.Errorf("%s differs between the chart and the manifests:\nchart:\n%s\nmanifest:\n%s", k, gb, wb)
		}
	}
}

func TestHelmChartVariations(t *testing.T) {
	has := func(out, w string) bool { return strings.Contains(out, w) }
	out := string(helmTemplate(t, "--set", "ingress.enabled=false", "--set", "networkPolicy.enabled=false"))
	for _, w := range []string{"kind: Certificate", "kind: IngressRoute", "kind: Middleware", "kind: NetworkPolicy"} {
		if has(out, w) {
			t.Errorf("disabled ingress/networkPolicy still renders %q", w)
		}
	}
	if out := string(helmTemplate(t, "--set", "persistence.keepOnUninstall=false")); has(out, "helm.sh/resource-policy") {
		t.Error("keepOnUninstall=false still has the keep annotation")
	}
	out = string(helmTemplate(t, "--set", "trustedProxies=10.0.0.0/8", "--set", "persistence.storageClass=fast",
		"--set", "imagePullSecrets[0].name=regcred", "--set-string", "image.tag=1234567"))
	for _, w := range []string{"KUBEMAP_TRUSTED_PROXIES", "storageClassName: \"fast\"", "name: regcred", "localhost/kubemap:1234567"} {
		if !has(out, w) {
			t.Errorf("variation output lacks %q", w)
		}
	}
	if n := strings.Count(out, "name: regcred"); n != 2 {
		t.Errorf("imagePullSecrets in %d pod specs, want 2 (server and node agent)", n)
	}
}

// Chart.appVersion and values image.tag are kept equal; the Host() rules are quoted.
// Set KUBEMAP_REQUIRE_HELM=1 in CI to fail instead of skip when helm is missing.
func TestHelmChartDefaults(t *testing.T) {
	out := string(helmTemplate(t))
	chart, err := os.ReadFile(filepath.Join(chartDir, "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		AppVersion string `json:"appVersion"`
	}
	if err := yaml.Unmarshal(chart, &c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "image: \"localhost/kubemap:"+c.AppVersion+"\"") {
		t.Errorf("default image tag is not Chart appVersion %q", c.AppVersion)
	}
	for _, w := range []string{
		"Host(`kubemap.albertycliu.xyz`) && !PathPrefix(`/v1/connect`)",
		"helm.sh/resource-policy: keep",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("rendered chart lacks %q", w)
		}
	}
	// A wrong namespace or an unsafe host must be refused, with our message.
	fails := []struct {
		args []string
		msg  string
	}{
		{[]string{"-n", "kubemap-system", "--set", "ingress.host=a`b"}, "ingress.host must be a DNS name"},
		{[]string{"-n", "kubemap-system", "--set", "ingress.issuer=x y"}, "ingress.issuer must be a ClusterIssuer name"},
		{[]string{"-n", "kubemap-system", "--set", "trustedProxies=evil;rm"}, "trustedProxies must be"},
		{[]string{"-n", "other"}, "must be installed in the namespace kubemap-system"},
	}
	for _, f := range fails {
		var stderr bytes.Buffer
		cmd := exec.Command("helm", append([]string{"template", "kubemap", chartDir}, f.args...)...)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err == nil || !strings.Contains(stderr.String(), f.msg) {
			t.Errorf("helm template %v: want failure containing %q, got err=%v stderr=%s", f.args, f.msg, err, stderr.String())
		}
	}
}
