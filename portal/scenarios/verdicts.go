package scenarios

import (
	"fmt"
	"strings"

	"kubemap/pkg/protocol"
)

// maxDetails bounds the evidence lines of one verdict.
const maxDetails = 8

func capped(d []string) []string {
	if len(d) > maxDetails {
		return append(d[:maxDetails:maxDetails], fmt.Sprintf("and %d more", len(d)-maxDetails))
	}
	return d
}

// serviceVerdict: good when every Service checked is healthy.
func serviceVerdict(r protocol.ServiceCheckResult) Result {
	if len(r.Services) == 0 {
		return Result{Verdict: Unknown, Summary: "No Service to check."}
	}
	var bad, details []string
	for _, s := range r.Services {
		if s.Healthy {
			continue
		}
		bad = append(bad, s.Name)
		for _, f := range s.Findings {
			line := fmt.Sprintf("%s: %s", s.Name, f.Problem)
			if f.Port != "" {
				line += " (port " + f.Port + ")"
			}
			details = append(details, line+". "+f.Detail)
			if f.Fix != "" {
				details = append(details, "Fix: "+f.Fix)
			}
		}
	}
	if len(bad) == 0 {
		if len(r.Services) == 1 {
			s := r.Services[0]
			return Result{Verdict: Good, Summary: fmt.Sprintf("Service %s has %d ready endpoint(s) (%d of %d pods ready).",
				s.Name, s.ReadyEndpoints, s.PodsReady, s.PodsMatched)}
		}
		return Result{Verdict: Good, Summary: fmt.Sprintf("All %d Services have ready endpoints.", len(r.Services))}
	}
	s := r.Services[0] // problems first
	summary := fmt.Sprintf("Service %s: %d ready endpoint(s), %d of %d pods ready.", s.Name, max(s.ReadyEndpoints, 0), s.PodsReady, s.PodsMatched)
	if len(bad) > 1 {
		summary = fmt.Sprintf("%d Services have problems: %s.", len(bad), strings.Join(bad, ", "))
	}
	return Result{Verdict: Bad, Summary: summary, Details: capped(details)}
}

// podDebugVerdict: good when the checklist found no cause.
func podDebugVerdict(d protocol.PodDebug) Result {
	var details []string
	for _, s := range d.Steps {
		if s.Verdict == protocol.VerdictProblem {
			details = append(details, s.Title+": "+s.Finding)
		}
	}
	if d.Cause == "" && len(details) == 0 {
		return Result{Verdict: Good, Summary: fmt.Sprintf("Pod %s is %s; all %d checklist steps are ok.", d.Name, d.Status, len(d.Steps))}
	}
	summary := d.Cause
	if summary == "" {
		summary = fmt.Sprintf("Pod %s (%s) has problems.", d.Name, d.Status)
	}
	return Result{Verdict: Bad, Summary: summary, Details: capped(details)}
}

// diagnoseVerdict: bad on a critical finding, warn on a warning.
func diagnoseVerdict(d protocol.PodDiagnosis) Result {
	verdict := Good
	var details []string
	for _, f := range d.Findings {
		switch f.Severity {
		case "critical":
			verdict = Bad
		case "warning":
			if verdict == Good {
				verdict = Warn
			}
		default:
			continue
		}
		details = append(details, f.Title+": "+f.Detail)
	}
	if verdict == Good {
		return Result{Verdict: Good, Summary: fmt.Sprintf("Pod %s is %s with no problem found.", d.Name, d.Status)}
	}
	return Result{Verdict: verdict, Summary: fmt.Sprintf("Pod %s is %s: %s", d.Name, d.Status, strings.SplitN(details[0], ":", 2)[0]), Details: capped(details)}
}

// netpolVerdict: good when every path is allowed; no path is not a block.
func netpolVerdict(n protocol.NetpolResult) Result {
	var details []string
	for _, p := range n.Paths {
		if !p.Allowed {
			if !p.Egress.Allowed {
				details = append(details, p.To+": egress "+p.Egress.Decision)
			}
			if !p.Ingress.Allowed {
				details = append(details, p.To+": ingress "+p.Ingress.Decision)
			}
		}
	}
	details = append(details, n.Notes...)
	switch {
	case len(n.Paths) == 0:
		return Result{Verdict: Warn, Summary: "No pod is behind the destination, so there is no connection to check. Look at step 2 (endpoints).", Details: capped(n.Notes)}
	case n.Allowed:
		return Result{Verdict: Good, Summary: n.Summary, Details: capped(n.Notes)}
	}
	return Result{Verdict: Bad, Summary: n.Summary, Details: capped(details)}
}

// pressureVerdict: bad when a node is NotReady or under pressure.
func pressureVerdict(r protocol.NodePressureReport) Result {
	if len(r.Nodes) == 0 {
		return Result{Verdict: Unknown, Summary: "No node reported.", Details: r.Notes}
	}
	verdict := Good
	var details []string
	worse := func(v string) {
		rank := map[string]int{Good: 0, Unknown: 1, Warn: 2, Bad: 3}
		if rank[v] > rank[verdict] {
			verdict = v
		}
	}
	for _, n := range r.Nodes {
		switch {
		case !n.Ready:
			worse(Bad)
			details = append(details, n.Node+": NotReady")
		case n.Verdict == protocol.VerdictPressure:
			worse(Bad)
			details = append(details, n.Node+": "+n.Summary)
		case n.Verdict == protocol.VerdictElevated:
			worse(Warn)
			details = append(details, n.Node+": "+n.Summary)
		case n.Verdict == protocol.VerdictUnknown:
			worse(Unknown)
			details = append(details, n.Node+": "+n.Summary)
		}
	}
	details = append(details, r.Notes...)
	switch verdict {
	case Good:
		return Result{Verdict: Good, Summary: fmt.Sprintf("All %d node(s) are Ready with no pressure.", len(r.Nodes))}
	case Bad:
		return Result{Verdict: Bad, Summary: "A node is NotReady or under pressure.", Details: capped(details)}
	case Warn:
		return Result{Verdict: Warn, Summary: "A node shows elevated pressure: workloads wait, but the kubelet is not acting yet.", Details: capped(details)}
	}
	return Result{Verdict: Unknown, Summary: "Pressure could not be judged for every node.", Details: capped(details)}
}

// healthVerdict: bad on a critical finding, warn on a warning.
func healthVerdict(h protocol.HealthReport) Result {
	n := map[string]int{}
	var details []string
	for _, f := range h.Findings {
		n[f.Severity]++
		if f.Severity == protocol.SeverityCritical || f.Severity == protocol.SeverityWarning {
			details = append(details, fmt.Sprintf("%s %s: %s", f.Severity, f.Object, f.Title))
		}
	}
	switch {
	case n[protocol.SeverityCritical] > 0:
		return Result{Verdict: Bad, Summary: fmt.Sprintf("%d critical and %d warning finding(s) from %d rules.", n[protocol.SeverityCritical], n[protocol.SeverityWarning], len(h.Rules)), Details: capped(details)}
	case n[protocol.SeverityWarning] > 0:
		return Result{Verdict: Warn, Summary: fmt.Sprintf("%d warning finding(s) from %d rules.", n[protocol.SeverityWarning], len(h.Rules)), Details: capped(details)}
	}
	return Result{Verdict: Good, Summary: fmt.Sprintf("No critical or warning finding from %d rules.", len(h.Rules))}
}

// appMapVerdict: bad when a route points to a missing Service or to one
// with no ready endpoints (the 503 itself). Other route findings (a
// conflict, plain HTTP without a redirect) do not cause a 503; they are
// listed, but they do not fail the step.
func appMapVerdict(m protocol.AppMap) Result {
	var causes, other []string
	routes := 0
	add := func(c protocol.MapComponent) {
		for _, r := range c.Routes {
			routes++
			for _, p := range r.Problems {
				line := fmt.Sprintf("%s%s → %s: %s", orStar(r.Host), orSlash(r.Path), r.Service, p)
				if causes503(p) {
					causes = append(causes, line)
				} else {
					other = append(other, line)
				}
			}
		}
	}
	for _, app := range m.Apps {
		for _, c := range app.Components {
			add(c)
		}
	}
	for _, c := range m.Unassigned {
		add(c)
	}
	var notes []string
	for _, o := range other {
		notes = append(notes, "Not a 503 cause: "+o)
	}
	switch {
	case routes == 0:
		return Result{Verdict: Unknown, Summary: "No Ingress or IngressRoute found here; the traffic may enter elsewhere."}
	case len(causes) == 0:
		return Result{Verdict: Good, Summary: fmt.Sprintf("All %d route rule(s) point to Services with ready endpoints.", routes), Details: capped(notes)}
	}
	return Result{Verdict: Bad, Summary: fmt.Sprintf("%d route(s) lead nowhere; the first is %s", len(causes), causes[0]),
		Details: capped(append(causes, notes...))}
}

// causes503 tells the route findings that make the ingress answer 503.
func causes503(problem string) bool {
	return strings.HasPrefix(problem, "service missing") || strings.HasPrefix(problem, "no ready endpoints")
}

func orStar(h string) string {
	if h == "" {
		return "*"
	}
	return h
}

func orSlash(p string) string {
	if p == "" {
		return "/"
	}
	return p
}
