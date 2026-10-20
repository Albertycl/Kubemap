# Kubemap Helm chart

Renders the same objects as `deploy/kubemap.yaml`, `deploy/networkpolicy.yaml` and `deploy/ingress.yaml`
(read-only by design). A Go test (`agent/install/helm_test.go`) fails if the chart and the manifests drift.
The manifests and `deploy/install.sh` remain the other way to install.

```
helm lint deploy/helm/kubemap -n kubemap-system
helm install kubemap deploy/helm/kubemap -n kubemap-system --create-namespace
```

## Namespaces

- `kubemap-system` is the release namespace. `--create-namespace` creates it, so the chart does not template it.
  The release does not own it: `helm uninstall` leaves it. It is created without the Pod Security label;
  NOTES.txt prints the `kubectl label` command that adds `pod-security.kubernetes.io/enforce=restricted`.
  The chart refuses any other release namespace.
- `kubemap-node` (node agents, hostPID) is templated and owned by the release: `helm uninstall` removes it.

## Volume

PVC `kubemap-data` holds the database. By default it has `helm.sh/resource-policy: keep`, so `helm uninstall`
leaves it. A later `helm install` then fails on it ("exists and cannot be imported") until you delete it
(`kubectl -n kubemap-system delete pvc kubemap-data`, data lost) or adopt it (see NOTES.txt). Set
`--set persistence.keepOnUninstall=false` to delete the data with the release.

## Values

See `values.yaml`: `image.*`, `imagePullSecrets` (names only; create the Secrets in both namespaces yourself),
`trustedProxies`, `ingress.{enabled,host,issuer}`, `networkPolicy.enabled`, `persistence.{size,storageClass,keepOnUninstall}`.
Security settings (RBAC, securityContext, probes, limits, token audience) are not values on purpose.
