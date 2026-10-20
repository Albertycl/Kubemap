// Package scenarios holds the SRE scenarios the portal walks you through
// (KM-82): a real question ("Users get HTTP 503"), the steps a senior SRE
// checks in order, each with why, the command, the Kubemap page to open and
// an optional read-only live check, then the usual causes, prevention and
// what a senior would say.
//
// The files are scenarios/<area>/<id>.yaml, embedded into the portal. Load
// decodes them strictly and validates them; TestEmbeddedScenarios runs it,
// so a bad file fails "make all".
package scenarios

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"

	"kubemap/pkg/protocol"
)

//go:embed */*.yaml
var files embed.FS

// Areas a scenario belongs to.
var Areas = []string{"traffic", "pods", "nodes", "security", "operations"}

// Pages a step can open (web routes).
var Pages = []string{"app-map", "pod", "node", "netpol", "logs", "findings", "debug"}

// Scenario is one file.
type Scenario struct {
	ID         string   `json:"id"`
	Area       string   `json:"area"`
	Title      string   `json:"title"`
	Question   string   `json:"question"`
	Symptoms   []string `json:"symptoms,omitempty"`
	Steps      []Step   `json:"steps"`
	Causes     []Cause  `json:"causes,omitempty"`
	Prevention []string `json:"prevention,omitempty"`
	Senior     []string `json:"senior,omitempty"`
	Lab        string   `json:"lab,omitempty"`
	// Params are the placeholders the scenario uses ("ns", "svc"), in order
	// of first use; filled in when it is started from a real object.
	Params []string `json:"params"`
}

// Step is one thing to check.
type Step struct {
	Title   string `json:"title"`
	Why     string `json:"why"`
	Command string `json:"command"`
	Kubemap string `json:"kubemap,omitempty"`
	Check   *Check `json:"check,omitempty"`
	Good    string `json:"good,omitempty"`
	Bad     string `json:"bad,omitempty"`
}

// Check is a read-only live check: an agent request op with arguments.
type Check struct {
	Op   string            `json:"op"`
	Args map[string]string `json:"args,omitempty"`
}

// Cause is a usual cause with its fix and the command that proves the fix.
type Cause struct {
	Cause string `json:"cause"`
	Fix   string `json:"fix"`
	Prove string `json:"prove"`
}

// Placeholders a scenario may use: "$ns" in check args, "<ns>" in commands.
var Placeholders = map[string]string{
	"ns":        "namespace",
	"svc":       "Service",
	"pod":       "pod",
	"container": "container",
	"node":      "node",
	"app":       "app (an app map ID)",
	"port":      "port",
	"from":      "source pod",
	"fromns":    "source namespace",
	"deploy":    "Deployment",
	"pvc":       "PersistentVolumeClaim",
	"hpa":       "HorizontalPodAutoscaler",
	"cert":      "cert-manager Certificate",
	"argoapp":   "Argo CD Application",
	"host":      "host name (as in the URL)",
}

// opSpec says which arguments a check op takes. Only these ops may be used:
// every one only reads (the agent's list, get and watch, no writes).
type opSpec struct {
	required, optional []string
}

// AllowedOps is the read-only allow list.
var AllowedOps = map[string]opSpec{
	string(protocol.OpServiceCheck): {optional: []string{"namespace", "service"}},
	string(protocol.OpPodDebug):     {required: []string{"namespace", "pod"}},
	string(protocol.OpPodDiagnose):  {required: []string{"namespace", "pod"}},
	string(protocol.OpNodePressure): {optional: []string{"node"}},
	string(protocol.OpHealthCheck):  {optional: []string{"namespace"}},
	string(protocol.OpAppMap):       {optional: []string{"namespace", "app"}},
	string(protocol.OpNetpolCheck): {required: []string{"fromNamespace", "fromPod", "toNamespace", "port"},
		optional: []string{"toService", "toPod", "protocol"}},
}

var (
	idPattern    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	argRef       = regexp.MustCompile(`^\$([a-z]+)$`)
	commandRef   = regexp.MustCompile(`<([a-z]+)>`)
	labPattern   = regexp.MustCompile(`^[a-z0-9-]+/[a-z0-9-]+$`)
	literalValue = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,253}$`)
)

// Load reads and validates every embedded scenario, sorted by area order,
// then ID.
func Load() ([]Scenario, error) { return load(files) }

func load(fsys fs.FS) ([]Scenario, error) {
	paths, err := fs.Glob(fsys, "*/*.yaml")
	if err != nil {
		return nil, err
	}
	var out []Scenario
	ids := map[string]string{}
	for _, p := range paths {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, err
		}
		s, err := Parse(b)
		if err != nil {
			return nil, fmt.Errorf("scenarios/%s: %w", p, err)
		}
		if want := strings.TrimSuffix(path.Base(p), ".yaml"); s.ID != want {
			return nil, fmt.Errorf("scenarios/%s: id %q must match the file name %q", p, s.ID, want)
		}
		if dir := path.Dir(p); s.Area != dir {
			return nil, fmt.Errorf("scenarios/%s: area %q must match the folder %q", p, s.Area, dir)
		}
		if other, dup := ids[s.ID]; dup {
			return nil, fmt.Errorf("scenarios/%s: id %q is also used by %s", p, s.ID, other)
		}
		ids[s.ID] = p
		out = append(out, s)
	}
	areaRank := map[string]int{}
	for i, a := range Areas {
		areaRank[a] = i
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Area != out[j].Area {
			return areaRank[out[i].Area] < areaRank[out[j].Area]
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Parse decodes one scenario strictly (an unknown field is an error) and
// validates it.
func Parse(b []byte) (Scenario, error) {
	j, err := yaml.YAMLToJSON(b)
	if err != nil {
		return Scenario{}, fmt.Errorf("not valid YAML: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.DisallowUnknownFields()
	var s Scenario
	if err := dec.Decode(&s); err != nil {
		return Scenario{}, fmt.Errorf("does not match the scenario format: %w", err)
	}
	if s.Params != nil {
		return Scenario{}, fmt.Errorf("params is computed from the placeholders; do not set it")
	}
	if err := s.validate(); err != nil {
		return Scenario{}, err
	}
	return s, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (s *Scenario) validate() error {
	switch {
	case !idPattern.MatchString(s.ID):
		return fmt.Errorf("id %q must be lowercase words joined by '-'", s.ID)
	case !contains(Areas, s.Area):
		return fmt.Errorf("area %q must be one of %s", s.Area, strings.Join(Areas, ", "))
	case strings.TrimSpace(s.Title) == "":
		return fmt.Errorf("title is required")
	case strings.TrimSpace(s.Question) == "":
		return fmt.Errorf("question is required")
	case len(s.Steps) == 0:
		return fmt.Errorf("steps: at least one step is required")
	case s.Lab != "" && !labPattern.MatchString(s.Lab):
		return fmt.Errorf("lab %q must look like namespace/scenario", s.Lab)
	}
	seen := map[string]bool{}
	use := func(name, where string) error {
		if _, ok := Placeholders[name]; !ok {
			return fmt.Errorf("%s: unknown placeholder %q (known: %s)", where, name, strings.Join(placeholderNames(), ", "))
		}
		if !seen[name] {
			seen[name] = true
			s.Params = append(s.Params, name)
		}
		return nil
	}
	for i, st := range s.Steps {
		where := fmt.Sprintf("steps[%d]", i)
		switch {
		case strings.TrimSpace(st.Title) == "":
			return fmt.Errorf("%s: title is required", where)
		case strings.TrimSpace(st.Why) == "":
			return fmt.Errorf("%s (%s): why is required", where, st.Title)
		case strings.TrimSpace(st.Command) == "":
			return fmt.Errorf("%s (%s): command is required", where, st.Title)
		case st.Kubemap != "" && !contains(Pages, st.Kubemap):
			return fmt.Errorf("%s (%s): kubemap %q must be one of %s", where, st.Title, st.Kubemap, strings.Join(Pages, ", "))
		}
		for _, m := range commandRef.FindAllStringSubmatch(st.Command, -1) {
			if err := use(m[1], where+".command"); err != nil {
				return err
			}
		}
		if st.Check != nil {
			if err := st.Check.validate(where+".check", use); err != nil {
				return err
			}
		}
	}
	for i, c := range s.Causes {
		if strings.TrimSpace(c.Cause) == "" || strings.TrimSpace(c.Fix) == "" {
			return fmt.Errorf("causes[%d]: cause and fix are required", i)
		}
	}
	if s.Params == nil {
		s.Params = []string{}
	}
	return nil
}

func placeholderNames() []string {
	var n []string
	for k := range Placeholders {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

func (c *Check) validate(where string, use func(name, where string) error) error {
	spec, ok := AllowedOps[c.Op]
	if !ok {
		var allowed []string
		for op := range AllowedOps {
			allowed = append(allowed, op)
		}
		sort.Strings(allowed)
		return fmt.Errorf("%s: op %q is not on the read-only allow list (%s)", where, c.Op, strings.Join(allowed, ", "))
	}
	for _, k := range spec.required {
		if c.Args[k] == "" {
			return fmt.Errorf("%s: op %s needs argument %q", where, c.Op, k)
		}
	}
	for k := range c.Args {
		if !contains(spec.required, k) && !contains(spec.optional, k) {
			return fmt.Errorf("%s: op %s has no argument %q", where, c.Op, k)
		}
	}
	// In the op's own argument order, so Params (and the page's fields)
	// come out the same every time, not in map order.
	for _, k := range append(append([]string{}, spec.required...), spec.optional...) {
		v, ok := c.Args[k]
		if !ok {
			continue
		}
		if m := argRef.FindStringSubmatch(v); m != nil {
			if err := use(m[1], where+".args."+k); err != nil {
				return err
			}
		} else if !literalValue.MatchString(v) {
			return fmt.Errorf("%s: argument %s=%q must be a placeholder like $ns or a plain value", where, k, v)
		}
	}
	if c.Op == string(protocol.OpNetpolCheck) && (c.Args["toService"] == "") == (c.Args["toPod"] == "") {
		return fmt.Errorf("%s: netpol.check needs exactly one of toService and toPod", where)
	}
	return nil
}

// Value rules per placeholder. A command with a filled-in value is shown in
// a Copy box for a person to paste into a shell with cluster-admin rights,
// so a value is used only if it is a plain name or number of its kind:
// never ";", "|", "$(", quotes, spaces or newlines (review KM-82 B1).
var (
	dnsName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	appID   = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?/[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
)

// ValidValue checks a placeholder value by its kind: names are DNS names
// (at most 253 characters), app is "namespace/name", port is 1..65535.
func ValidValue(name, v string) error {
	switch {
	case name == "port":
		if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != v {
			return fmt.Errorf("%q is not a port (1 to 65535)", v)
		}
	case name == "app":
		if len(v) > 253 || !appID.MatchString(v) {
			return fmt.Errorf("%q is not an app ID (namespace/name)", v)
		}
	default:
		if _, known := Placeholders[name]; !known {
			return fmt.Errorf("unknown placeholder %q", name)
		}
		if len(v) > 253 || !dnsName.MatchString(v) {
			return fmt.Errorf("%q is not a valid %s name", v, Placeholders[name])
		}
	}
	return nil
}

// Fill replaces "<name>" in a command with the values given. A placeholder
// without a value, with an invalid one, or that is not a known placeholder
// stays as it is, for the reader to fill in.
func Fill(command string, values map[string]string) string {
	return commandRef.ReplaceAllStringFunc(command, func(m string) string {
		name := m[1 : len(m)-1]
		if v := values[name]; v != "" && ValidValue(name, v) == nil {
			return v
		}
		return m
	})
}

// ErrMissing is returned by Request when a placeholder has no value.
type ErrMissing struct{ Names []string }

func (e *ErrMissing) Error() string {
	return "needs " + strings.Join(e.Names, ", ")
}

// Request builds the agent request for a check, with its placeholders
// filled in. It returns *ErrMissing when a value is missing.
func (c *Check) Request(values map[string]string) (*protocol.Request, error) {
	args := map[string]string{}
	var missing []string
	for k, v := range c.Args {
		if m := argRef.FindStringSubmatch(v); m != nil {
			v = values[m[1]]
			if v == "" {
				missing = append(missing, m[1])
				continue
			}
			if err := ValidValue(m[1], v); err != nil {
				return nil, err
			}
		}
		args[k] = v
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, &ErrMissing{Names: missing}
	}
	req := &protocol.Request{Op: protocol.Op(c.Op)}
	switch protocol.Op(c.Op) {
	case protocol.OpServiceCheck:
		req.Namespace, req.Name = args["namespace"], args["service"]
	case protocol.OpPodDebug, protocol.OpPodDiagnose:
		req.Namespace, req.Name = args["namespace"], args["pod"]
	case protocol.OpNodePressure:
		req.Name = args["node"]
	case protocol.OpHealthCheck:
		req.Namespace = args["namespace"]
	case protocol.OpAppMap:
		req.Namespace = args["namespace"]
		if args["app"] != "" {
			req.AppMap = &protocol.AppMapOptions{App: args["app"]}
		}
	case protocol.OpNetpolCheck:
		port, err := strconv.Atoi(args["port"])
		if err != nil {
			return nil, fmt.Errorf("port %q is not a number", args["port"])
		}
		req.NetpolCheck = &protocol.NetpolCheckRequest{
			From:     protocol.NetpolEndpoint{Namespace: args["fromNamespace"], Pod: args["fromPod"]},
			To:       protocol.NetpolEndpoint{Namespace: args["toNamespace"], Service: args["toService"], Pod: args["toPod"]},
			Port:     int32(port),
			Protocol: args["protocol"],
		}
	}
	if err := req.Validate(); err != nil {
		return nil, protocol.AsError(err)
	}
	return req, nil
}
