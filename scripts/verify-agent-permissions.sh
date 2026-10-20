#!/usr/bin/env bash
# Proof that the Kubemap agents cannot change a cluster (KM-81).
#
# Live (asks the cluster's API server, changes nothing):
#   scripts/verify-agent-permissions.sh [--context NAME]
#
#   For both agent ServiceAccounts, it runs "kubectl auth can-i" with --as and
#   the groups a ServiceAccount token carries, for every line of
#   scripts/agent-permission-checks.txt. A "deny" line must answer "no", an
#   "allow" line "yes" (there is none now). It then prints what
#   the cluster already grants to every ServiceAccount through groups, and
#   the full "kubectl auth can-i --list" of both agents for the record.
#
#   It works before the install too: it then shows what the cluster's
#   existing bindings would give the agents. "auth can-i" creates no object;
#   your kubeconfig user needs the "impersonate" right (cluster admins have it).
#
#   --before-install: the agents are not installed yet, so any "allow" lines
#   must answer "no".
#
#   Besides the list, it compares each agent's "auth can-i --list" with that
#   of a ServiceAccount that does not exist in the same namespace (it has only
#   what every ServiceAccount inherits). Any extra write verb must be one of
#   the approved exceptions (tokenreviews create).
#
# Static (no cluster; needs Go and this repository):
#   scripts/verify-agent-permissions.sh --static FILE...
#
#   Evaluates the RBAC in the rendered YAML (deploy/kubemap.yaml) with scripts/permcheck.
#
# Exit status: 0 = every check passed, 1 = at least one failed, 2 = could not run.
set -uo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
CHECKS=$ROOT/scripts/agent-permission-checks.txt
KUBECTL=(kubectl)
BEFORE=0

usage() { sed -n '2,32p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }

static=()
while [ $# -gt 0 ]; do
	case "$1" in
	-h | --help) usage ;;
	--before-install) BEFORE=1 ;;
	--context)
		[ -n "${2:-}" ] || usage
		KUBECTL+=(--context "$2")
		shift
		;;
	--static)
		shift
		[ $# -gt 0 ] || usage
		for f in "$@"; do
			case "$f" in /*) static+=("$f") ;; *) static+=("$PWD/$f") ;; esac
		done
		break
		;;
	*) usage ;;
	esac
	shift
done
if [ ${#static[@]} -gt 0 ]; then
	command -v go >/dev/null || { echo "verify: --static needs Go (run it from the Kubemap repository)" >&2; exit 2; }
	cd "$ROOT" && exec go run ./scripts/permcheck -checks "$CHECKS" "${static[@]}"
fi
command -v kubectl >/dev/null || { echo "verify: kubectl is needed" >&2; exit 2; }
command -v jq >/dev/null || { echo "verify: jq is needed" >&2; exit 2; }
"${KUBECTL[@]}" version >/dev/null 2>&1 || { echo "verify: cannot reach the cluster with this kubeconfig" >&2; exit 2; }

AGENTS=("cluster kubemap-system kubemap" "node kubemap-node kubemap-node-agent")
# The identity for the "what every ServiceAccount inherits" baseline: a
# ServiceAccount in a namespace that does not exist, so no namespace group
# grants apply to it.
PROBE_NS=kubemap-permission-probe
PROBE_NAME=none
# KUBEMAP_AGENTS="short namespace name;…" checks other ServiceAccounts, to
# test the kit itself against identities that do have rights.
if [ -n "${KUBEMAP_AGENTS:-}" ]; then
	IFS=';' read -r -a AGENTS <<<"$KUBEMAP_AGENTS"
fi
SCOPES=(kubemap-system kubemap-node default kube-system)
CLUSTER_SCOPED=" nodes namespaces persistentvolumes users groups clusterroles.rbac.authorization.k8s.io clusterrolebindings.rbac.authorization.k8s.io customresourcedefinitions.apiextensions.k8s.io mutatingwebhookconfigurations.admissionregistration.k8s.io validatingwebhookconfigurations.admissionregistration.k8s.io tokenreviews.authentication.k8s.io "

as_args() { # as_args <namespace> <name>
	echo "--as=system:serviceaccount:$1:$2 --as-group=system:serviceaccounts --as-group=system:serviceaccounts:$1 --as-group=system:authenticated"
}

# rb_namespaces <namespace> <name>: the namespaces where a RoleBinding names
# this ServiceAccount, its user name, or one of the groups its token carries.
# "auth can-i -A" only answers for cluster-wide rights, so these namespaces
# must be asked one by one. RoleBindings hold no secrets.
RB_JSON=$("${KUBECTL[@]}" get rolebindings -A -o json 2>/dev/null) || { echo "verify: cannot list RoleBindings" >&2; exit 2; }
rb_namespaces() {
	jq -r --arg ns "$1" --arg n "$2" '
	  .items[] | .metadata.namespace as $b |
	  select(any(.subjects[]?;
	    (.kind == "ServiceAccount" and .name == $n and ((.namespace // $b) == $ns)) or
	    (.kind == "User" and .name == ("system:serviceaccount:" + $ns + ":" + $n)) or
	    (.kind == "Group" and (.name == "system:serviceaccounts" or .name == ("system:serviceaccounts:" + $ns) or .name == "system:authenticated"))
	  )) | $b' <<<"$RB_JSON" | sort -u
}

echo "Cluster: $("${KUBECTL[@]}" config current-context 2>/dev/null) ($("${KUBECTL[@]}" version -o json 2>/dev/null | sed -n 's/.*"gitVersion": *"\([^"]*\)".*/\1/p' | tail -1))"
echo "Checks:  $CHECKS"
declare -A EXTRA
for a in "${AGENTS[@]}"; do
	read -r short ans aname <<<"$a"
	EXTRA[$short]=$(rb_namespaces "$ans" "$aname" | paste -sd' ' -)
	echo "RoleBindings for the $short agent (system:serviceaccount:$ans:$aname, or its groups) in: ${EXTRA[$short]:-none}"
done
echo

fails=0
total=0
while read -r expect who ns verb resource sub name; do
	case "$expect" in "" | \#*) continue ;; esac
	[ "$sub" = - ] && sub=""
	[ "$name" = - ] && name=""
	for a in "${AGENTS[@]}"; do
		read -r short ans aname <<<"$a"
		[ "$who" = "*" ] || [ "$who" = "$short" ] || continue
		if [ "$ns" != "*" ]; then
			scopes=("$ns")
		elif [[ $CLUSTER_SCOPED == *" $resource "* ]]; then
			scopes=(-A)
		else
			# shellcheck disable=SC2206
			scopes=(-A $(printf '%s\n' "${SCOPES[@]}" ${EXTRA[$short]} | awk '!seen[$0]++'))
		fi
		target=$resource
		[ -n "$name" ] && target="$resource/$name"
		for s in "${scopes[@]}"; do
			if [ "$s" = -A ]; then nsarg=(-A); where=all-namespaces; else nsarg=(-n "$s"); where=$s; fi
			subarg=()
			[ -n "$sub" ] && subarg=(--subresource "$sub")
			# shellcheck disable=SC2046
			# The answer is the last line; kubectl may print a warning first
			# (for example for users or groups, which are not API objects).
			out=$("${KUBECTL[@]}" auth can-i "$verb" "$target" "${subarg[@]}" "${nsarg[@]}" $(as_args "$ans" "$aname") 2>&1 | tail -n 1)
			case "$out" in
			yes*) got=yes ;;
			no*) got=no ;;
			*) got="error: $out" ;;
			esac
			want=no
			[ "$expect" = allow ] && [ "$BEFORE" = 0 ] && want=yes
			total=$((total + 1))
			label="$resource${sub:+/$sub}${name:+ name=$name}"
			if [ "$got" = "$want" ]; then
				printf 'PASS  %-7s %-16s %-12s %-44s -> %s\n' "$short" "$where" "$verb" "$label" "$got"
			else
				printf 'FAIL  %-7s %-16s %-12s %-44s -> %s (want %s)\n' "$short" "$where" "$verb" "$label" "$got" "$want"
				fails=$((fails + 1))
			fi
		done
	done
done <"$CHECKS"

echo
echo "== What this cluster grants to EVERY ServiceAccount (groups system:serviceaccounts, system:authenticated)"
echo "   Not Kubemap's doing, but the agents inherit it. Review these for a production cluster."
"${KUBECTL[@]}" get clusterrolebindings -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.roleRef.kind}/{.roleRef.name}{"\t"}{range .subjects[*]}{.kind}:{.name},{end}{"\n"}{end}' 2>/dev/null |
	awk -F'\t' '$3 ~ /Group:system:(serviceaccounts|authenticated|unauthenticated)(,|$)|Group:system:serviceaccounts:kubemap/ {printf "   ClusterRoleBinding %s -> %s  (%s)\n", $1, $2, $3}'
"${KUBECTL[@]}" get rolebindings -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\t"}{.roleRef.kind}/{.roleRef.name}{"\t"}{range .subjects[*]}{.kind}:{.name},{end}{"\n"}{end}' 2>/dev/null |
	awk -F'\t' '$3 ~ /Group:system:(serviceaccounts|authenticated)(,|$)|Group:system:serviceaccounts:kubemap/ {printf "   RoleBinding %s -> %s  (%s)\n", $1, $2, $3}'

# rows <namespace> <sa-namespace> <sa-name>: resource rows of "auth can-i
# --list" as "resource|names|verbs", one per line. The separator is not
# whitespace, so an empty names column stays a column.
rows() {
	# shellcheck disable=SC2046
	"${KUBECTL[@]}" auth can-i --list -n "$1" $(as_args "$2" "$3") 2>/dev/null |
		awk 'NR > 1 && $1 !~ /^\[/ { n = split($0, f, /\] *\[|\[|\]/); r = $1; printf "%s|%s|%s\n", r, f[3], f[4] }'
}

# A binding to "every ServiceAccount in the agent's namespace" is never part
# of Kubemap's install; it widens the agent's rights without naming it.
for a in "${AGENTS[@]}"; do
	read -r short ans aname <<<"$a"
	groupbind=$( {
		"${KUBECTL[@]}" get clusterrolebindings -o json 2>/dev/null | jq -r --arg g "system:serviceaccounts:$ans" '.items[] | select(any(.subjects[]?; .kind == "Group" and .name == $g)) | "ClusterRoleBinding \(.metadata.name) -> \(.roleRef.kind)/\(.roleRef.name)"'
		jq -r --arg g "system:serviceaccounts:$ans" '.items[] | select(any(.subjects[]?; .kind == "Group" and .name == $g)) | "RoleBinding \(.metadata.namespace)/\(.metadata.name) -> \(.roleRef.kind)/\(.roleRef.name)"' <<<"$RB_JSON"
	} )
	total=$((total + 1))
	if [ -n "$groupbind" ]; then
		while read -r line; do echo "FAIL  $short: $line binds the group system:serviceaccounts:$ans (every ServiceAccount there, the agent included)"; done <<<"$groupbind"
		fails=$((fails + 1))
	else
		echo "PASS  $short: no binding to the group system:serviceaccounts:$ans"
	fi
done

for a in "${AGENTS[@]}"; do
	read -r short ans aname <<<"$a"
	# shellcheck disable=SC2046
	for s in $(printf '%s\n' "$ans" default ${EXTRA[$short]} | awk '!seen[$0]++'); do
		echo
		echo "== kubectl auth can-i --list  as $short agent (system:serviceaccount:$ans:$aname) in namespace $s"
		# shellcheck disable=SC2046
		"${KUBECTL[@]}" auth can-i --list -n "$s" $(as_args "$ans" "$aname") 2>&1 | sed 's/^/   /'
		# Write verbs the agent has beyond what every ServiceAccount inherits.
		# The probe lives in a namespace no ServiceAccount lives in, so its
		# baseline has only system:serviceaccounts and system:authenticated,
		# not the agent's namespace group (review-km-81-fixes B3).
		base=$(rows "$s" "$PROBE_NS" "$PROBE_NAME")
		while IFS='|' read -r res names verbs; do
			[ -n "$res" ] || continue
			grep -qxF "$res|$names|$verbs" <<<"$base" && continue
			w=$(tr ' ' '\n' <<<"$verbs" | grep -vxE 'get|list|watch' | tr '\n' ' ')
			[ -n "${w// /}" ] || continue
			total=$((total + 1))
			if [ "$short" = cluster ] && [ "$res" = tokenreviews.authentication.k8s.io ] && [ "${w// /}" = create ]; then
				echo "   PASS  create on tokenreviews: approved exception (node agent identity, stores nothing)"
			else
				echo "   FAIL  write verb(s) ${w}on $res${names:+ names=$names}: not an approved exception"
				fails=$((fails + 1))
			fi
		done < <(rows "$s" "$ans" "$aname")
	done
done

echo
echo "$total check(s), $fails failed."
if [ "$fails" -gt 0 ]; then
	echo "RESULT: FAIL${KUBEMAP_AGENTS:+ (identities overridden by KUBEMAP_AGENTS)}"
	exit 1
fi
if [ -n "${KUBEMAP_AGENTS:-}" ]; then
	echo "RESULT: PASS (identities overridden by KUBEMAP_AGENTS: this is a test of the kit, not the proof for the Kubemap agents)"
else
	echo "RESULT: PASS"
fi
