# Kubemap architecture

> **Superseded.** This is the original Windows-service design (kept as history). The current design is in docs/architecture-web.md.

Status: approved design, 2026-09-27 (decisions A2–A9). All decisions approved (A1–A10). Portal host: Windows 11, 192.168.32.32.

## Terms

- **Portal:** the Go web server on the Windows server. It is the UI and the control point for all clusters.
- **Cluster agent:** one Deployment per cluster. It holds the only connection to the portal.
- **Node agent:** a DaemonSet with one pod per node. It collects node performance data.
- **Worker agents:** the AI coding agents that build this project. They are not part of the product.

## Overview

```
 Windows server                              Kubernetes cluster (1 node today, 3 later)
┌──────────────────────────┐               ┌────────────────────────────────────────────┐
│ kubemap-portal.exe       │   wss :8443   │ node 1 (k8server)                          │
│  (Windows service)       │◀──────────────┤  cluster-agent (Deployment, 1 per cluster) │
│  • web UI (embedded)     │◀──────────────┤  node-agent    (DaemonSet)                 │
│  • tunnel endpoint       │               ├────────────────────────────────────────────┤
│  • SQLite: clusters,     │◀──────────────┤ node 2   node-agent                        │
│    nodes, users, tokens, │               ├────────────────────────────────────────────┤
│    audit, metrics        │◀──────────────┤ node 3   node-agent                        │
└──────────────────────────┘  every agent  └────────────────────────────────────────────┘
        ▲ browser             dials OUT
```

## Key decisions

1. **The agent connects out to the portal.** The portal never connects in to a cluster. So clusters behind NAT or firewalls work, the portal stores no kubeconfigs, and only one port (8443) on the Windows server must be open, reachable from every node. Rancher uses the same pattern.
2. **Every node agent registers with the portal on its own.** The cluster agent carries cluster data (objects, logs, commands). Each node agent carries that node's health. So a node's status reaches the portal even when the node running the cluster agent is down, and the portal can tell "node down" from "agent down". A node added later gets a node agent from the DaemonSet and registers by itself. (Changed 2026-09-25 when the plan grew to 3 nodes.)
3. **Data stays in the cluster.** The cluster agent keeps the full object cache. It pushes small summaries (health, counts, node metrics) every few seconds, and the portal requests details on demand through the tunnel.
4. **Protocol v1 is WebSocket + JSON.** Every message has an envelope `{v, type, id, payload}`. Types: `register`, `heartbeat`, `summary`, `metrics`, `request`, `response`, `stream.open/data/close`, `error`. Heartbeat every 10 s; the portal marks a cluster Disconnected after 30 s without one. The agent reconnects with exponential backoff.
5. **The portal is one Go binary.** It is built for `windows/amd64`, has the UI embedded, uses SQLite (pure Go, no CGO) and runs as a Windows service. During development it also runs on Linux.
6. **The agent is one Go binary with two modes.** `kubemap-agent cluster` and `kubemap-agent node`. It ships as one container image.

## Registration flow (A8, A10)

1. In the portal, **Add cluster** creates a cluster record and a **one-time import token**, valid for 1 hour, stored only as a hash, and never written to logs.
2. The portal shows an import command that downloads the manifest over HTTPS while **checking the portal CA checksum** shown next to it, then applies it. The manifest contains the namespaces, ServiceAccounts, RBAC, Deployment, DaemonSet, and a Secret with the portal URL, the token and the CA checksum.
3. The cluster agent dials `wss://<portal>:8443/v1/connect`, pins the CA checksum and presents the token.
4. The portal marks the cluster **Pending** (A10). When you click **Accept**, it issues a long-lived per-cluster credential. The agent saves that credential in its own Secret, and the cluster becomes **Active**.
5. **Node agents** prove their identity with a short-lived **projected ServiceAccount token** (audience `kubemap-portal`) bound to their pod and node. The portal asks the cluster agent to check it with a `TokenReview`. There is no shared password, and tokens rotate automatically (KM-68).

## Security (A3, A4, A7)

- **Read-only by design.** Kubemap watches and advises. It has no action that changes the cluster (no scale, restart, delete or cordon), so the public site cannot be used to change it. Fixes are made with kubectl or in Git.
- **Cluster agent RBAC:** only `get`, `list` and `watch`, generated from API discovery as an explicit resource list with no wildcards. Includes `pods/log` and read-only `pods.metrics.k8s.io` / `nodes.metrics.k8s.io` (approved 2026-09-28). The cluster agent makes **no kubelet calls**: node pressure comes from the node agents, so the agent's cluster-wide read token is never sent to a node (review KM-60, approved 2026-09-28); `nodes/stats` is removed. (Earlier versions also watched admission policies and Argo CD AppProjects for operator mode; both were removed with operator mode.) Excludes Secrets, exec, port-forward and `nodes/proxy`. A separate namespaced Role allows get and update on **its own credential Secret only**. A second narrow ClusterRole allows `create` on `tokenreviews` only, so the cluster agent can check node agent tokens (A8). TokenReviews are not stored and change nothing in the cluster (approved 2026-09-28). Operator mode adds a second ClusterRole with the action verbs.
- **Pods:** non-root UID 65532, read-only root filesystem, no privilege escalation, all capabilities dropped. `kubemap-system` enforces Pod Security `restricted`.
- **Node agent (A7):** a DaemonSet in its own namespace, **`kubemap-node`**, at Pod Security level `privileged`, because both `restricted` and `baseline` block these settings. It uses `hostPID: true` and read-only hostPath mounts of `/proc`, `/sys` and `/`. It is still non-root, with `privileged: false` and no capabilities.
- **Portal:** creates its own CA on first run, requires an admin login, and stores tokens and credentials only as hashes.
- **Known exposure:** read-only access still shows pod logs and ConfigMaps, which can contain sensitive values.

## Demo namespace (A9)

`kubemap-demo` holds tiny workloads that fail on purpose, for the interview demo: OOMKilled, CrashLoopBackOff, a NetworkPolicy that blocks frontend → backend, a Service whose selector matches no pods, and a Pending pod. They use under 100 MiB in total, and one command creates, resets or deletes them. It is the only namespace Kubemap's builders may change.

## Node agent budget

`k8server` runs at about 86% memory (1.2 GiB available of 12 GiB). The node agent gets a 64 MiB memory limit and a 50m CPU request. It reads host `/proc` and `/sys` through read-only mounts and has no other privileges.

## Application map (how the agent finds frontend → backend → database)

The agent combines several signals, because no single Kubernetes object describes an application:

| Signal | Example from this cluster |
|---|---|
| Argo CD app and `app.kubernetes.io` labels group workloads into an app | `flask-vue` = frontend + backend Deployments, Services, IngressRoute |
| Ingress / Traefik IngressRoute rules give the entry point and path routing | `flask-vue.albertycliu.xyz` `/api` → backend, `/` → frontend |
| Env vars and ConfigMaps that name a Service reveal dependencies | `url-shortener` → `DB_HOST=url-shortener-pooler`, `REDIS_HOST=redis-master` |
| Operator CRDs identify databases | CNPG Pooler `url-shortener-pooler` → Cluster `url-shortener-db` (2 instances) |
| Known ports mark databases and caches | 5432 Postgres, 6379 Redis |
| Later: OTel traces in Tempo confirm real traffic | `flask-vue-backend` already sends traces |

## Production read-only access

- Identity: `system:serviceaccount:kubemap-system:kubemap-agent`.
- ClusterRole `kubemap-agent-readonly` (draft: `deploy/rbac-readonly.yaml`): only `get`, `list` and `watch`, including `pods/log`. It excludes Secrets, exec, port-forward, `nodes/proxy`, and every write verb. Core resources are listed one by one, because RBAC cannot deny a single resource.
- Because the limit is enforced by the Kubernetes API server, the agent stays read-only even if the portal is compromised.
- The container runs as non-root UID 65532 with a read-only root filesystem and all capabilities dropped. The namespace enforces Pod Security `restricted`.
- Check with: `kubectl auth can-i --list --as=system:serviceaccount:kubemap-system:kubemap-agent`.
