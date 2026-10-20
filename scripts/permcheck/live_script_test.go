package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runLiveScript runs verify-agent-permissions.sh against testdata/fake-kubectl.
func runLiveScript(t *testing.T, env ...string) (string, int) {
	t.Helper()
	for _, tool := range []string{"bash", "jq", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " is not installed")
		}
	}
	bin := t.TempDir()
	fake, _ := filepath.Abs("testdata/fake-kubectl")
	if err := os.Symlink(fake, filepath.Join(bin, "kubectl")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "../verify-agent-permissions.sh", "--before-install")
	cmd.Env = append(os.Environ(), append([]string{"PATH=" + bin + ":" + os.Getenv("PATH")}, env...)...)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

const groupRoleBinding = `{"items":[{"metadata":{"namespace":"kubemap-demo","name":"every-sa"},
 "roleRef":{"kind":"Role","name":"pvc-writer"},
 "subjects":[{"kind":"Group","name":"system:serviceaccounts:kubemap-demo"}]}]}`

// review-km-81-fixes B3: a right given to the agent's namespace group must
// fail. The baseline probe lives in a namespace of its own, so it does not
// carry that group, and the binding itself is flagged too.
func TestLiveGrantToNamespaceGroupFails(t *testing.T) {
	rb := filepath.Join(t.TempDir(), "rb.json")
	if err := os.WriteFile(rb, []byte(groupRoleBinding), 0o644); err != nil {
		t.Fatal(err)
	}
	out, code := runLiveScript(t,
		"KUBEMAP_AGENTS=probe kubemap-demo reporter",
		"FAKE_GRANT_GROUP=system:serviceaccounts:kubemap-demo", "FAKE_GRANT_NS=kubemap-demo",
		"FAKE_RB_JSON="+rb)
	for _, want := range []string{
		"RoleBindings for the probe agent (system:serviceaccount:kubemap-demo:reporter, or its groups) in: kubemap-demo",
		"FAIL  probe: RoleBinding kubemap-demo/every-sa -> Role/pvc-writer binds the group system:serviceaccounts:kubemap-demo",
		"FAIL  write verb(s) update on persistentvolumeclaims: not an approved exception",
		"RESULT: FAIL (identities overridden by KUBEMAP_AGENTS)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if code != 1 {
		t.Errorf("exit %d, want 1\n%s", code, out)
	}
}

// The same grant with no RoleBinding to find (for example through a role
// the discovery does not see) is still caught by the --list comparison.
func TestLiveGrantCaughtByListComparison(t *testing.T) {
	out, code := runLiveScript(t,
		"KUBEMAP_AGENTS=probe kubemap-demo reporter",
		"FAKE_GRANT_GROUP=system:serviceaccounts:kubemap-demo", "FAKE_GRANT_NS=kubemap-demo")
	if !strings.Contains(out, "FAIL  write verb(s) update on persistentvolumeclaims") || code != 1 {
		t.Fatalf("exit %d, want the --list FAIL:\n%s", code, out)
	}
}

// Nothing granted: every check passes, and the result says the identities
// were overridden, so a test run is never mistaken for the proof.
func TestLiveNothingGrantedPasses(t *testing.T) {
	out, code := runLiveScript(t, "KUBEMAP_AGENTS=probe kubemap-demo reporter")
	if code != 0 || !strings.Contains(out, "RESULT: PASS (identities overridden by KUBEMAP_AGENTS") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if strings.Contains(out, "\nFAIL") {
		t.Fatalf("unexpected FAIL:\n%s", out)
	}
}
