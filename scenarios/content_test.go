package scenarios

import (
	"strings"
	"testing"
)

// The pods (KM-84), nodes (KM-85) and batch 2 (KM-88) scenarios keep the bar
// of traffic-503: enough steps, at least one live check, causes that say how
// to prove the fix, prevention and a senior section. The KM-84 pod scenarios
// link a practice lab.
func TestPodAndNodeScenarios(t *testing.T) {
	all, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"pods-crashloop": true, "pods-oom-137": true, "pods-pending": true, "pods-image-pull": true,
		"nodes-notready": true, "nodes-disk-pressure": true, "nodes-eviction-thresholds": true,
		"nodes-swap": true, "nodes-psi": true, "nodes-capacity": true,
		// KM-88
		"traffic-dns-failures": true, "traffic-certificate-expiry": true, "traffic-db-connections": true,
		"traffic-latency-throttling": true, "pods-rollout-stuck": true, "pods-hpa-not-scaling": true,
		"pods-pvc-full": true, "nodes-control-plane-slow": true, "ops-argocd-drift": true,
		"ops-cluster-upgrade": true, "ops-incident-response": true,
	}
	withLab := map[string]bool{"pods-crashloop": true, "pods-oom-137": true, "pods-pending": true, "pods-image-pull": true}
	for _, s := range all {
		if !want[s.ID] {
			continue
		}
		delete(want, s.ID)
		checks := 0
		for _, st := range s.Steps {
			if st.Check != nil {
				checks++
			}
			if st.Good == "" || st.Bad == "" {
				t.Errorf("%s: step %q needs good and bad", s.ID, st.Title)
			}
		}
		t.Logf("%-6s %-26s steps %d, checks %d, causes %d, prevention %d, senior %d, lab %q, params %v",
			s.Area, s.ID, len(s.Steps), checks, len(s.Causes), len(s.Prevention), len(s.Senior), s.Lab, s.Params)
		if len(s.Steps) < 5 || checks == 0 || len(s.Causes) < 3 || len(s.Prevention) < 3 || len(s.Senior) < 3 {
			t.Errorf("%s is thin", s.ID)
		}
		for _, c := range s.Causes {
			if strings.TrimSpace(c.Prove) == "" {
				t.Errorf("%s: cause %q has no command that proves the fix", s.ID, c.Cause)
			}
		}
		if withLab[s.ID] && !strings.HasPrefix(s.Lab, "kubemap-demo/") {
			t.Errorf("%s: pod scenarios link a kubemap-demo lab", s.ID)
		}
	}
	for id := range want {
		t.Errorf("scenario %s is missing", id)
	}
}

// KM-88: the new placeholders are object or host names, checked like ns:
// their values end up in commands people paste into a shell.
func TestNewPlaceholdersValidated(t *testing.T) {
	for _, name := range []string{"deploy", "pvc", "hpa", "cert", "argoapp", "host"} {
		if err := ValidValue(name, "url-shortener"); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		for _, bad := range []string{"x; rm -rf /", "$(id)", "a b", "A_B"} {
			if ValidValue(name, bad) == nil {
				t.Errorf("%s accepted %q", name, bad)
			}
		}
	}
	if err := ValidValue("host", "sre-lab.albertycliu.xyz"); err != nil {
		t.Error(err)
	}
}

// Steps only read (review-km-88 B1): no step command may change the
// cluster. A server dry run saves nothing and is allowed; kubectl exec only
// reads here, but it needs exec rights, so its step must say so (N-1).
// Writes belong in causes (fix), where a person decides.
func TestStepsOnlyRead(t *testing.T) {
	all, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	writes := []string{" run ", " create ", " apply ", " delete ", " patch ", " edit ", " replace ", " scale ", " label ", " annotate ",
		" cordon ", " uncordon ", " taint ", " set ", " debug ", " rollout restart", " rollout undo", " expose ", " autoscale ", " cp "}
	for _, s := range all {
		for i, st := range s.Steps {
			for _, part := range strings.FieldsFunc(st.Command, func(r rune) bool { return r == ';' || r == '|' || r == '&' }) {
				part = " " + strings.TrimSpace(strings.SplitN(part, "#", 2)[0]) + " "
				if !strings.Contains(part, "kubectl ") || strings.Contains(part, " auth can-i ") { // can-i only asks
					continue
				}
				for _, w := range writes {
					if strings.Contains(part, w) {
						t.Errorf("%s step %d changes the cluster (%q): %s", s.ID, i+1, strings.TrimSpace(w), part)
					}
				}
				if strings.Contains(part, " drain ") && !strings.Contains(part, "--dry-run=server") {
					t.Errorf("%s step %d drains without --dry-run=server", s.ID, i+1)
				}
				if strings.Contains(part, " exec ") && !strings.Contains(st.Why, "exec rights") {
					t.Errorf("%s step %d uses kubectl exec without saying it needs exec rights", s.ID, i+1)
				}
			}
		}
	}
}

// DNS uses UDP 53 (TCP for large answers): the DNS scenario checks both, so
// a UDP-only policy is not a false alarm and a TCP-only one is not missed
// (review-km-88-fixes N-1).
func TestDNSChecksBothProtocols(t *testing.T) {
	all, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range all {
		if s.ID != "traffic-dns-failures" {
			continue
		}
		for _, st := range s.Steps {
			if st.Check == nil || st.Check.Op != "netpol.check" {
				continue
			}
			req, err := st.Check.Request(map[string]string{"ns": "shop", "pod": "web-1"})
			if err != nil || req.NetpolCheck == nil || req.NetpolCheck.Port != 53 {
				t.Fatalf("request %+v: %v", req, err)
			}
			got = append(got, string(req.NetpolCheck.Protocol))
		}
	}
	if strings.Join(got, ",") != "UDP,TCP" {
		t.Errorf("DNS checks use %v, want UDP then TCP", got)
	}
}
