# Architecture: the web version in the cluster

Kubemap is a web page that runs in the cluster it shows. This replaces the earlier design (a Windows service plus agents that dial out and register with import tokens and manual Accept).

## Parts

| Part | Where | What it does |
|---|---|---|
| Browser | the user's PC | Opens `https://<host>/`, logs in as `admin`. |
| Traefik IngressRoute | kube-system | Ends TLS (Certificate from cert-manager, default host `kubemap.albertycliu.xyz`, ClusterIssuer `letsencrypt-prod`) and forwards to the Service on port 8080. Sets `X-Forwarded-Proto` and `X-Forwarded-For`. Does not route `/v1/connect` (node agents use the Service directly). |
| Service `kubemap` | kubemap-system | Cluster-internal address of the server, port 8080. |
| Server `kubemap` (Deployment, 1 replica, Recreate) | kubemap-system | Serves the embedded React UI and `/api` over plain HTTP; login, sessions, audit log, incident journal, drafter, alerts. |
| In-process cluster agent | inside the server | Reads the cluster through the Kubernetes API (informers, get, list, logs, TokenReview) Same request/response messages as before (`pkg/protocol`), but passed as function calls (`Hub.AttachLocal`). |
| ServiceAccount `kubemap` | kubemap-system | The server's identity. ClusterRoles `kubemap-readonly` and `kubemap-tokenreview`. |
| PVC `kubemap-data` | kubemap-system | 1 GiB ReadWriteOnce, mounted at `/data`, files in `/data/kubemap`: SQLite database (`portal.db`). |
| Node agent (DaemonSet `kubemap-node-agent`) | kubemap-node, one pod per node | Measures disk, memory, swap, PSI and kubelet state of its node and sends metrics. |
| Kubernetes API server | control plane | Answers the in-process agent; checks node agent tokens (TokenReview). |

## Flows

1. The browser calls the IngressRoute over HTTPS; Traefik forwards plain HTTP to the Service, which reaches the server.
2. At start the server creates the cluster record `local` ("this cluster") as Active and attaches the in-process agent. No registration, token or Accept exists.
3. A page asks the server; the server asks the in-process agent; the agent reads the Kubernetes API (or its cache) and answers.
4. Each node agent opens `ws://kubemap.kubemap-system.svc:8080/v1/connect` and sends `register` with its projected ServiceAccount token (audience `kubemap-portal`, bound to its pod and node).
5. The server asks the in-process agent for a TokenReview, then checks audience, ServiceAccount and node name (`AcceptNode`). Only then does the node count as connected.
6. The node agent sends metrics and heartbeats every 10 s. The server stores node status and metrics in SQLite. After 30 s of silence the node agent is "disconnected".
7. Sessions last until the token expires (at most one hour); the agent then reconnects with a fresh token.

## Several nodes

Nodes are tracked by cluster and node name (`nodeKey{cluster,node}`); nothing is tied to one node. Per node, Kubemap tells apart "node down" (Kubernetes says NotReady) from "agent down" (the node is Ready but its agent is silent) and "no agent" (never seen). Tests: `portal/metrics/multinode_test.go` and `portal/localcluster/localcluster_test.go`.

## Security

- Admin login with Argon2/bcrypt-hashed password, 12-hour idle sessions, brute-force limits, audit log (unchanged).
- Optional TOTP two-factor login (RFC 6238, standard library only): enrol from the account page (`#/security`), secret encrypted in SQLite, 8 hashed recovery codes, replay protection per time step, same login limiter, `kubemap-portal disable-totp` for lockout. `/healthz` returns only `{"status":"ok"}`.
- The session cookie is `Secure` unless the request carries `X-Forwarded-Proto: http`.
- The server's ServiceAccount is read-only plus TokenReview. Kubemap is read-only by design: it has no write path to the Kubernetes API.
- A node agent token is accepted only with audience `kubemap-portal`, the node agent ServiceAccount and a matching node name. Registrations are rate-limited.
- NetworkPolicies (ingress only) allow the server's port from Traefik and from the node agent namespace.

## Known limits

- **Read-only by design.** Kubemap cannot change the cluster: no fix buttons, no operator mode. This is a choice, not a missing feature, because the site is public.
- The PVC (ReadWriteOnce, one replica) pins the server to one node. If that node is lost, the server is down until the volume is attached elsewhere.
- The node agents' WebSocket is plain HTTP inside the cluster. The short-lived audience-bound token and the NetworkPolicy limit the risk; TLS inside the cluster is not done.
- The NetworkPolicy covers ingress only.
- Single cluster only. Several clusters would need a design of their own.
- The database keeps the old registration tables and some unused store code; migrations are checksummed, so they stay.

## Removed

The Windows service and `.exe`, the portal CA and certificate (`tlsca`), `/v1/ca.crt`, `/v1/import.yaml` and `/v1/agent-bundle.yaml`, import tokens, cluster credentials, Pending/Accept/Reject, Rotate credential, Remove cluster, the separate cluster-agent Deployment and its credential Secret, and the "Add cluster" pages.
