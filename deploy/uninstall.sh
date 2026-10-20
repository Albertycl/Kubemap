#!/usr/bin/env bash
# Remove Kubemap from a cluster, and prove nothing is left (KM-81).
#
#   deploy/uninstall.sh            show what would be deleted (server dry-run); changes nothing
#   deploy/uninstall.sh --yes      delete it, then check that nothing is left
#   deploy/uninstall.sh --check    only check that nothing is left
#
# What the install manifest creates, and so what this removes:
#   - the namespaces kubemap-system and kubemap-node, with everything in them
#     (Deployment, DaemonSet, Service, PVC with the database, ServiceAccounts, IngressRoutes, Certificate, NetworkPolicies);
#   - the ClusterRoles and ClusterRoleBindings kubemap-readonly and
#     kubemap-tokenreview;
# Kubemap is read-only by design and the manifest holds nothing else. An old
# 0.1.x install (the agent-only one) used other names, and these are removed
# too: the ClusterRoles and ClusterRoleBindings kubemap-agent-readonly and
# kubemap-agent-tokenreview, and the namespaces with the Deployment
# kubemap-agent, its Secret, Role and RoleBinding. If that version ran in
# operator mode, the leftovers go first (RBAC, then the guards): the
# ClusterRole and ClusterRoleBinding kubemap-operator or
# kubemap-agent-operator, the Role and RoleBinding of those names in any
# namespace, and the ValidatingAdmissionPolicies and bindings
# kubemap-operator-* and kubemap-agent-operator-*.
# An object is deleted only if it has one of these names AND the label
# app.kubernetes.io/managed-by=kubemap. Same-named objects without the label
# are left alone and reported.
#
# Works with the kubeconfig for any cluster (--context NAME to pick one).
# Exit status: 0 = nothing of Kubemap is left (or the dry run passed),
# 1 = something is left or could not be deleted, 2 = could not run.
set -uo pipefail

LABEL=app.kubernetes.io/managed-by=kubemap
NAMESPACES=(kubemap-system kubemap-node)
# The operator names of the current naming and of the old 0.1.x install.
OPERATORS=(kubemap-operator kubemap-agent-operator)
VAP=admissionregistration.k8s.io
# Deleted in this order: leftover operator RBAC, leftover guards, then the rest.
CLUSTER_OBJECTS=()
for op in "${OPERATORS[@]}"; do
	CLUSTER_OBJECTS+=(clusterrolebinding/$op clusterrole/$op)
done
for kind in validatingadmissionpolicybinding validatingadmissionpolicy; do
	for op in "${OPERATORS[@]}"; do
		for part in workloads argocd nodes protected; do
			CLUSTER_OBJECTS+=("$kind.$VAP/$op-$part")
		done
	done
done
for base in kubemap kubemap-agent; do
	CLUSTER_OBJECTS+=(clusterrolebinding/$base-readonly clusterrolebinding/$base-tokenreview)
done
for base in kubemap kubemap-agent; do
	CLUSTER_OBJECTS+=(clusterrole/$base-readonly clusterrole/$base-tokenreview)
done
KUBECTL=(kubectl)
mode=plan

while [ $# -gt 0 ]; do
	case "$1" in
	--yes) mode=delete ;;
	--check) mode=check ;;
	--context) KUBECTL+=(--context "${2:?--context needs a name}"); shift ;;
	*) sed -n '2,25p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
	esac
	shift
done
command -v kubectl >/dev/null || { echo "uninstall: kubectl is needed" >&2; exit 2; }
"${KUBECTL[@]}" version >/dev/null 2>&1 || { echo "uninstall: cannot reach the cluster" >&2; exit 2; }
echo "Cluster: $("${KUBECTL[@]}" config current-context 2>/dev/null)"

# An object is "kind/name", or "namespace:kind/name" for a namespaced one;
# split sets OBJ to its kubectl arguments.
split() { if [[ $1 == *:* ]]; then OBJ=(-n "${1%%:*}" "${1#*:}"); else OBJ=("$1"); fi; }
# ours <object>: exists and carries the Kubemap label.
ours() { split "$1"; [ "$("${KUBECTL[@]}" get "${OBJ[@]}" -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}' 2>/dev/null)" = kubemap ]; }
exists() { split "$1"; "${KUBECTL[@]}" get "${OBJ[@]}" >/dev/null 2>&1; }

# Leftover namespaced operator mode of an older version: a RoleBinding and Role
# named kubemap-operator or kubemap-agent-operator in any namespace (bindings
# first, as kubectl lists them).
OPERATOR_NS_OBJECTS=()
for op in "${OPERATORS[@]}"; do
	mapfile -t found < <("${KUBECTL[@]}" get rolebindings,roles -A -l "$LABEL" \
		--field-selector "metadata.name=$op" \
		-o jsonpath='{range .items[*]}{.metadata.namespace}:{.kind}/{.metadata.name}{"\n"}{end}' 2>/dev/null)
	for f in "${found[@]}"; do [ -n "$f" ] && OPERATOR_NS_OBJECTS+=("$f"); done
done

targets=()
for o in "${OPERATOR_NS_OBJECTS[@]}" "${CLUSTER_OBJECTS[@]}" "${NAMESPACES[@]/#/namespace/}"; do
	if ! exists "$o"; then
		continue
	elif ours "$o"; then
		targets+=("$o")
	else
		echo "SKIP  $o exists but has no label $LABEL: not created by Kubemap, left alone"
	fi
done

if [ "$mode" = plan ] || [ "$mode" = delete ]; then
	if [ ${#targets[@]} -eq 0 ]; then
		echo "Nothing of Kubemap to delete."
	else
		for ns in "${NAMESPACES[@]}"; do
			if printf '%s\n' "${targets[@]}" | grep -qx "namespace/$ns"; then
				echo "In namespace $ns (deleted with it):"
				# Table output (names only): -o name would download whole
				# objects, the server's own objects included.
				"${KUBECTL[@]}" get all,secrets,configmaps,serviceaccounts,roles,rolebindings -n "$ns" --no-headers 2>/dev/null | awk '{print "        " $1}'
			fi
		done
	fi
fi

# The namespaces hold the PVC with the SQLite database (users, audit log,
# incident journal). Deleting them deletes it, with no way back.
warn_data() {
	printf '%s\n' "${targets[@]}" | grep -q '^namespace/' || return 0
	echo
	echo "WARNING: this deletes the namespaces kubemap-system and kubemap-node, including the"
	echo "         PersistentVolumeClaim kubemap-data with the Kubemap database (users, audit log,"
	echo "         incident journal). The data cannot be recovered. Use this only to remove Kubemap"
	echo "         for good or an OLD 0.1.x install, never to upgrade 0.2.0 (upgrade with install.sh)."
}

case "$mode" in
plan)
	[ ${#targets[@]} -eq 0 ] && exit 0
	warn_data
	echo
	echo "Dry run on the API server (nothing is deleted):"
	rc=0
	for o in "${targets[@]}"; do
		split "$o"
		"${KUBECTL[@]}" delete "${OBJ[@]}" --dry-run=server 2>&1 | sed 's/^/  /' || rc=1
	done
	echo
	echo "Run again with --yes to delete."
	exit $rc
	;;
delete)
	warn_data
	for o in "${targets[@]}"; do
		# Namespaces last (the loop order above), and wait until they are gone.
		split "$o"
		"${KUBECTL[@]}" delete "${OBJ[@]}" --wait=true --timeout=180s 2>&1 | sed 's/^/  /'
	done
	;;
esac

# The proof: nothing of Kubemap is left.
echo
echo "== Is anything of Kubemap left?"
left=0
for o in "${CLUSTER_OBJECTS[@]}" "${NAMESPACES[@]/#/namespace/}"; do
	if exists "$o" && ours "$o"; then
		echo "LEFT  $o"
		left=1
	fi
done
# Any object with the Kubemap label, of any kind, anywhere. Each kind is
# asked on its own: one kubectl call for all kinds returns nothing at all
# when a single kind cannot be listed, which would look like "nothing left".
unlisted=()
while read -r kind scoped; do
	[ -n "$kind" ] || continue
	if [ "$scoped" = true ]; then
		out=$("${KUBECTL[@]}" get "$kind" -A -l "$LABEL" --no-headers --ignore-not-found 2>/dev/null) || { unlisted+=("$kind"); continue; }
		out=$(awk -v k="$kind" 'NF {print k " " $1 "/" $2}' <<<"$out")
	else
		out=$("${KUBECTL[@]}" get "$kind" -l "$LABEL" --no-headers --ignore-not-found 2>/dev/null) || { unlisted+=("$kind"); continue; }
		out=$(awk -v k="$kind" 'NF {print k " " $1}' <<<"$out")
	fi
	if [ -n "$out" ]; then
		sed 's/^/LEFT  /' <<<"$out"
		left=1
	fi
done < <(
	for scoped in true false; do
		"${KUBECTL[@]}" api-resources --verbs=list --namespaced=$scoped -o name 2>/dev/null |
			grep -vE '^(events|events\.events\.k8s\.io|.*\.metrics\.k8s\.io)$' | sed "s/\$/ $scoped/"
	done
)
# Discovery itself can be partial: with an API service down, api-resources
# lists the rest and exits non-zero. The missing groups' kinds were never
# asked, so the sweep is incomplete.
if ! derr=$("${KUBECTL[@]}" api-resources --verbs=list -o name 2>&1 >/dev/null); then
	echo "NOTE  API discovery is incomplete: $(tr '\n' ' ' <<<"$derr")"
	unlisted+=("(kinds of the API groups above)")
fi
if [ ${#unlisted[@]} -gt 0 ]; then
	echo "NOTE  could not list: ${unlisted[*]} (no rights, or an API service is down)"
	echo "FAIL  the check is incomplete: objects of those kinds were not checked."
	exit 1
fi
if [ "$left" = 0 ]; then
	echo "PASS  no namespace, ClusterRole, ClusterRoleBinding, guard policy or other object labelled $LABEL is left."
	exit 0
fi
echo "FAIL  something is left (see above)."
exit 1
