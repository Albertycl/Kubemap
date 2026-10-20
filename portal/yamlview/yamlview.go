// Package yamlview shows any object's YAML with secret-looking values
// redacted, and turns an edited copy into a diff, a minimal patch and the
// kubectl commands that dry-run and apply it (KM-39).
//
// Kubemap advises here; it does not apply (decision 2026-09-28). The agent
// stays within KM-38's four actions, and a server-side dry run needs the
// same RBAC as the write, so the dry run and the apply are commands for
// the admin to run with their own credentials. The patch holds only the
// changed fields and the object's resourceVersion, so it fails instead of
// overwriting a newer version, and it never writes a redacted placeholder.
package yamlview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/yaml"
)

// Placeholder replaces a redacted value. An edit that would write it back
// is refused.
const Placeholder = "<redacted by Kubemap>"

// lastApplied is kubectl's copy of the last applied object: it can hold
// any value of the object, so it is hidden as a whole.
const lastApplied = "kubectl.kubernetes.io/last-applied-configuration"

var (
	// secretName matches env var and ConfigMap key names that usually hold
	// a secret.
	secretName = regexp.MustCompile(`(?i)(passw|pwd|secret|token|api[_-]?key|private[_-]?key|credential|access[_-]?key|dsn|connection[_-]?string|auth[_-]?key)`)
	// urlPassword matches a URL with a password in it (postgres://u:p@h),
	// also without a user name (redis://:p@h, review km-39 N-2).
	urlPassword = regexp.MustCompile(`://[^/\s:@]*:[^/\s@]+@`)
)

func secretLooking(name, value string) bool {
	return secretName.MatchString(name) || urlPassword.MatchString(value)
}

// Redact returns a copy of obj (decoded JSON) with secret-looking values
// replaced by Placeholder, and the paths it redacted. It hides:
//   - Secret data (the agent never reads Secrets; this is defence in depth);
//   - ConfigMap data under secret-looking keys, and all binaryData;
//   - literal env values under secret-looking names, or with a URL password,
//     in every container list of a Pod, a pod template or a CronJob;
//   - kubectl's last-applied-configuration annotation;
//   - an Argo CD Application's Helm values, parameters and plugin env, and a
//     Flux HelmRelease's values, as a whole: they often carry passwords
//     (review km-39 N-1).
//
// managedFields are dropped: they are noise, not secret.
func Redact(obj map[string]any) (map[string]any, []string) {
	out := deepCopy(obj)
	var paths []string
	if md, ok := out["metadata"].(map[string]any); ok {
		delete(md, "managedFields")
		if ann, ok := md["annotations"].(map[string]any); ok {
			if _, ok := ann[lastApplied]; ok {
				ann[lastApplied] = Placeholder
				paths = append(paths, "metadata.annotations."+lastApplied)
			}
		}
	}
	switch out["kind"] {
	case "Secret":
		for _, f := range []string{"data", "stringData"} {
			if m, ok := out[f].(map[string]any); ok {
				for k := range m {
					m[k] = Placeholder
					paths = append(paths, f+"."+k)
				}
			}
		}
	case "ConfigMap":
		if m, ok := out["data"].(map[string]any); ok {
			for k, v := range m {
				if s, _ := v.(string); secretLooking(k, s) {
					m[k] = Placeholder
					paths = append(paths, "data."+k)
				}
			}
		}
		if m, ok := out["binaryData"].(map[string]any); ok {
			for k := range m {
				m[k] = Placeholder
				paths = append(paths, "binaryData."+k)
			}
		}
	}
	paths = append(paths, redactChartValues(out)...)
	for _, p := range podSpecs(out) {
		spec, _ := dig(out, p).(map[string]any)
		for _, list := range []string{"initContainers", "containers", "ephemeralContainers"} {
			cs, _ := spec[list].([]any)
			for _, c := range cs {
				cm, _ := c.(map[string]any)
				env, _ := cm["env"].([]any)
				for _, e := range env {
					em, _ := e.(map[string]any)
					name, _ := em["name"].(string)
					value, isString := em["value"].(string)
					if isString && secretLooking(name, value) {
						em["value"] = Placeholder
						paths = append(paths, fmt.Sprintf("%s.%s[%v].env[%s]", strings.Join(p, "."), list, cm["name"], name))
					}
				}
			}
		}
	}
	sort.Strings(paths)
	return out, paths
}

// redactChartValues hides the value fields of GitOps objects: an Argo CD
// Application's helm.values, valuesObject, parameters and fileParameters
// and plugin env and parameters, in spec.source and every spec.sources[i],
// and a Flux HelmRelease's spec.values. The whole field goes: its keys are
// arbitrary, so a name-based rule would miss some. An edit that leaves the
// placeholder as it is does not touch the field.
func redactChartValues(out map[string]any) []string {
	var paths []string
	hide := func(m map[string]any, prefix string, fields ...string) {
		for _, f := range fields {
			if _, ok := m[f]; ok {
				m[f] = Placeholder
				paths = append(paths, prefix+f)
			}
		}
	}
	spec, _ := out["spec"].(map[string]any)
	if spec == nil {
		return nil
	}
	switch out["kind"] {
	case "Application":
		source := func(src map[string]any, prefix string) {
			if h, ok := src["helm"].(map[string]any); ok {
				hide(h, prefix+"helm.", "values", "valuesObject", "parameters", "fileParameters")
			}
			if pl, ok := src["plugin"].(map[string]any); ok {
				hide(pl, prefix+"plugin.", "env", "parameters")
			}
		}
		if src, ok := spec["source"].(map[string]any); ok {
			source(src, "spec.source.")
		}
		if srcs, ok := spec["sources"].([]any); ok {
			for i, s := range srcs {
				if src, ok := s.(map[string]any); ok {
					source(src, fmt.Sprintf("spec.sources[%d].", i))
				}
			}
		}
	case "HelmRelease":
		hide(spec, "spec.", "values")
	}
	return paths
}

// podSpecs are where pod specs live in the kinds that have one.
func podSpecs(obj map[string]any) [][]string {
	switch obj["kind"] {
	case "Pod":
		return [][]string{{"spec"}}
	case "CronJob":
		return [][]string{{"spec", "jobTemplate", "spec", "template", "spec"}}
	}
	if dig(obj, []string{"spec", "template", "spec"}) != nil {
		return [][]string{{"spec", "template", "spec"}}
	}
	return nil
}

func dig(obj map[string]any, path []string) any {
	var cur any = obj
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	return cur
}

func deepCopy(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	json.Unmarshal(b, &out)
	return out
}

// View is an object ready to show.
type View struct {
	YAML            string   `json:"yaml"`
	APIVersion      string   `json:"apiVersion"`
	Kind            string   `json:"kind"`
	ResourceVersion string   `json:"resourceVersion"`
	Redacted        []string `json:"redacted"`
	// ArgoApp is the Argo CD application that manages the object: an edit
	// belongs in Git, or Argo CD reverts it.
	ArgoApp string `json:"argoApp,omitempty"`
}

// NewView renders the live object (raw JSON from the agent).
func NewView(raw []byte) (*View, map[string]any, error) {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, nil, fmt.Errorf("object: %w", err)
	}
	red, paths := Redact(obj)
	y, err := yaml.Marshal(red)
	if err != nil {
		return nil, nil, err
	}
	v := &View{YAML: string(y), Redacted: paths}
	v.APIVersion, _ = obj["apiVersion"].(string)
	v.Kind, _ = obj["kind"].(string)
	if md, ok := obj["metadata"].(map[string]any); ok {
		v.ResourceVersion, _ = md["resourceVersion"].(string)
		if l, ok := md["labels"].(map[string]any); ok {
			v.ArgoApp, _ = l["argocd.argoproj.io/instance"].(string)
		}
		if a, ok := md["annotations"].(map[string]any); ok && v.ArgoApp == "" {
			v.ArgoApp, _ = a["argocd.argoproj.io/tracking-id"].(string)
		}
	}
	if v.Redacted == nil {
		v.Redacted = []string{}
	}
	return v, red, nil
}

// Target names the object for the commands.
type Target struct {
	Group, Version, Resource, Namespace, Name string
}

// kubectlResource is "deployments.apps", or "pods" for the core group.
func (t Target) kubectlResource() string {
	if t.Group == "" {
		return t.Resource
	}
	return t.Resource + "." + t.Group
}

// DiffLine is one line of a line diff: Op is " ", "-" or "+".
type DiffLine struct {
	Op   string `json:"op"`
	Text string `json:"text"`
}

// Command is one kubectl command to run.
type Command struct {
	Title   string `json:"title"`
	Command string `json:"command"`
}

// Result is the answer to an edit.
type Result struct {
	Changed   bool       `json:"changed"`
	Diff      []DiffLine `json:"diff"`
	PatchType string     `json:"patchType,omitempty"` // strategic or merge
	Patch     string     `json:"patch,omitempty"`
	Commands  []Command  `json:"commands"`
	Notes     []string   `json:"notes"`
}

// ErrEdit is an edit Kubemap can't turn into a safe patch; the message is
// for the user.
type ErrEdit struct{ Msg string }

func (e *ErrEdit) Error() string { return e.Msg }

func editErr(format string, a ...any) error { return &ErrEdit{fmt.Sprintf(format, a...)} }

// maxLines bounds the line diff (an O(n·m) table).
const maxLines = 4000

// ignoredMeta are server-set fields; an edit to them means nothing.
var ignoredMeta = []string{"managedFields", "resourceVersion", "uid", "creationTimestamp", "generation", "selfLink"}

// normalize drops what an edit can't change: server-set metadata and
// status.
func normalize(obj map[string]any) {
	if md, ok := obj["metadata"].(map[string]any); ok {
		for _, k := range ignoredMeta {
			delete(md, k)
		}
	}
	delete(obj, "status")
}

// Diff compares the edited YAML with the live object and returns the diff,
// the patch and the commands. live is the raw object from the agent.
func Diff(t Target, live []byte, edited string) (*Result, error) {
	view, red, err := NewView(live)
	if err != nil {
		return nil, err
	}
	editedJSON, err := yaml.YAMLToJSON([]byte(edited))
	if err != nil {
		return nil, editErr("the edited text is not valid YAML: %v", err)
	}
	var ed map[string]any
	if err := json.Unmarshal(editedJSON, &ed); err != nil || ed == nil {
		return nil, editErr("the edited text must be one object (a YAML mapping)")
	}
	// It must be the same object.
	name := func(o map[string]any, k string) string { s, _ := dig(o, []string{"metadata", k}).(string); return s }
	for _, k := range []string{"apiVersion", "kind"} {
		if ed[k] != red[k] {
			return nil, editErr("%s changed from %v to %v: that is another object, not an edit", k, red[k], ed[k])
		}
	}
	for _, k := range []string{"name", "namespace"} {
		if name(ed, k) != name(red, k) {
			return nil, editErr("metadata.%s changed from %q to %q: that is another object, not an edit", k, name(red, k), name(ed, k))
		}
	}
	res := &Result{Commands: []Command{}, Notes: []string{}, Diff: []DiffLine{}}
	if !reflect.DeepEqual(ed["status"], red["status"]) && ed["status"] != nil {
		res.Notes = append(res.Notes, "status is written by controllers, not by an edit: the status changes are ignored.")
	}
	before, after := deepCopy(red), deepCopy(ed)
	normalize(before)
	normalize(after)
	if reflect.DeepEqual(before, after) {
		res.Notes = append(res.Notes, "No change: the edited object equals the live one.")
		return res, nil
	}
	res.Changed = true

	by, _ := yaml.Marshal(before)
	ay, _ := yaml.Marshal(after)
	res.Diff, err = lineDiff(string(by), string(ay))
	if err != nil {
		return nil, err
	}

	bj, _ := json.Marshal(before)
	aj, _ := json.Marshal(after)
	gvk := schema.FromAPIVersionAndKind(view.APIVersion, view.Kind)
	var patch []byte
	if typed, err := scheme.Scheme.New(gvk); err == nil {
		// A built-in kind: a strategic merge patch changes list items by
		// their merge key (a container by name), so the patch holds only
		// what changed.
		res.PatchType = "strategic"
		patch, err = strategicpatch.CreateTwoWayMergePatch(bj, aj, typed)
		if err != nil {
			return nil, editErr("cannot build a patch: %v", err)
		}
	} else {
		// A custom resource: a JSON merge patch (lists are replaced whole).
		res.PatchType = "merge"
		patch, err = jsonpatch.CreateMergePatch(bj, aj)
		if err != nil {
			return nil, editErr("cannot build a patch: %v", err)
		}
	}
	// Checked on the decoded patch, not its bytes: JSON may escape the
	// placeholder's < and > (\u003c), and a byte search would miss it.
	var pm map[string]any
	if err := json.Unmarshal(patch, &pm); err != nil {
		return nil, fmt.Errorf("patch: %w", err)
	}
	if holdsPlaceholder(pm) {
		return nil, editErr("the change would write a redacted value (%q) into the object. "+
			"Leave redacted values as they are, or use kubectl edit for that field.", Placeholder)
	}
	// Optimistic lock: the patch applies only to the version shown.
	md, _ := pm["metadata"].(map[string]any)
	if md == nil {
		md = map[string]any{}
		pm["metadata"] = md
	}
	md["resourceVersion"] = view.ResourceVersion
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // readable in the command; the same JSON either way
	enc.Encode(pm)
	res.Patch = strings.TrimSpace(buf.String())

	base := "kubectl"
	if t.Namespace != "" {
		base += " -n " + shellQuote(t.Namespace)
	}
	base += " patch " + shellQuote(t.kubectlResource()) + " " + shellQuote(t.Name) + " --type " + res.PatchType + " -p " + shellQuote(res.Patch)
	res.Commands = append(res.Commands,
		Command{Title: "Dry run on the server (validation and admission, nothing changes)", Command: base + " --dry-run=server -o yaml"},
		Command{Title: "Apply", Command: base},
	)
	res.Notes = append(res.Notes,
		"Run the commands with your own credentials: Kubemap's agent does not apply edits.",
		"The patch holds only the changed fields and resourceVersion "+view.ResourceVersion+": if the object changed since this view, it fails with a conflict. Reload and edit again.")
	if view.ArgoApp != "" {
		res.Notes = append(res.Notes, "Managed by Argo CD ("+view.ArgoApp+"): change it in Git. Argo CD reverts a direct patch at its next sync, or reports the app OutOfSync.")
	}
	if view.Kind == "Pod" {
		res.Notes = append(res.Notes, "Most fields of a running pod can't change (only images, activeDeadlineSeconds, tolerations and labels): the dry run tells. Edit its Deployment or StatefulSet instead.")
	}
	return res, nil
}

// holdsPlaceholder reports whether any key or string in v contains the
// placeholder.
func holdsPlaceholder(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.Contains(x, Placeholder)
	case map[string]any:
		for k, e := range x {
			if strings.Contains(k, Placeholder) || holdsPlaceholder(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if holdsPlaceholder(e) {
				return true
			}
		}
	}
	return false
}

// lineDiff is a longest-common-subsequence line diff.
func lineDiff(a, b string) ([]DiffLine, error) {
	x, y := strings.Split(strings.TrimRight(a, "\n"), "\n"), strings.Split(strings.TrimRight(b, "\n"), "\n")
	if len(x) > maxLines || len(y) > maxLines {
		return nil, editErr("the object is too big for the diff view (more than %d lines)", maxLines)
	}
	n, m := len(x), len(y)
	lcs := make([][]int32, n+1)
	for i := range lcs {
		lcs[i] = make([]int32, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []DiffLine
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case x[i] == y[j]:
			out = append(out, DiffLine{" ", x[i]})
			i, j = i+1, j+1
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, DiffLine{"-", x[i]})
			i++
		default:
			out = append(out, DiffLine{"+", y[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, DiffLine{"-", x[i]})
	}
	for ; j < m; j++ {
		out = append(out, DiffLine{"+", y[j]})
	}
	return out, nil
}

// shellQuote single-quotes s for sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
