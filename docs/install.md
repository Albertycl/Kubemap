# Install Kubemap in the cluster

Kubemap is one Deployment (`kubemap`) and one DaemonSet (`kubemap-node-agent`) from the same image. You open it in a browser; nothing runs outside the cluster. Everything below runs where `kubectl` points at the cluster, with admin rights. No `sudo` is needed except to import the image into k3s.

## 1. Requirements

- Kubernetes 1.30 or newer (the TokenReview of a node agent token returns the node name only from 1.30).
- A StorageClass for one 1 GiB ReadWriteOnce volume (k3s `local-path` works).
- For the web address: Traefik with the `websecure` entrypoint and cert-manager with a ClusterIssuer (default `letsencrypt-prod`; a private name can use `--issuer lab-ca-issuer`). Without them use `--no-ingress` and `kubectl port-forward`.

## 2. Build and load the image

There is no registry yet, so the image is imported into k3s on every node that will run a pod.

```
make image VERSION=0.2.0 IMAGE=localhost/kubemap
docker save localhost/kubemap:0.2.0 | sudo k3s ctr images import -
```

`make release VERSION=0.2.0` writes the same image as `dist/kubemap-0.2.0-image.tar.gz`; on another node use `gunzip -c <file> | sudo k3s ctr images import -`.

## 3. Remove an older install

Only if the namespace `kubemap-system` holds an **old 0.1.x** Kubemap (for example the previous agent-only install), and only **before the first install of 0.2.0**:

```
deploy/uninstall.sh --yes
```

Run it first because `deploy/install.sh` over an old install does not replace it: the old `kubemap-agent` Deployment (and its `kubemap-agent-*` roles and guard policies) stay in place and keep running next to the new server. The script removes the old names (`kubemap-agent-readonly`, `kubemap-agent-tokenreview`, `kubemap-agent-operator*`, the `kubemap-agent` Deployment and Secret) and only objects that carry the label `app.kubernetes.io/managed-by=kubemap`. `deploy/uninstall_test.sh` checks with a fake `kubectl` that its dry run lists them.

Warning: this deletes the namespaces `kubemap-system` and `kubemap-node`, including the volume (PVC `kubemap-data`) with the SQLite database. Never run it on a working 0.2.0 install to upgrade it: the users, audit log and incident journal are lost for good. To upgrade, see "Upgrade" below. The script prints the same warning before it deletes.

## 4. Install

Without `--yes` the script only lists what it would apply.

```
deploy/install.sh --image localhost/kubemap:0.2.0
deploy/install.sh --image localhost/kubemap:0.2.0 --yes
```

The default host is `kubemap.albertycliu.xyz`; `--host HOST` overrides it.

Options: `--host HOST` (default `kubemap.albertycliu.xyz`), `--issuer NAME` (default `letsencrypt-prod`), `--no-ingress`, `--no-netpol`, `--context NAME`. It applies `deploy/kubemap.yaml` (namespaces, RBAC, PVC, server, Service, node agents, guard policies), waits for the rollout, then `deploy/networkpolicy.yaml` and `deploy/ingress.yaml` (Certificate, IngressRoute on `websecure`, http-to-https redirect; the route excludes `/v1/connect`, because node agents reach the Service inside the cluster). `kubectl apply -k deploy/` does the same with kustomize.

Make the host name point at Traefik's address (DNS or `/etc/hosts`).

## Install with Helm

An alternative to `deploy/install.sh`, with the same objects (chart: `deploy/helm/kubemap/`, see its README). Needs Helm 3.2 or newer (use `--set-string image.tag=...` so a numeric-looking tag stays text). Do not use both ways on the same cluster.

```
helm lint deploy/helm/kubemap -n kubemap-system
helm template kubemap deploy/helm/kubemap -n kubemap-system
helm install kubemap deploy/helm/kubemap -n kubemap-system --create-namespace --set ingress.host=kubemap.example.org
helm status kubemap -n kubemap-system
helm get values kubemap -n kubemap-system
helm upgrade --install kubemap deploy/helm/kubemap -n kubemap-system --set image.tag=NEWTAG
helm history kubemap -n kubemap-system
helm rollback kubemap 1 -n kubemap-system
helm uninstall kubemap -n kubemap-system
```

- `helm lint` checks the chart for mistakes. `helm template` prints the YAML it would apply and changes nothing (add `| kubectl apply --dry-run=server -f -` to let the API server check it; the namespace `kubemap-system` must already exist for that).
- `helm install` creates release `kubemap`; `--create-namespace` creates `kubemap-system` first. The namespace must be `kubemap-system`; the chart refuses another one. `--set` or `-f my-values.yaml` change the values in `values.yaml` (host, issuer, image, `ingress.enabled=false`, `networkPolicy.enabled=false`, `persistence.size`).
- `helm status` shows the release state and prints the next steps (NOTES.txt): wait for the rollout, set the admin password with `kubemap-portal reset-admin-password` (as in "First login"), open the URL, turn on two-factor under `#/security`.
- `helm get values` shows the values you set. `helm upgrade --install` installs or changes the release (use `--reuse-values` to keep earlier `--set` values, or pass them again). Each change is a numbered revision; `helm history` lists them and `helm rollback kubemap N` goes back to revision N.
- `helm uninstall` removes the objects of the release: the server, node agents, RBAC, ingress, NetworkPolicies and the namespace `kubemap-node`. It does NOT remove the namespace `kubemap-system`, because `--create-namespace` creates it outside the release, and it does NOT remove the PVC `kubemap-data` (annotation `helm.sh/resource-policy: keep`, so the database survives; `--set persistence.keepOnUninstall=false` makes it delete it). Because the PVC stays, a new `helm install` fails on it until you delete the PVC (data lost) or adopt it with the `kubectl label` and `kubectl annotate` commands printed in NOTES.txt. To remove everything: `helm uninstall`, then `kubectl delete ns kubemap-system` (this deletes the data).
- `kubemap-system` has no Pod Security label after `helm install`; add it with `kubectl label ns kubemap-system pod-security.kubernetes.io/enforce=restricted app.kubernetes.io/managed-by=kubemap`; to avoid an unlabelled window, run `kubectl create ns kubemap-system`, label it, then `helm install` (`--create-namespace` accepts an existing namespace). When adopting a kept PVC, keep the same `persistence.size` and `storageClass`; they cannot change on an existing PVC.

## 5. First login

The first start creates the user `admin` with a random password. The image has no shell, so set a known one with the server binary; it prints the new password:

```
kubectl -n kubemap-system exec deploy/kubemap -- /usr/local/bin/kubemap-portal reset-admin-password
```

Open `https://kubemap.albertycliu.xyz/`, log in as `admin` and set a new password (12 to 72 bytes). Without an ingress: `kubectl -n kubemap-system port-forward svc/kubemap 8080:8080` and open `http://localhost:8080/`.

## 6. Check

```
kubectl -n kubemap-system get deploy,svc,pvc
kubectl -n kubemap-node get ds,pods -o wide
curl -s https://kubemap.albertycliu.xyz/healthz
```

The cluster list shows "this cluster" as Active. Each node appears with its agent after the node agent registers (a few seconds).

## Internet exposure checklist

Do this before you open `https://kubemap.albertycliu.xyz/` to the internet. Only the owner must be able to log in. There is no sign-up: the only account is `admin`, with a random password that must be changed at the first login.

1. **DNS.** An A (and AAAA, if you have IPv6) record for `kubemap.albertycliu.xyz` that points to your public address.
2. **Router / NAT.** Forward port 443 to Traefik's address (the k3s node or its load balancer). Forward port 80 too: Let's Encrypt HTTP-01 needs port 80 reachable from the internet to issue and renew the certificate. Traefik answers port 80 with a redirect to https.
3. **Certificate.** `kubectl -n kubemap-system get certificate kubemap-tls` must show `READY True` (ClusterIssuer `letsencrypt-prod`). If not: `kubectl -n kubemap-system describe certificate,challenge,order`.
4. **First login, then two-factor.** Log in as `admin` and set a new password (the first one is random and forced to change). Then open your user name in the top bar (Account security), enter your password, add the secret or the `otpauth://` link to an authenticator app, type the 6-digit code, and save the 8 recovery codes (each works once, they are shown only once). From then on the login asks for a code.
5. **Check the client address.** After the first login, look at the audit log (`#/audit`): the address of the `login` entry must be your public address, not a node or pod address. If it is a pod or node address, every visitor shares one login limit key. A shared proxy address allows 50 wrong attempts per 15 minutes (a normal client address: 5), so it takes 50 wrong attempts from anyone to block everybody for 15 minutes; see "Client address and trusted proxies".
6. **Only the web port is public.** `/v1/connect` (node agents) is not routed through Traefik, and the NetworkPolicies allow the server's port 8080 only from Traefik and the node agents. Check: `curl -s -o /dev/null -w '%{http_code}\n' https://kubemap.albertycliu.xyz/v1/connect` must answer 404 (Traefik has no route for it), and `kubectl -n kubemap-system get networkpolicy` lists `default-deny-ingress` and `allow-traefik-and-node-agents`. Do not forward any other port.
7. **What is public.** Without a login the server answers only: `/healthz` (`{"status":"ok"}`, no version), `POST /api/login`, the page `/` and `/assets/*`. Every other `/api` route needs a session. The cookie is `Secure`, `HttpOnly`, `SameSite=Strict`; a session ends after 12 hours without use or 7 days after login.
8. **Lockout recovery.**
   - Lost password: `kubectl -n kubemap-system exec deploy/kubemap -- /usr/local/bin/kubemap-portal reset-admin-password`.
   - Lost phone and recovery codes: `kubectl -n kubemap-system exec deploy/kubemap -- /usr/local/bin/kubemap-portal disable-totp` (add `-user NAME` for another user). It removes the secret and the recovery codes and writes an audit entry (user `cli`). Log in with the password and turn two-factor on again.
   - Both need `kubectl` access to the cluster, so keep your kubeconfig safe: it is the real recovery key.
   - Blocked by the login limit (5 wrong attempts per address in 15 minutes): wait, or restart the server pod (`kubectl -n kubemap-system rollout restart deploy/kubemap`).
   - Second factor locked (10 wrong codes for one user, from any address, in 15 minutes): see "Account-wide second-factor limit" below.

Account-wide second-factor limit: 10 wrong codes (authenticator or recovery codes) for one user, from any address, lock the second factor of that user for 15 minutes. The lock does not end sessions that already exist, and the 15 minutes start at the first wrong code. The limit has a price: someone who knows the password (but has no code) can keep the admin locked out of a new login by sending 10 wrong codes again every 15 minutes. Recovery: set a new password with `kubectl -n kubemap-system exec deploy/kubemap -- /usr/local/bin/kubemap-portal reset-admin-password`, then restart the server pod (`kubectl -n kubemap-system rollout restart deploy/kubemap`), because the lock is kept in memory and a restart clears it. Without the restart the lock ends by itself after 15 minutes.

Two-factor details: RFC 6238 (SHA-1, 6 digits, 30 seconds, one step of clock drift allowed either way). A code works once. Wrong codes count against the same login limit as wrong passwords. The secret is stored encrypted (AES-GCM) with a key in `/data/kubemap/totp.key` on the same volume: this protects a copy of the database alone, not someone who can read the whole volume.

## Several nodes

The node agent runs on every node (tolerations let it run on control-plane nodes too) and registers with its own ServiceAccount token; nothing names a node. The server pod runs on one node only: its volume is ReadWriteOnce, the strategy is Recreate, and there is one replica. If that node dies, the server stays down until the volume can be attached elsewhere.

## Client address and trusted proxies

Behind Traefik the TCP peer of every browser request is the Traefik pod. The server therefore reads the client address (login limiter, audit log, login log) from `X-Forwarded-For`, and only when the direct peer is a trusted proxy: it takes the right-most entry that is not itself trusted. A header from any other peer is ignored, so a client cannot choose its own address. Set the trusted peers with `-trusted-proxies` or the environment variable `KUBEMAP_TRUSTED_PROXIES` (comma-separated CIDRs or IPs; empty trusts none). Default: `127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16`, which covers loopback and the usual pod ranges. If your pod CIDR is outside these, or Traefik runs outside the cluster, set it, or all users will share the Traefik address.

## Upgrade

Import the new image, then `deploy/install.sh --host ... --image localhost/kubemap:<new> --yes`. The server restarts (about a minute), the data stays on the volume, and node agents reconnect by themselves.

## Remove

`deploy/uninstall.sh --yes` removes the namespaces (and so the volume and the data), and the cluster roles. (It also removes the objects of an old 0.1.x install, `kubemap-agent-*`, and the operator role, binding and guard policies of an older version, if they exist.) Without `--yes` it only lists them.

## Known limits

- **ConfigMaps are readable cluster-wide.** The read-only ClusterRole `kubemap-readonly` can get, list and watch ConfigMaps (and read pod logs) in every namespace, and every logged-in Kubemap user sees what it shows. Do not keep secrets in ConfigMaps.
- **Read-only by design.** Kubemap has no fix buttons and no operator mode; it cannot change anything in the cluster. The node name that the TokenReview returns for a node agent token (bound-node claim) needs Kubernetes 1.30 or newer. Older clusters are not supported.
- **The PVC pins the server to one node.** The volume is ReadWriteOnce, there is one replica and the strategy is Recreate. If that node is lost, the server is down until the volume is attached elsewhere.
- **Node agent traffic is plain `ws://` inside the cluster.** The token (audience `kubemap-portal`, one hour, bound to pod and node) and the TokenReview limit the risk; TLS inside the cluster is not done.
- **The NetworkPolicy covers ingress only.** Egress from the Kubemap pods is not restricted.
- **One cluster only.** Kubemap shows the cluster it runs in; several clusters would need a new design.
- **No registry.** The image is imported into k3s (`k3s ctr images import`) on every node that runs a Kubemap pod.
- **Two nodes are not tested on a real second node.** Several nodes are covered by tests and by design, but only a single-node cluster was run for real.
