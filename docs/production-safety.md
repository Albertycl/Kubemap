# Kubemap in a production cluster: the safety case

This page is for the person who decides whether Kubemap may be installed in a production cluster. It says exactly what the install creates, what Kubemap may and may not do, how to prove that on the real cluster before and after the install, and how to remove everything again. The last section is a change request you can fill in.

Every fact here comes from the code: the install manifest (`deploy/kubemap.yaml`, made by `agent/install`), the permission check list (`scripts/agent-permission-checks.txt`), and the scripts named below. If the code changes, this page must change with it.

**In one paragraph:**
- Kubemap installs one small web server pod and one small node agent per node. They **read** the cluster. The only write-like right is to **check node agent tokens** (TokenReview, which stores nothing).
- The server opens **one port (8080) inside the cluster**, behind Traefik, which ends TLS. The node agents connect to it **inside the cluster**. Nothing connects out of the cluster.
- **They cannot read Secrets** or **get into containers or nodes**.
- Kubemap is **read-only by design**. There are no fix buttons and no operator mode: no code path and no right can change a workload, a pod or a node.
- One script proves the permissions on the live cluster without changing anything. Another script removes everything.

---

## 1. What the install creates, and nothing else

`deploy/install.sh` applies `deploy/kubemap.yaml` (**12 objects**), then two optional files. Every object carries the label `app.kubernetes.io/managed-by=kubemap` (the optional files carry `app.kubernetes.io/part-of=kubemap`).

| # | Kind | Namespace | Name | Why |
|---|---|---|---|---|
| 1 | Namespace | – | `kubemap-system` | The server. Pod Security **`restricted`** is enforced. |
| 2 | Namespace | – | `kubemap-node` | The node agent. Pod Security **`privileged`**, because it needs `hostPID` and read-only host mounts; see section 4. |
| 3 | ServiceAccount | kubemap-system | `kubemap` | The server's identity (and that of the in-process cluster agent). |
| 4 | ClusterRole | – | `kubemap-readonly` | Read-only rules; section 2. |
| 5 | ClusterRoleBinding | – | `kubemap-readonly` | Binds 4 to 3. |
| 6 | ClusterRole | – | `kubemap-tokenreview` | `create tokenreviews` only; section 2. |
| 7 | ClusterRoleBinding | – | `kubemap-tokenreview` | Binds 6 to 3. |
| 8 | PersistentVolumeClaim | kubemap-system | `kubemap-data` | 1 GiB, ReadWriteOnce: the SQLite database. |
| 9 | Deployment | kubemap-system | `kubemap` | The server, 1 replica, strategy Recreate. |
| 10 | Service | kubemap-system | `kubemap` | ClusterIP, port 8080. |
| 11 | ServiceAccount | kubemap-node | `kubemap-node-agent` | The node agent's identity. It has **no RBAC at all**. |
| 12 | DaemonSet | kubemap-node | `kubemap-node-agent` | The node agent, one pod per node. |

**Optional files** (skip them with `--no-netpol` and `--no-ingress`):
- `deploy/networkpolicy.yaml`: default-deny ingress in both namespaces, then allow Traefik and the node agents to reach port 8080 of the server.
- `deploy/ingress.yaml`: a cert-manager Certificate, a Traefik IngressRoute on `websecure` (it does not route `/v1/connect`: node agents use the Service inside the cluster), an http-to-https redirect (a Middleware and an IngressRoute).

**Not created:**
- no CustomResourceDefinition, no webhook;
- no write rights for workloads, pods or nodes (a test fails if one appears in `deploy/*.yaml`);
- nothing in any other namespace.

**Requirements:** Kubernetes **1.30 or newer** (the node name that the TokenReview returns for a node agent token needs it); a StorageClass for the volume. One image, `localhost/kubemap:<version>` (imported on each node while there is no registry), with `imagePullPolicy: IfNotPresent`.

**To see it yourself before applying:** `deploy/install.sh --host HOST` without `--yes` only lists the objects. `scripts/verify-agent-permissions.sh --static deploy/kubemap.yaml` checks its RBAC offline (section 3).

---

## 2. The accounts, and every permission with its reason

### The two accounts

| Account | What runs as it | Container user | Kubernetes token in the pod |
|---|---|---|---|
| `kubemap-system/kubemap` | the server and the in-process cluster agent (`kubemap-portal`), 1 pod | UID/GID **65532**, non-root | yes: it reads the cluster through the API |
| `kubemap-node/kubemap-node-agent` | the node agent (`kubemap-agent node`), 1 pod per node | UID/GID **65533**, non-root | **no** (`automountServiceAccountToken: false`). It gets only a projected token for the audience `kubemap-portal`, valid for 1 hour, which proves to the server which node it runs on. This token can't be used against the Kubernetes API. |

### Every rule

**`ClusterRole kubemap-readonly`** (the server). **Only `get`, `list` and `watch`.** The resources are listed one by one, with no wildcard, because RBAC can't take one resource away from a wildcard.

| API group | Resources | Why |
|---|---|---|
| core | `pods`, `pods/log`, `nodes`, `namespaces`, `services`, `configmaps`, `events`, `persistentvolumeclaims`, `persistentvolumes`, `serviceaccounts` | the cluster map, pod health, logs for debugging, and dependencies named in ConfigMaps |
| apps | `deployments`, `statefulsets`, `daemonsets`, `replicasets` | workloads and their rollouts |
| batch | `jobs`, `cronjobs` | jobs |
| autoscaling | `horizontalpodautoscalers` | scaling state |
| discovery.k8s.io | `endpointslices` | whether a Service has ready endpoints (503 debugging) |
| networking.k8s.io | `ingresses`, `ingressclasses`, `networkpolicies` | routes, and the NetworkPolicy checker |
| policy | `poddisruptionbudgets` | disruption budgets |
| rbac.authorization.k8s.io | `roles`, `rolebindings`, `clusterroles`, `clusterrolebindings` | the "who can do what" view |
| argoproj.io | `applications`, `appprojects` | the Argo CD view, and sync windows |
| traefik.io, traefik.containo.us | `ingressroutes` | Traefik routes |
| postgresql.cnpg.io | `clusters`, `poolers` | CloudNativePG databases on the app map |
| metrics.k8s.io | `pods` (**`get`, `list` only**) | CPU and memory use per pod |

Custom resources that a cluster doesn't have are simply skipped.

**The one write-like right, and nothing else:**

| Object | Rule | Why |
|---|---|---|
| `ClusterRole kubemap-tokenreview` | `create` on **tokenreviews** | When a node agent connects, the server asks its in-process agent to check that node agent's token with the Kubernetes TokenReview API. A TokenReview is not stored and changes nothing in the cluster. |

The server has **no Secret rights at all**, not even for its own. Its only state is the SQLite file on the volume.

**What read access still shows:** pod logs and ConfigMaps may contain sensitive values an application put there. Kubemap shows them to its logged-in users, like `kubectl logs` would. Kubemap itself never reads Secrets other than its own.

---

## 3. What is refused, and how to prove it on the production cluster

### What the proof checks

`scripts/verify-agent-permissions.sh` checks the **78 lines** of `scripts/agent-permission-checks.txt`, all of which must be refused. Live, it asks them for each account, in every namespace and in a few sample namespaces, so a run prints several hundred answers. The lines:

- **Secrets:**
  - no `get`, `list` or `watch` of Secrets anywhere;
  - no Secret in kubemap-system, not even by name;
  - the node agent can't read any Secret.
- **Getting into containers and nodes:**
  - no `pods/exec`, `pods/attach` or `pods/portforward`;
  - no `nodes/proxy`, `nodes/stats` or `nodes/log`, so no kubelet access at all;
  - no `serviceaccounts/token`;
  - no `pods/proxy` or `services/proxy`.
- **Any write:** no `create`, `update`, `patch`, `delete` or `deletecollection` on workloads, pods, Services, ConfigMaps, namespaces, nodes and the other core objects, in any namespace.
- **Taking more rights:** no `impersonate`; no `bind` or `escalate`; no writes to Roles, ClusterRoles or their bindings.
- **Changing how the cluster admits or runs things:** no writes to webhook configurations or admission policies, CRDs, and similar.

It also compares each account's full `kubectl auth can-i --list` with that of a ServiceAccount that doesn't exist, which has only what every account inherits. **Any extra write verb must be the TokenReview.** The static mode (`--static deploy/kubemap.yaml`) passes on this branch.

### How to run it in production

It is read-only: it runs `kubectl auth can-i --as=…`, which creates nothing. Your kubeconfig user needs the `impersonate` right, which cluster admins have.

```bash
# 1. Before the install: what the cluster's existing bindings would already
#    give the Kubemap accounts.
scripts/verify-agent-permissions.sh --before-install --context <prod-context>

# 2. Offline, on the exact file you are about to apply (no cluster needed).
scripts/verify-agent-permissions.sh --static deploy/kubemap.yaml

# 3. After the install: the real answers of the API server.
scripts/verify-agent-permissions.sh --context <prod-context>
```

The exit status is 0 when every check passed, 1 when one failed, and 2 when it could not run. Keep the output as test evidence (section 7).

**Why step 1 matters:** a cluster may already grant things to **every** ServiceAccount, for example through a binding to the group `system:serviceaccounts`. Such rights are the cluster's, not Kubemap's, and step 1 shows them before you install.

---

## 4. Pod security, resources and network

### Pod security

| | Server | Node agent |
|---|---|---|
| Namespace Pod Security | `restricted` (enforced) | `privileged` (needed for the settings below) |
| User | non-root, 65532 | non-root, 65533 |
| Privileged | no | **no** |
| Privilege escalation | not allowed | not allowed |
| Capabilities | all dropped | all dropped |
| Root filesystem | read-only (writable `/data` volume and a 64 MiB `/tmp`) | read-only |
| seccomp | RuntimeDefault | RuntimeDefault |
| Host access | none | `hostPID` (to see node processes), and host **`/`, `/proc`, `/sys` mounted read-only**, recursively read-only where the runtime supports it (`recursiveReadOnly: IfPossible`). No host network. |
| Tolerations | none | all (`operator: Exists`), so it runs on every node, including control-plane nodes |

**The image** is built on `gcr.io/distroless/static:nonroot` (pinned by digest): no shell and no package manager. It holds two static binaries, `kubemap-portal` and `kubemap-agent`.

### Resources

| | CPU request | Memory request | Memory limit |
|---|---|---|---|
| Server (1 pod) | 100m | 128 MiB | 384 MiB |
| Node agent (per node) | 50m | 32 MiB | 64 MiB |

No CPU limit is set, so nothing is throttled into timeouts. Requests reserve what they normally use.

### Network

| From | To | Why |
|---|---|---|
| Traefik (kube-system) | the server, port 8080 (**inbound**) | the web page; Traefik ends TLS |
| node agents | the server, `ws://kubemap.kubemap-system.svc:8080/v1/connect` (**inbound** to the server, inside the cluster) | node metrics and heartbeats |
| the server | the **Kubernetes API server** (in-cluster) | the reads in section 2 |
| the server | **Tempo**, when the cluster has one (a Service `tempo` with port 3200), over HTTP GET searches | "confirmed by traces" on the app map. Set `KUBEMAP_TEMPO_URL=off` in the server's environment to turn it off. |

- **Nothing leaves the cluster.** The server and the agents make no connection to the outside.
- **The node agent connection is plain HTTP inside the cluster.** The token it sends has audience `kubemap-portal`, lives one hour and is bound to the pod and node, and the server checks it with a TokenReview and rate-limits registrations. The NetworkPolicy lets only Traefik and the node agents reach port 8080. TLS inside the cluster is not done.
- **The NetworkPolicy covers ingress only.** It is not applied with `--no-netpol`. If your production cluster uses default-deny egress, allow the node agents to reach the server's Service and the server to reach the API server; see section 7.

---

## 5. Read-only by design

Kubemap has **no write path** to the Kubernetes API. The install contains no `create`, `update`, `patch`, `delete` or `deletecollection` right (except `create` on `tokenreviews`), no Secret, no exec and no wildcard. There are no fix buttons, no operator mode, no `kubemap-operator` role and no admission policies. Fixes are made by a person with `kubectl` or, for an Argo CD app, in Git.

The test `TestDeployManifestsAreReadOnly` (package `agent/install`) reads every `deploy/*.yaml` and fails if a write verb, a wildcard, a Secret or exec right, an admission policy or an operator binding appears.

What Kubemap does write is its own data on its own volume: users, sessions, the audit log, the incident journal and TOTP secrets in SQLite.

---

## 6. Rollback

```bash
deploy/uninstall.sh --context <prod-context>          # shows what would be deleted (server dry run); changes nothing
deploy/uninstall.sh --yes --context <prod-context>    # deletes it, then proves nothing is left
deploy/uninstall.sh --check --context <prod-context>  # only the proof
```

**It removes, in this order:**
1. the ClusterRoleBindings and ClusterRoles `kubemap-readonly` and `kubemap-tokenreview`;
2. the namespaces `kubemap-system` and `kubemap-node`, with everything in them (the server, the node agents, the ServiceAccounts, the Service, the volume with the database, the IngressRoutes, the Certificate and the NetworkPolicies).

**What it will not touch:** an object is deleted only if it has one of these names **and** the label `app.kubernetes.io/managed-by=kubemap`. A same-named object without the label is left alone and reported.

**The proof at the end:** it lists every object with the Kubemap label in the whole cluster, asking each kind on its own. Exit 0 means **nothing of Kubemap is left**. If it couldn't list a kind, it says so and fails, rather than passing without having looked.

**Data:** the database is on the volume in `kubemap-system`, so uninstalling deletes the audit log and the incident journal. Export the journal first if you want to keep it.

**Time to roll back:** under a minute. Nothing else in the cluster depends on Kubemap.

---

## 7. Change request

Copy this, fill in the parts in `<…>`, and attach the evidence.

```text
CHANGE REQUEST: install Kubemap (read-only) in <cluster name>

What:     Install the Kubemap server (1 pod, with a 1 GiB volume) and the node
          agent (1 pod per node), version <x.y.z>, with deploy/install.sh.
          12 objects: 2 namespaces (kubemap-system, kubemap-node), 2 ServiceAccounts,
          2 cluster roles with bindings, 1 PVC, 1 Deployment, 1 Service, 1 DaemonSet
          plus NetworkPolicies and a Certificate with IngressRoutes at <host name>.
Why:      <what the team gets: application map, guided debugging, node pressure, ...>
When:     <date and time, time zone>          Owner: <name>          Approver: <name>

Risk:     Low.
  - Read-only: get/list/watch only. One write-like right: create tokenreviews,
    which is stored nowhere.
  - No Secret reading, no exec, no port-forward, no node proxy, no kubelet access.
  - Web access only through Traefik with TLS, behind a login (admin password,
    12-hour sessions, brute-force limits, audit log). Ingress NetworkPolicy
    limits who reaches the server's port 8080.
  - Node agents talk to the server inside the cluster over plain HTTP, with a
    1-hour audience-bound token checked by TokenReview.
  - Read-only by design: there is no operator mode and no write right.
  - Resources: 100m CPU and 128 MiB (server, limit 384 MiB), plus 50m and 32 MiB
    per node (limit 64 MiB).
  - Node agent: privileged namespace for hostPID and read-only host mounts;
    the container itself is not privileged, runs as non-root and drops all capabilities.
  - The server's volume is ReadWriteOnce, so the server runs on one node only.
  - Known exposure: logged-in Kubemap users can read pod logs and ConfigMaps.
  - Needs: Kubernetes 1.30+; a StorageClass; Traefik and cert-manager for the web address.

Test evidence (home lab, <cluster>, Kubernetes <version>):
  - Before install, on production: scripts/verify-agent-permissions.sh --before-install    -> <attach output>
  - Offline, on the exact file: scripts/verify-agent-permissions.sh --static deploy/kubemap.yaml -> RESULT: PASS <attach>
  - After install, on production: scripts/verify-agent-permissions.sh                        -> <attach output>

Rollback (under a minute, no dependencies):
  deploy/uninstall.sh --yes --context <prod-context>
  It deletes only objects labelled app.kubernetes.io/managed-by=kubemap, and ends
  with a check that nothing of Kubemap is left (exit 0). The database goes with it.

Verification after the change:
  - The cluster is Active in the web page, and its nodes report.
  - verify-agent-permissions.sh passes (above).
  - kubectl -n kubemap-system get pods; kubectl -n kubemap-node get pods: Running.
```
