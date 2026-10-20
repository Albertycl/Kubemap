#!/usr/bin/env bash
# Test: uninstall.sh (plan mode) lists the objects of an OLD 0.1.x install
# (kubemap-agent-* names) and of the current one. Uses a fake kubectl; touches no cluster.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
cat >"$tmp/kubectl" <<'F'
#!/usr/bin/env bash
# Fake: every object of the old install exists and carries the Kubemap label.
args="$*"
case "$1" in
version) exit 0 ;;
config) echo fake; exit 0 ;;
api-resources) exit 0 ;;
esac
if [ "$1" = get ]; then
	case "$args" in
	*rolebindings,roles*"metadata.name=kubemap-agent-operator"*) printf 'kubemap-system:RoleBinding/kubemap-agent-operator\nkubemap-system:Role/kubemap-agent-operator\n'; exit 0 ;;
	*rolebindings,roles*) exit 0 ;;
	*jsonpath*) echo kubemap; exit 0 ;;
	*"all,secrets"*) echo "deployment.apps/kubemap-agent"; exit 0 ;;
	*) case "$args" in *kubemap-operator*|*kubemap-readonly*|*kubemap-tokenreview*) exit 1 ;; esac; exit 0 ;;
	esac
fi
[ "$1" = delete ] && { echo "would delete ${args}"; exit 0; }
exit 0
F
chmod +x "$tmp/kubectl"
out=$(PATH="$tmp:$PATH" "$here/uninstall.sh" 2>&1); rc=$?
fail=0
want=(
	clusterrole/kubemap-agent-readonly clusterrole/kubemap-agent-tokenreview
	clusterrolebinding/kubemap-agent-readonly clusterrolebinding/kubemap-agent-tokenreview
	clusterrole/kubemap-agent-operator clusterrolebinding/kubemap-agent-operator
	validatingadmissionpolicy.admissionregistration.k8s.io/kubemap-agent-operator-workloads
	validatingadmissionpolicy.admissionregistration.k8s.io/kubemap-agent-operator-protected
	validatingadmissionpolicybinding.admissionregistration.k8s.io/kubemap-agent-operator-nodes
	validatingadmissionpolicybinding.admissionregistration.k8s.io/kubemap-agent-operator-argocd
	namespace/kubemap-system namespace/kubemap-node
	"-n kubemap-system RoleBinding/kubemap-agent-operator" "-n kubemap-system Role/kubemap-agent-operator"
	"deployment.apps/kubemap-agent" "kubemap-data"
)
for w in "${want[@]}"; do
	grep -qF -- "$w" <<<"$out" || { echo "FAIL: output does not list $w"; fail=1; }
done
[ "$rc" = 0 ] || { echo "FAIL: exit status $rc"; fail=1; }
grep -q 'SKIP' <<<"$out" && { echo "FAIL: unexpected SKIP"; fail=1; }
[ "$fail" = 0 ] && echo "PASS uninstall_test.sh"
exit $fail
