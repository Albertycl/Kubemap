# Kubemap operator guide

This guide is for the person who runs Kubemap: install it, keep it up to date, fix it when something is down, and remove it. For what Kubemap shows and how to use the pages, see the [user guide](user-guide.md). Step-by-step install: [install.md](install.md). Design: [architecture-web.md](architecture-web.md).

Kubemap runs **inside the cluster it shows**:

- **The server**: Deployment `kubemap` in namespace `kubemap-system`, one pod. It serves the web UI on port 8080 (plain HTTP; Traefik ends TLS) and keeps its data on the volume `kubemap-data` (mounted at `/data`, files in `/data/kubemap`). The cluster agent runs inside it.
- **The node agents**: DaemonSet `kubemap-node-agent` in namespace `kubemap-node`, one pod on every node. They report to the server over the Service `kubemap` and are checked with a TokenReview.

Kubemap is **read-only by design**: it only reads the cluster (get, list, watch) and has no button or setting that changes anything in it. The only write-like right is the TokenReview that checks node agent tokens.

"Where kubectl works" means a shell where `kubectl` points at the cluster, with admin rights. The examples use the home lab host name `kubemap.albertycliu.xyz`.

---

## 1. Install

See [install.md](install.md). In short: `make image`, import the image into k3s, `deploy/install.sh --host HOST --image IMAGE --yes`, then set the admin password with `kubectl -n kubemap-system exec deploy/kubemap -- /usr/local/bin/kubemap-portal reset-admin-password`.

`make release VERSION=x.y.z` writes `dist/` (the image, the install manifest, `RELEASE.txt`, `SHA256SUMS`). Check with `sha256sum -c SHA256SUMS`.

## 2. Check that it works

```bash
kubectl -n kubemap-system get deploy,pods,pvc
kubectl -n kubemap-node get ds,pods -o wide
curl -s https://kubemap.albertycliu.xyz/healthz      # {"status":"ok"}
```

In the browser, the cluster is Active and every node shows its agent. If a node agent is missing, see section 4.

## 3. Upgrade and uninstall

**Upgrade.** Import the new image on every node, then run `deploy/install.sh --host HOST --image localhost/kubemap:<new> --yes`. The server restarts (it uses the strategy Recreate because the volume is ReadWriteOnce), the data stays on the volume, the node agents roll one by one and reconnect by themselves.

**Backup.** The data is the SQLite database on the volume. Scale the server to zero, copy the volume's folder on the node (`kubectl -n kubemap-system get pv` shows its path), scale back to one. Or copy while running: SQLite is in WAL mode, so copy `portal.db`, `portal.db-wal` and `portal.db-shm` together.

**Uninstall.** `deploy/uninstall.sh` lists what it would delete; `deploy/uninstall.sh --yes` deletes it and checks that nothing is left. This removes the namespaces, so the volume and the data go too.

---

## 4. Troubleshoot

| Symptom | Cause | Fix |
|---|---|---|
| Server pod `ErrImagePull` / `ImagePullBackOff` | The image is not on that node. | Import it (install.md, section 2). |
| Server pod `Pending` | The volume cannot be bound or attached. | `kubectl -n kubemap-system describe pod,pvc`. The volume pins the server to one node. |
| Page does not open, certificate error | The Certificate is not ready, or the host name does not point at Traefik. | `kubectl -n kubemap-system get certificate,ingressroute`. |
| Login works but you are logged out at once | The cookie is Secure and the browser uses plain HTTP. | Use the https address, or port-forward and `http://localhost:8080`. |
| Cluster shows Disconnected | The in-process agent lost the Kubernetes API. | `kubectl -n kubemap-system logs deploy/kubemap --tail=50`. |
| A node has no agent | The node agent pod is not running there, or cannot reach the server. | Below. |

**One node only.**

```bash
kubectl -n kubemap-node get pods -o wide                       # is there a pod on that node?
kubectl -n kubemap-node logs ds/kubemap-node-agent --tail=50   # or: logs <pod>
kubectl -n kubemap-system logs deploy/kubemap --tail=100 | grep -i 'node agent'
```

In the node agent log, `cannot connect` means the Service or a NetworkPolicy blocks the path; `registration refused` means the token review failed (the server log has the reason: wrong audience, wrong ServiceAccount or the node name differs). Too many registrations from one address are answered with `unavailable`, and the agent retries by itself. The page tells **node down** (Kubernetes says NotReady) from **agent down** (the node is Ready but its agent is silent).

**Server log lines.** `node agent connected` / `node agent disconnected`; `agent registration refused` (with `reason`, never a token); `node agent session ended: token expired` (normal, once an hour).

## Rotate credentials

| Credential | Where it lives | Rotation |
|---|---|---|
| Admin password | SQLite (hash only) | `kubectl -n kubemap-system exec deploy/kubemap -- /usr/local/bin/kubemap-portal reset-admin-password`. It prints a new password, ends all sessions and asks for a change at the next login. |
| Node agent token | Projected into each pod, valid an hour, refreshed by the kubelet | Automatic. |
| Server ServiceAccount token | Mounted into the server pod | Automatic. |

If the volume leaked: the database holds only password hashes, session-token hashes and the audit log. Reset the admin password, which ends all sessions.

---

## 5. Everyday commands

| Task | Command |
|---|---|
| Pods | `kubectl -n kubemap-system get pods; kubectl -n kubemap-node get pods -o wide` |
| Server log | `kubectl -n kubemap-system logs deploy/kubemap --tail=50` |
| Node agent log | `kubectl -n kubemap-node logs ds/kubemap-node-agent --tail=50` |
| Server version | `kubectl -n kubemap-system exec deploy/kubemap -- /usr/local/bin/kubemap-portal version` |
| Reach it without the ingress | `kubectl -n kubemap-system port-forward svc/kubemap 8080:8080` |
| New admin password | `kubectl -n kubemap-system exec deploy/kubemap -- /usr/local/bin/kubemap-portal reset-admin-password` |
| Import incident notes into the journal | Use the Journal page (Import Markdown); the image has no shell to copy files in. |
