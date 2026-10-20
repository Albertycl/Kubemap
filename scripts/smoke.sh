#!/bin/sh
# make smoke: check that both binaries run and report the same version.
# The server needs a Kubernetes API to start, so it is not started here; the
# in-process path is covered by the Go tests (portal/localcluster, portal/tunnel).
#
#   scripts/smoke.sh <server binary> <agent binary> [expected version]
set -eu

portal=${1:?usage: smoke.sh <server binary> <agent binary> [version]}
agent=${2:?usage: smoke.sh <server binary> <agent binary> [version]}
want=${3:-}

fail() {
	echo "smoke: FAIL: $*" >&2
	exit 1
}

[ -x "$portal" ] || fail "$portal is not an executable (run make build)"
[ -x "$agent" ] || fail "$agent is not an executable (run make build)"

pv=$("$portal" version) || fail "$portal version failed"
av=$("$agent" version | head -n 1) || fail "$agent version failed"
case "$av" in *"$pv"*) ;; *) fail "server reports $pv, agent reports $av" ;; esac
if [ -n "$want" ] && [ "$pv" != "$want" ]; then
	fail "version is $pv, want $want"
fi
"$agent" bundle --image localhost/kubemap:smoke | grep -q 'kind: Deployment' || fail "the bundle has no Deployment"

echo "smoke: OK: version $pv"
