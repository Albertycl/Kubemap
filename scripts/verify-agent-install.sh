#!/usr/bin/env bash
# Checks around installing the Kubemap agents (KM-81). Works with the
# kubeconfig for any cluster.
#
#   scripts/verify-agent-install.sh pre deploy/kubemap.yaml
#       Before the install. Changes nothing:
#       - every workload in the file must meet the pod security settings
#         (non-root, read-only root filesystem, no privilege escalation, all
#         capabilities dropped, not privileged, a memory limit, host paths
#         read-only); checked offline with jq;
#       - the RBAC may only read, apart from the two approved exceptions;
#       - "kubectl apply --dry-run=server": the API server runs its admission
#         checks (Pod Security, webhooks, quotas) without saving anything.
#         Kubernetes cannot dry-run objects in a namespace that does not exist
#         yet, so for those it runs a client dry-run with strict schema
#         validation instead, and says so.
#
#   scripts/verify-agent-install.sh snapshot FILE
#       Before the install. Counts the objects of every kind outside the
#       kubemap namespaces and writes the counts to FILE. Changes nothing.
#
#   scripts/verify-agent-install.sh post FILE
#       After the install. Checks the running agent pods' security settings
#       and memory limits, lists every object Kubemap owns, and compares the
#       counts outside the kubemap namespaces with the snapshot FILE.
#       Changes nothing.
#
#   --context NAME (before the subcommand) picks the kubeconfig context.
#
# Exit status: 0 = passed, 1 = a check failed, 2 = could not run.
set -uo pipefail

KUBECTL=(kubectl)
if [ "${1:-}" = --context ]; then
	[ -n "${2:-}" ] || { echo "verify: --context needs a name" >&2; exit 2; }
	KUBECTL+=(--context "$2")
	shift 2
fi

NAMESPACES=(kubemap-system kubemap-node)
# Kinds that change all the time on their own: left out of the comparison.
CHURN='^(events|events\.events\.k8s\.io|leases\.coordination\.k8s\.io|endpoints|endpointslices\.discovery\.k8s\.io|pods|replicasets\.apps|jobs\.batch|controllerrevisions\.apps|nodes\.metrics\.k8s\.io|pods\.metrics\.k8s\.io|tokenreviews.*|.*reviews\..*)$'

fails=0
pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; fails=$((fails + 1)); }
note() { echo "NOTE  $*"; }
die() { echo "verify: $*" >&2; exit 2; }
finish() {
	echo
	if [ "$fails" -gt 0 ]; then echo "RESULT: FAIL ($fails)"; exit 1; fi
	echo "RESULT: PASS"
	exit 0
}

command -v kubectl >/dev/null || die "kubectl is needed"
command -v jq >/dev/null || die "jq is needed"

# podsec <json-list>: the pod security checks for every pod template.
podsec() {
	jq -r '
	  def podspec: if .kind == "Pod" then .spec else .spec.template.spec end;
	  .items[] | select(.kind | IN("Pod","Deployment","DaemonSet","StatefulSet","ReplicaSet","Job","CronJob")) |
	  (if .kind == "CronJob" then .spec.jobTemplate.spec.template.spec else podspec end) as $p |
	  "\(.kind)/\(.metadata.namespace)/\(.metadata.name)" as $w |
	  ($p.containers + ($p.initContainers // []))[] as $c |
	  (($p.securityContext // {}) as $ps | ($c.securityContext // {}) as $cs |
	   [
	     (if (($cs.runAsNonRoot // $ps.runAsNonRoot) == true) then empty else "runAsNonRoot is not true" end),
	     (if (($cs.runAsUser // $ps.runAsUser // 1) == 0) then "runs as UID 0" else empty end),
	     (if $cs.readOnlyRootFilesystem == true then empty else "readOnlyRootFilesystem is not true" end),
	     (if $cs.allowPrivilegeEscalation == false then empty else "allowPrivilegeEscalation is not false" end),
	     (if ($cs.privileged // false) then "privileged" else empty end),
	     (if ($cs.capabilities.drop // []) | index("ALL") then empty else "capabilities do not drop ALL" end),
	     (if (($cs.capabilities.add // []) | length) > 0 then "adds capabilities \($cs.capabilities.add)" else empty end),
	     (if $c.resources.limits.memory then empty else "no memory limit" end),
	     ($p.volumes // [] | map(select(.hostPath)) | map(.name)) as $hp |
	       ($c.volumeMounts // [] | map(select(.name as $n | $hp | index($n)) | select(.readOnly != true)) | map("host path \(.name) mounted writable") | .[])
	   ] as $bad |
	   if ($bad | length) == 0 then "PASS  \($w) container \($c.name): non-root, read-only root, no escalation, no capabilities, memory limit \($c.resources.limits.memory)"
	   else "FAIL  \($w) container \($c.name): \($bad | join("; "))" end)'
}

# rbac_audit <json-list>: every rule may only read, except the approved ones.
rbac_audit() {
	jq -r '
	  .items[] | select(.kind == "ClusterRole" or .kind == "Role") as $r |
	  ($r.rules // [])[] as $x |
	  "\($r.kind) \($r.metadata.namespace // "")/\($r.metadata.name): \($x.verbs | join(",")) on \($x.resources | join(","))\(if $x.resourceNames then " names=" + ($x.resourceNames | join(",")) else "" end)" as $d |
	  if ([$x.verbs[], $x.resources[], $x.apiGroups[]] | index("*")) then "FAIL  \($d): wildcard"
	  elif ([$x.resources[] | select(contains("/")) | select(. != "pods/log")] | length) > 0 then "FAIL  \($d): subresource not approved (only pods/log)"
	  elif ($x.resources | index("secrets")) then
	    "FAIL  \($d): Secret access"
	  elif ($x.verbs - ["get","list","watch"] | length) == 0 then "PASS  \($d): read-only"
	  elif $x.apiGroups == ["authentication.k8s.io"] and $x.resources == ["tokenreviews"] and $x.verbs == ["create"] and $r.kind == "ClusterRole" then "PASS  \($d): approved exception (TokenReview, stores nothing)"
	  else "FAIL  \($d): write verb" end'
}

# count prints "kind count" for every listable kind. Left out: objects in the
# kubemap namespaces, and the Kubemap objects outside them (the two
# namespaces, the two ClusterRoles and their bindings). So after an install
# every count must be exactly what it was before.
count() {
	# The default table output is printed by the server and carries names
	# only, never object contents (no Secret data leaves the cluster).
	local scoped kind lines derr
	# Partial discovery (an API service down) must not look like "no such
	# kinds": it is recorded, and snapshot and post report it.
	if ! derr=$("${KUBECTL[@]}" api-resources --verbs=list -o name 2>&1 >/dev/null); then
		echo "__discovery__ error $(tr '\n' ' ' <<<"$derr" | tr -s ' ' | cut -c1-300)"
	fi
	for scoped in true false; do
		"${KUBECTL[@]}" api-resources --verbs=list --namespaced=$scoped -o name 2>/dev/null | sort | while read -r kind; do
			[[ $kind =~ $CHURN ]] && continue
			if [ "$scoped" = true ]; then
				lines=$("${KUBECTL[@]}" get "$kind" -A --no-headers --ignore-not-found 2>/dev/null) || { echo "$kind error"; continue; }
				lines=$(awk '{print $1 "/" $2}' <<<"$lines")
			else
				lines=$("${KUBECTL[@]}" get "$kind" --no-headers --ignore-not-found 2>/dev/null) || { echo "$kind error"; continue; }
				lines=$(awk '{print "/" $1}' <<<"$lines")
			fi
			n=$(grep -vE '^kubemap-(system|node)/' <<<"$lines" | grep -vxE "$(own_pattern "$kind")" | grep -c .)
			echo "$kind $n"
		done
	done | sort
}

own_pattern() { # the Kubemap objects of a cluster-scoped kind, as "/name" lines
	case "$1" in
	namespaces) echo '/kubemap-(system|node)' ;;
	clusterroles.rbac.authorization.k8s.io | clusterrolebindings.rbac.authorization.k8s.io) echo '/kubemap-(readonly|tokenreview)' ;;
	*) echo '^$' ;;
	esac
}

cmd=${1:-}
case "$cmd" in
pre)
	file=${2:-}
	[ -f "$file" ] || die "usage: $0 pre deploy/kubemap.yaml"
	json=$("${KUBECTL[@]}" apply --dry-run=client -o json -f "$file" 2>&1) || die "the file does not parse: $json"
	# A single object comes back without a List around it.
	json=$(jq 'if .kind == "List" then . else {items: [.]} end' <<<"$json")
	echo "== Pod security of every workload (offline)"
	out=$(podsec <<<"$json")
	[ -n "$out" ] || note "no workloads in the file"
	while read -r line; do [ -n "$line" ] || continue; echo "$line"; [[ $line == FAIL* ]] && fails=$((fails + 1)); done <<<"$out"
	echo
	echo "== RBAC rules (offline)"
	out=$(rbac_audit <<<"$json")
	while read -r line; do [ -n "$line" ] || continue; echo "$line"; [[ $line == FAIL* ]] && fails=$((fails + 1)); done <<<"$out"
	bad=$(jq -r '.items[] | select(.kind | test("RoleBinding$")) | .subjects[]? | select(.kind != "ServiceAccount" or (.name | IN("kubemap","kubemap-node-agent") | not)) | "\(.kind) \(.name)"' <<<"$json")
	if [ -n "$bad" ]; then fail "bindings name other subjects: $bad"; else pass "every binding names only the agent ServiceAccounts"; fi
	echo
	echo "== Dry run on the API server (nothing is saved)"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	jq -c '.items[]' <<<"$json" | while read -r obj; do
		kind=$(jq -r .kind <<<"$obj")
		name=$(jq -r .metadata.name <<<"$obj")
		ns=$(jq -r '.metadata.namespace // ""' <<<"$obj")
		f=$tmp/obj.json
		printf '%s\n' "$obj" >"$f"
		if [ -n "$ns" ] && ! "${KUBECTL[@]}" get namespace "$ns" >/dev/null 2>&1; then
			if r=$("${KUBECTL[@]}" apply --dry-run=client --validate=strict -f "$f" 2>&1); then
				echo "PASS  $kind $ns/$name: client dry-run with strict validation (namespace $ns does not exist yet, so the server cannot dry-run it)"
			else
				echo "FAIL  $kind $ns/$name: $r"
			fi
		elif r=$("${KUBECTL[@]}" apply --dry-run=server --validate=strict -f "$f" 2>&1); then
			echo "PASS  $kind ${ns:+$ns/}$name: server dry-run accepted"
		else
			echo "FAIL  $kind ${ns:+$ns/}$name: $r"
		fi
	done >"$tmp/dry.txt"
	while read -r line; do echo "$line"; [[ $line == FAIL* ]] && fails=$((fails + 1)); done <"$tmp/dry.txt"
	if grep -q 'namespace .* does not exist yet' "$tmp/dry.txt"; then
		note "Pod Security admission for the agent pods is checked by the offline pod security check above, and by 'post' after the install."
	fi
	finish
	;;
snapshot)
	out=${2:-}
	[ -n "$out" ] || die "usage: $0 snapshot FILE"
	count >"$out" || die "could not count objects"
	if grep -q '^__discovery__ error' "$out"; then
		echo "NOTE  API discovery was incomplete: $(grep '^__discovery__ error' "$out" | cut -d' ' -f3-); kinds of the missing API groups are not counted"
	fi
	if grep -q ' error$' "$out"; then
		echo "NOTE  could not list: $(awk '$2 == "error" {print $1}' "$out" | paste -sd' ' -) (no rights, or an API service is down); these kinds are not compared"
	fi
	echo "wrote the object counts of $(wc -l <"$out") kinds (outside ${NAMESPACES[*]}, without fast-changing kinds) to $out"
	;;
post)
	snap=${2:-}
	[ -f "$snap" ] || die "usage: $0 post SNAPSHOT (made with: $0 snapshot SNAPSHOT before the install)"
	echo "== Running agent pods"
	pods=$("${KUBECTL[@]}" get pods -n kubemap-system -o json 2>/dev/null | jq '{items: .items}')
	nodepods=$("${KUBECTL[@]}" get pods -n kubemap-node -o json 2>/dev/null | jq '{items: .items}')
	all=$(jq -s '{items: (.[0].items + .[1].items)}' <<<"$pods $nodepods")
	[ "$(jq '.items | length' <<<"$all")" -gt 0 ] || fail "no pods in ${NAMESPACES[*]}"
	out=$(podsec <<<"$all")
	while read -r line; do [ -n "$line" ] || continue; echo "$line"; [[ $line == FAIL* ]] && fails=$((fails + 1)); done <<<"$out"
	jq -r '.items[] | "\(.metadata.namespace)/\(.metadata.name) \(.status.phase) on \(.spec.nodeName)"' <<<"$all" | sed 's/^/INFO  /'
	echo
	echo "== Objects Kubemap owns (label app.kubernetes.io/managed-by=kubemap)"
	# Read-only by design: the install owns only two ClusterRoles and their
	# bindings. Any operator role or admission policy shows up as unexpected.
	wide=clusterroles,clusterrolebindings,validatingadmissionpolicies,validatingadmissionpolicybindings
	"${KUBECTL[@]}" get "$wide,namespaces" -l app.kubernetes.io/managed-by=kubemap -o name 2>/dev/null | sed 's/^/INFO  /'
	extra=$("${KUBECTL[@]}" get "$wide" -l app.kubernetes.io/managed-by=kubemap -o name 2>/dev/null |
		grep -vxE 'clusterrole(binding)?\.rbac\.authorization\.k8s\.io/kubemap-(readonly|tokenreview)')
	if [ -n "$extra" ]; then fail "unexpected cluster-wide Kubemap objects: $extra"; else pass "cluster-wide Kubemap objects are only the two ClusterRoles and their bindings"; fi
	echo
	echo "== Objects outside ${NAMESPACES[*]}, before and after"
	now=$(count)
	changed=0
	if grep -q '^__discovery__ error' "$snap" || grep -q '^__discovery__ error' <<<"$now"; then
		note "API discovery was incomplete before or now; kinds of the missing API groups are not compared"
	fi
	while read -r kind n rest; do
		[ "$kind" = __discovery__ ] && continue
		before=$(awk -v k="$kind" '$1 == k {print $2}' "$snap")
		if [ "$n" = error ] || [ "$before" = error ]; then
			note "could not list $kind (before: ${before:-none}, now: $n); not compared"
			continue
		fi
		if [ "$n" != "${before:-0}" ]; then
			fail "$kind: ${before:-0} before, $n now"
			changed=1
		fi
	done <<<"$now"
	[ "$changed" = 0 ] && pass "no object count outside ${NAMESPACES[*]} changed (Kubemap's own namespaces, ClusterRoles, bindings and guard policies left out)"
	note "Counts only: an update of an existing object keeps the count and is not detected here; the RBAC checks (verify-agent-permissions.sh) are what rule out updates."
	note "Other controllers can change counts too; a FAIL here needs a look, not panic."
	finish
	;;
*)
	sed -n '2,29p' "$0" | sed 's/^# \{0,1\}//'
	exit 2
	;;
esac
