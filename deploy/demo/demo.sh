#!/usr/bin/env bash
# Demo scenarios for the interview (KM-71, A9). Everything lives in the
# namespace kubemap-demo; this script never touches another namespace.
#
#   demo.sh create          create the namespace and all nine broken scenarios
#   demo.sh status [--wait] show each scenario's state; --wait polls until all
#                           are broken as expected (at most 2 minutes)
#   demo.sh reset           undo any fixes: recreate the scenarios, broken again
#   demo.sh delete          delete the whole namespace
#   demo.sh budget          sum of the memory limits in the namespace
#
# Scenarios (KM-71): oom, crashloop, netpol, badsvc, pending; (KM-87):
# readiness503, upstream502, imagepull, rbacforbidden. Their symptoms, causes,
# fixes and prevention: docs/demo-scenarios.md. The 503 and 502 labs are
# checked through Traefik (curl to its LoadBalancer address, if reachable).
set -euo pipefail

NS=kubemap-demo
DIR=$(cd "$(dirname "$0")" && pwd)
K=(kubectl)

# guard: every object in the manifests is the namespace itself or inside it.
guard() {
	local bad
	bad=$("${K[@]}" apply --dry-run=client -f "$DIR" -o jsonpath='{range .items[*]}{.kind}/{.metadata.name} ns={.metadata.namespace}{"\n"}{end}' |
		awk -v ns="$NS" '!($1 == "Namespace/" ns && $2 == "ns=") && $2 != "ns=" ns')
	if [ -n "$bad" ]; then
		echo "refusing: objects outside $NS:" >&2
		echo "$bad" >&2
		exit 1
	fi
}

create() {
	guard
	"${K[@]}" apply -f "$DIR/00-namespace.yaml"
	"${K[@]}" apply -f "$DIR"
	# Run the reporter once now, instead of waiting for its schedule.
	if ! "${K[@]}" -n "$NS" get job reporter-first >/dev/null 2>&1; then
		"${K[@]}" -n "$NS" create job reporter-first --from=cronjob/reporter
	fi
}

reset() {
	guard
	# Remove whatever a fix changed or added, then apply the broken state again.
	"${K[@]}" -n "$NS" delete deployment,service,networkpolicy,ingressroutes.traefik.io,cronjob,job,role,rolebinding --all --wait=true --ignore-not-found
	"${K[@]}" -n "$NS" delete configmap,serviceaccount -l app.kubernetes.io/part-of=kubemap-demo --wait=true --ignore-not-found
	"${K[@]}" -n "$NS" delete pod --all --wait=true --ignore-not-found --grace-period=1
	create
}

delete() {
	"${K[@]}" delete namespace "$NS" --ignore-not-found --wait=true --timeout=180s
}

budget() {
	"${K[@]}" -n "$NS" get deploy,cronjob -o json | python3 -c '
import json, sys
units = {"Ki": 1 / 1024, "Mi": 1, "Gi": 1024}
def mib(v):
    for u, f in units.items():
        if v.endswith(u):
            return float(v[:-2]) * f
    return float(v) / 1048576
total = 0
for d in json.load(sys.stdin)["items"]:
    if d["kind"] == "CronJob":
        n, spec = 1, d["spec"]["jobTemplate"]["spec"]["template"]["spec"]  # one run at a time (Forbid)
    else:
        n, spec = d["spec"]["replicas"], d["spec"]["template"]["spec"]
    for c in spec["containers"]:
        m = mib(c["resources"]["limits"]["memory"])
        total += m * n
        dn, cn = d["metadata"]["name"], c["name"]
        print(f"{dn:14} {cn:15} x{n} {m:5.0f} MiB")
print(f"total memory limits: {total:.0f} MiB, the CronJob counted while a run is active (quota 160 MiB)")
'
	"${K[@]}" -n "$NS" get resourcequota demo-budget -o jsonpath='quota: limits.memory used {.status.used.limits\.memory} of {.status.hard.limits\.memory}{"\n"}'
}

# pod_json <name label> prints the first pod of that app as JSON.
pod_json() { "${K[@]}" -n "$NS" get pods -l "app.kubernetes.io/name=$1" -o json; }

# Each check prints "<scenario> BROKEN|NOT-YET|FIXED <detail>".
check_oom() {
	pod_json oom | python3 -c '
import json, sys
p = [x for x in json.load(sys.stdin)["items"] if not x["metadata"].get("deletionTimestamp")]
if not p: print("oom NOT-YET no pod"); sys.exit()
cs = (p[0]["status"].get("containerStatuses") or [{}])[0]
last = cs.get("lastState", {}).get("terminated") or cs.get("state", {}).get("terminated") or {}
wait = cs.get("state", {}).get("waiting", {}).get("reason", "")
r = cs.get("restartCount", 0)
code = last.get("exitCode")
reason = last.get("reason", "Error")
if last.get("reason") == "OOMKilled" or code == 137:
    print(f"oom BROKEN {reason} exit {code} (SIGKILL: memory limit), restarts {r} {wait}")
elif cs.get("ready"):
    print(f"oom FIXED running, restarts {r}")
else:
    print(f"oom NOT-YET restarts {r} {wait}")
'
}

check_crashloop() {
	pod_json checkout | python3 -c '
import json, sys
p = [x for x in json.load(sys.stdin)["items"] if not x["metadata"].get("deletionTimestamp")]
if not p: print("crashloop NOT-YET no pod"); sys.exit()
cs = (p[0]["status"].get("containerStatuses") or [{}])[0]
wait = cs.get("state", {}).get("waiting", {}).get("reason", "")
last = cs.get("lastState", {}).get("terminated") or {}
r = cs.get("restartCount", 0)
code = last.get("exitCode")
reason = last.get("reason", "Error")
if wait == "CrashLoopBackOff" or (r >= 1 and code == 1 and not cs.get("ready")):
    print(f"crashloop BROKEN {wait or reason}, last exit {code}, restarts {r}")
elif cs.get("ready"):
    print(f"crashloop FIXED running, restarts {r}")
else:
    print(f"crashloop NOT-YET restarts {r} {wait}")
'
}

check_netpol() {
	local ready logs
	ready=$("${K[@]}" -n "$NS" get pods -l app.kubernetes.io/name=api -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null || true)
	logs=$("${K[@]}" -n "$NS" logs deploy/web --since=20s 2>/dev/null || true)
	if [ "$ready" != true ]; then
		echo "netpol NOT-YET api not ready"
	elif grep -q ' ERROR ' <<<"$logs"; then
		echo "netpol BROKEN api ready, web logs: $(grep ' ERROR ' <<<"$logs" | tail -1 | cut -c1-110)"
	elif grep -q ' ok$' <<<"$logs"; then
		echo "netpol FIXED web reaches api"
	else
		echo "netpol NOT-YET no web log lines yet"
	fi
}

check_badsvc() {
	local ready eps
	ready=$("${K[@]}" -n "$NS" get pods -l app.kubernetes.io/name=catalog -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null || true)
	eps=$("${K[@]}" -n "$NS" get endpointslices -l kubernetes.io/service-name=catalog -o jsonpath='{range .items[*].endpoints[*]}{.addresses[0]} {end}' 2>/dev/null || true)
	if [ "$ready" != true ]; then
		echo "badsvc NOT-YET catalog pod not ready"
	elif [ -z "${eps// /}" ]; then
		echo "badsvc BROKEN Service catalog has 0 endpoints, pod catalog is ready"
	else
		echo "badsvc FIXED endpoints: $eps"
	fi
}

check_pending() {
	pod_json batch-worker | python3 -c '
import json, sys
p = [x for x in json.load(sys.stdin)["items"] if not x["metadata"].get("deletionTimestamp")]
if not p: print("pending NOT-YET no pod"); sys.exit()
st = p[0]["status"]
cond = {c["type"]: c for c in st.get("conditions", [])}
sch = cond.get("PodScheduled", {})
phase = st.get("phase")
if phase == "Pending" and sch.get("reason") == "Unschedulable":
    print("pending BROKEN Pending: " + sch.get("message", "")[:90])
elif phase == "Running":
    print("pending FIXED running")
else:
    print(f"pending NOT-YET phase {phase}")
'
}

# traefik_code <host>: the HTTP status Traefik answers for that host, or ""
# when Traefik cannot be reached from here.
traefik_code() {
	local ip
	ip=$("${K[@]}" -n kube-system get svc traefik -o jsonpath='{.status.loadBalancer.ingress[0].ip}' 2>/dev/null) || return 0
	[ -n "$ip" ] && command -v curl >/dev/null || return 0
	curl -s -o /dev/null -m 3 -w '%{http_code}' -H "Host: $1" "http://$ip/" 2>/dev/null || true
}

check_readiness503() {
	local ready eps code
	ready=$("${K[@]}" -n "$NS" get pods -l app.kubernetes.io/name=shop-web -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null || true)
	eps=$("${K[@]}" -n "$NS" get endpointslices -l kubernetes.io/service-name=shop-web -o jsonpath='{range .items[*].endpoints[?(@.conditions.ready==true)]}{.addresses[0]} {end}' 2>/dev/null || true)
	code=$(traefik_code demo-503.local)
	if [ "$ready" = true ] && [ -n "${eps// /}" ]; then
		echo "readiness503 FIXED pod Ready, Traefik answers ${code:-?}"
	elif [ "$ready" = false ] && [ -z "${eps// /}" ] && [ "${code:-503}" = 503 ]; then
		echo "readiness503 BROKEN pod Running but not Ready, 0 ready endpoints, Traefik answers ${code:-? (not reachable)}"
	else
		echo "readiness503 NOT-YET ready=${ready:-none} code=${code:-?}"
	fi
}

check_upstream502() {
	local ready code
	ready=$("${K[@]}" -n "$NS" get pods -l app.kubernetes.io/name=orders -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null || true)
	code=$(traefik_code demo-502.local)
	if [ "$ready" != true ]; then
		echo "upstream502 NOT-YET orders pod not ready"
	elif [ "$code" = 502 ]; then
		echo "upstream502 BROKEN pod Ready, Traefik answers 502 (the upstream closes the connection)"
	elif [ "$code" = 200 ]; then
		echo "upstream502 FIXED Traefik answers 200"
	elif [ -z "$code" ]; then
		echo "upstream502 BROKEN? pod Ready; Traefik not reachable from here to confirm the 502"
	else
		echo "upstream502 NOT-YET Traefik answers $code"
	fi
}

check_imagepull() {
	local wait img
	wait=$("${K[@]}" -n "$NS" get pods -l app.kubernetes.io/name=invoice -o jsonpath='{.items[0].status.containerStatuses[0].state.waiting.reason}' 2>/dev/null || true)
	img=$("${K[@]}" -n "$NS" get pods -l app.kubernetes.io/name=invoice -o jsonpath='{.items[0].spec.containers[0].image}' 2>/dev/null || true)
	case "$wait" in
	ErrImagePull | ImagePullBackOff) echo "imagepull BROKEN $wait for $img" ;;
	"")
		if [ "$("${K[@]}" -n "$NS" get pods -l app.kubernetes.io/name=invoice -o jsonpath='{.items[0].status.containerStatuses[0].ready}' 2>/dev/null)" = true ]; then
			echo "imagepull FIXED running $img"
		else
			echo "imagepull NOT-YET no container status yet"
		fi
		;;
	*) echo "imagepull NOT-YET $wait" ;;
	esac
}

check_rbacforbidden() {
	local job logs
	# The newest reporter run.
	job=$("${K[@]}" -n "$NS" get jobs -l app.kubernetes.io/name=reporter --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1:].metadata.name}' 2>/dev/null || true)
	[ -n "$job" ] || { echo "rbacforbidden NOT-YET no reporter run yet"; return; }
	logs=$("${K[@]}" -n "$NS" logs "job/$job" 2>/dev/null || true)
	if grep -q 'HTTP 403' <<<"$logs"; then
		echo "rbacforbidden BROKEN job $job: $(grep -o "cannot list resource[^,]*" <<<"$logs" | head -1 | sed "s#\\\\##g; s#\"\$##" | cut -c1-90)"
	elif grep -q '^report:' <<<"$logs"; then
		echo "rbacforbidden FIXED job $job: $(grep '^report:' <<<"$logs")"
	else
		echo "rbacforbidden NOT-YET job $job has no result yet"
	fi
}

status_once() {
	check_oom
	check_crashloop
	check_netpol
	check_badsvc
	check_pending
	check_readiness503
	check_upstream502
	check_imagepull
	check_rbacforbidden
}

status() {
	if ! "${K[@]}" get namespace "$NS" >/dev/null 2>&1; then
		echo "namespace $NS does not exist (run: demo.sh create)"
		return 1
	fi
	if [ "${1:-}" != --wait ]; then
		status_once | column -t -l 3
		return 0
	fi
	local start=$SECONDS out
	while :; do
		out=$(status_once)
		if ! grep -qv ' BROKEN ' <<<"$out"; then
			column -t -l 3 <<<"$out"
			echo "all $(wc -l <<<"$out") scenarios broken as expected after $((SECONDS - start)) s"
			return 0
		fi
		if [ $((SECONDS - start)) -ge 120 ]; then
			column -t -l 3 <<<"$out"
			echo "not all scenarios broken after 120 s" >&2
			return 1
		fi
		sleep 5
	done
}

case "${1:-}" in
create) create ;;
reset) reset ;;
delete) delete ;;
status) status "${2:-}" ;;
budget) budget ;;
*)
	sed -n '4,11p' "$0" | sed 's/^# \{0,1\}//'
	exit 2
	;;
esac
