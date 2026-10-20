#!/usr/bin/env bash
# Install Kubemap into the cluster your kubeconfig points at.
#
#   deploy/install.sh                  list what would be applied (nothing changes)
#   deploy/install.sh --yes            apply it (host kubemap.albertycliu.xyz, issuer letsencrypt-prod)
#   deploy/install.sh --host HOST --issuer NAME --yes   use another name or issuer
#
# Options:
#   --host NAME        the name the browser uses (default kubemap.albertycliu.xyz); goes into the Certificate and IngressRoutes
#   --image IMAGE      image with the server and node agent (default localhost/kubemap:dev)
#   --issuer NAME      cert-manager ClusterIssuer (default letsencrypt-prod; for a private name use your own, for example lab-ca-issuer)
#   --no-ingress       skip the Certificate and IngressRoutes (reach it with kubectl port-forward)
#   --no-netpol        skip the NetworkPolicies
#   --context NAME     kubeconfig context
#
# It applies deploy/kubemap.yaml (namespaces, RBAC, server, node agents),
# then deploy/networkpolicy.yaml and deploy/ingress.yaml. The image has no
# shell, so the first admin password is set with the server binary:
#   kubectl -n kubemap-system exec deploy/kubemap -- /usr/local/bin/kubemap-portal reset-admin-password
# No sudo; nothing outside the cluster.
set -euo pipefail

dir=$(cd "$(dirname "$0")" && pwd)
host="kubemap.albertycliu.xyz" image="localhost/kubemap:dev" issuer="letsencrypt-prod" yes=0 ingress=1 netpol=1 ctx=()
while [ $# -gt 0 ]; do
	case $1 in
	--host) host=${2:?}; shift 2 ;;
	--image) image=${2:?}; shift 2 ;;
	--issuer) issuer=${2:?}; shift 2 ;;
	--context) ctx=(--context "${2:?}"); shift 2 ;;
	--yes) yes=1; shift ;;
	--no-ingress) ingress=0; shift ;;
	--no-netpol) netpol=0; shift ;;
	-h | --help) sed -n 2,21p "$0"; exit 0 ;;
	*) echo "install.sh: unknown option $1" >&2; exit 2 ;;
	esac
done
if [ "$ingress" = 1 ]; then
	[ -n "$host" ] || { echo "install.sh: --host NAME is required (or --no-ingress)" >&2; exit 2; }
	# Both values go into sed expressions: allow DNS-name characters only.
	echo "$host" | grep -Eqx '[a-z0-9]([a-z0-9.-]*[a-z0-9])?' || { echo "install.sh: --host $host is not a host name" >&2; exit 2; }
	echo "$issuer" | grep -Eqx '[a-z0-9]([a-z0-9.-]*[a-z0-9])?' || { echo "install.sh: --issuer $issuer is not a valid issuer name" >&2; exit 2; }
fi
echo "$image" | grep -Eqx '[A-Za-z0-9._/:@-]+' || { echo "install.sh: bad --image" >&2; exit 2; }

k=(kubectl "${ctx[@]}")

apply() { sed "$1" "$2" | "${k[@]}" apply -f -; }

if [ "$yes" = 1 ]; then
	apply "s#localhost/kubemap:dev#$image#g" "$dir/kubemap.yaml"
	"${k[@]}" -n kubemap-system rollout status deploy/kubemap --timeout=180s
	[ "$netpol" = 1 ] && "${k[@]}" apply -f "$dir/networkpolicy.yaml"
	if [ "$ingress" = 1 ]; then
		apply "s#kubemap.albertycliu.xyz#$host#g; s#name: letsencrypt-prod#name: $issuer#" "$dir/ingress.yaml"
		echo "Open https://$host/ (the name must point at Traefik's address)."
	else
		echo "Reach it with: kubectl -n kubemap-system port-forward svc/kubemap 8080:8080  then http://localhost:8080/"
	fi
	echo "Admin password: kubectl -n kubemap-system exec deploy/kubemap -- /usr/local/bin/kubemap-portal reset-admin-password"
else
	echo "Dry run (nothing is changed). Objects that would be applied:"
	sed "s#localhost/kubemap:dev#$image#g" "$dir/kubemap.yaml" | "${k[@]}" apply --dry-run=client -f -
	[ "$netpol" = 1 ] && "${k[@]}" apply --dry-run=client -f "$dir/networkpolicy.yaml"
	[ "$ingress" = 1 ] && sed "s#kubemap.albertycliu.xyz#$host#g; s#name: letsencrypt-prod#name: $issuer#" "$dir/ingress.yaml" | "${k[@]}" apply --dry-run=client -f -
	echo "Run again with --yes to apply."
fi
