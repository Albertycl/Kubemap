# Kubemap tunnel protocol v1

Go types: `pkg/protocol`. One golden example per message: `pkg/protocol/testdata/*.json`.
A test (`TestDocsMatchTypes`) fails if this file and the Go types disagree on message types or top-level payload fields.

## Transport

- The cluster agent runs **inside the server process** and speaks no network protocol: the server calls it directly with the same `request`, `response`, `summary` and `stream.*` messages (see "In-process cluster agent" below). Only **node agents** use the network: each dials out to `ws://kubemap.kubemap-system.svc:8080/v1/connect` (the cluster-internal Service; TLS for browsers ends at Traefik, not here) and holds its own connection.
- One WebSocket **text** frame carries one JSON message.
- Maximum message size is **1 MiB** (`MaxMessageSize`), envelope included. Both sides refuse larger messages with code `too_large`.
- Log bytes (`stream.data.data`, `response.logs`) are **base64** on the wire and at most **512 KiB** raw (`MaxStreamChunk`, `MaxLogBytes`). Any content, including ANSI colour codes, HTML and invalid UTF-8, fits in one message and arrives byte for byte.
- **Intervals:** `heartbeat` every **10 s** from both sides, `metrics` every 10 s (`MetricsInterval`), `summary` every 5 s (`SummaryInterval`). A side that gets no heartbeat for **30 s** counts the peer as Disconnected and closes the connection. The agent then reconnects with exponential backoff (KM-21).

### Rules for the transport (KM-18 portal, KM-21 agent)

1. Set the WebSocket read limit to `MaxMessageSize` **before** reading, so an oversized frame is refused without being buffered. Turn permessage-deflate off (no compression bombs).
2. The first message must be `register`, within **10 s** of the connection opening; otherwise close. Until it has sent `register.ack`, the portal answers anything else with `error` (`unauthorized`) and closes.
3. A node agent sends `register` once; a second `register` on the same connection gets `error` (`bad_request`).
4. **Ids** are unique per connection and sender. The portal's request and stream ids share one space (for example `q-<n>`, `s-<n>`); the agent's register id is its own.
5. Heartbeats go ahead of `stream.data` in the send queue, so a busy log stream never causes a false Disconnected. The agent keeps at most 8 unsent `stream.data` chunks per stream; when the queue is full it closes the stream with `error` code `unavailable` ("reader too slow").
6. A node agent's projected token expires (default 1 h) but its connection does not. The node agent re-registers on a new connection before the token expires (KM-68).
7. Never log a raw message or envelope. The Go types' `LogValue` methods leave out payloads and redact tokens.
8. `error` messages from the portal carry no internal details (paths, SQL); they say what was wrong with the message.
9. List responses are paged: the agent uses a default page size (100) and returns fewer items plus `continue` when a page would exceed 1 MiB. `limit` may not exceed 500 (`MaxListLimit`).

## Envelope

```json
{"v": 1, "type": "request", "id": "q-1", "payload": { }}
```

| Field | Type | Rule |
|---|---|---|
| `v` | int | Always `1`. Any other value is rejected (`unsupported`). |
| `type` | string | One of the message types below. Missing → `bad_request`. Unknown → `unsupported`. |
| `id` | string | Required for `register`, `register.ack`, `request`, `response` and all `stream.*` messages. Optional elsewhere. |
| `payload` | object | Required. Its shape depends on `type`. |

Unknown fields are ignored, so a v1 peer may add **optional** fields. A breaking change needs `v: 2`.

An unknown `type` is never fatal: the receiver answers with an `error` message (code `unsupported`) that carries the same `id`, and keeps the connection open.

## Messages

| Type | Direction | `id` | Purpose |
|---|---|---|---|
| `register` | node agent → server | required | First message. Projected ServiceAccount token and node name. |
| `register.ack` | server → node agent | same as `register` | Status `active`. |
| `heartbeat` | both | – | Liveness, every 10 s. |
| `summary` | cluster agent → portal | – | Small cluster state: counts and current problems. |
| `metrics` | node agent → portal | – | One node's CPU, memory, swap, load, PSI, filesystems, network and top processes. |
| `request` | portal → cluster agent | required | `list`, `get`, `logs`, `tokenreview`, `graph`, `appmap`, `netpol.check`, `pod.diagnose`, `pod.debug`, `service.check`, the other read ops (every op is a read: Kubemap is read-only by design; there is no op that changes the cluster). |
| `response` | cluster agent → portal | same as `request` | The result, or an error with a code. |
| `stream.open` | portal → cluster agent | new stream id | Start following pod logs. |
| `stream.data` | cluster agent → portal | stream id | One chunk of the stream. |
| `stream.close` | both | stream id | End or cancel the stream. |
| `error` | both | id of the message that failed, if known | A message could not be handled. |

### `register`

| Field | Type | Who | Notes |
|---|---|---|---|
| `role` | string | all | `node`. Any other role is refused with `bad_request`: the cluster agent does not register. |
| `agentVersion` | string | all | Required. |
| `clusterId` | string | node | Required. The server's ID for the cluster (the Deployment passes it to the DaemonSet as `KUBEMAP_CLUSTER_ID`). |
| `nodeName` | string | node | Required. From the downward API (`spec.nodeName`), never fixed. |
| `serviceAccountToken` | string | node | Projected token, audience `kubemap-portal`, bound to the pod and node. |

The Go types redact the token when logged with `slog`.

### `register.ack`

| Field | Type | Notes |
|---|---|---|
| `clusterId` | string | Required. |
| `status` | string | `active`. |

A rejected registration gets an `error` with code `unauthorized`, and the portal closes the connection.

### `heartbeat`

| Field | Type | Notes |
|---|---|---|
| `time` | RFC 3339 time | Sender's clock. Required. |

### `summary`

| Field | Type | Notes |
|---|---|---|
| `time` | RFC 3339 time | Required. |
| `kubernetesVersion` | string | |
| `nodes` | object | `total`, `ready`. |
| `nodeList` | array | Optional, at most 500: `name`, `ready` (Kubernetes Ready condition) per node. |
| `namespaces` | int | |
| `pods` | object | `total`, `running`, `pending`, `succeeded`, `failed`, `unknown`. |
| `workloads` | object | `deployments`, `deploymentsDegraded`, `statefulSets`, `daemonSets`. |
| `problems` | array | At most 50, worst first. Each: `kind`, `namespace`, `name`, `reason` (for example `CrashLoopBackOff`, `OOMKilled`, `Unschedulable`), `message`, `since`. |

### `metrics`

Byte values are bytes. Percentages are 0–100.

| Field | Type | Notes |
|---|---|---|
| `time` | RFC 3339 time | Required. |
| `nodeName` | string | Required. |
| `cpu` | object | `cores`, `usagePercent`. |
| `memory` | object | `totalBytes`, `availableBytes`. |
| `swap` | object | `totalBytes`, `usedBytes`. |
| `load` | object | `load1`, `load5`, `load15`. |
| `psi` | object | Pressure stall info: `cpu`, `memory`, `io`, each `{some, full}` with `avg10`, `avg60`, `avg300`. `full` may be absent. The whole field is absent if the kernel has no PSI. |
| `filesystems` | array | `mountPoint`, `device`, `fsType`, `totalBytes`, `usedBytes` (total − free), `availableBytes` (free to non-root; excludes reserved blocks), `inodesTotal`, `inodesFree`. Used % = `usedBytes / totalBytes`. |
| `network` | array | `interface`, `rxBytes`, `txBytes`, `rxErrors`, `txErrors` (counters since boot). |
| `topProcesses` | array | At most 20: the top 10 by CPU plus the top 10 by memory. Each: `pid`, `command` (short kernel name; never the command line), `cpuPercent` (share of the whole node since the last sample), `rssBytes`, `origin` (`pod`, `container`, `service`, `user`, `kernel`, `host`), `containerId`, `unit` (systemd unit), and for pods `podUid`, plus `podNamespace`, `podName`, `container` once the portal has mapped the UID. |

### `request`

| Field | Type | Used by | Notes |
|---|---|---|---|
| `op` | string | all | `list`, `get`, `logs`, `tokenreview`, `graph`, `appmap`, `netpol.check`, `pod.diagnose`, `pod.debug`, `node.pressure`, `service.check`, `health.check`, `pod.usage`, `rbac.whocan`, `logs.search`, `object.chain`. Unknown → `unsupported`. `namespace` and `name` may not contain `/` or be `.`/`..`. `group` must be empty or a lowercase DNS subdomain, `version` like `v1`, `v1beta1` or `v2alpha1`, and `resource` a lowercase DNS label: they become API paths, so nothing else passes (review-km-44 M1). Negative numbers are refused. `secrets` are refused with `forbidden` in **any** group (the agent's RBAC cannot read them anyway, except its own credential Secret). |
| `group` | string | list, get | Empty for the core group. |
| `version` | string | list, get | Required, for example `v1`. |
| `resource` | string | list, get | Required. Lowercase plural, for example `pods`. |
| `namespace` | string | list, get, logs, graph, appmap, pod.diagnose, pod.debug, service.check, health.check, pod.usage | Must be empty for `node.pressure`. For `pod.usage`, one namespace. For `health.check`, limits the pod rules to one namespace (node rules always run). Empty for cluster-scoped objects or all namespaces. Required for `logs`. For `graph`, limits the map to one namespace. For `appmap`, keeps the apps whose main namespace it is. Required for `pod.diagnose` and `pod.debug`. For `service.check`, limits the check to one namespace (required with `name`). |
| `name` | string | get, logs, pod.diagnose, pod.debug, node.pressure, service.check, pod.usage | Required for get, logs, pod.diagnose and pod.debug; for `logs`, `pod.diagnose` and `pod.debug` it is the pod. Optional for `node.pressure`: one node instead of all. Optional for `service.check`: one Service (needs `namespace`). Optional for `pod.usage`: one pod, with its namespace. |
| `labelSelector` | string | list | |
| `fieldSelector` | string | list | |
| `limit` | int | list | Page size, 0 (agent default) to 500. |
| `continue` | string | list | Token from the previous page. |
| `logs` | object | logs | `container`, `tailLines`, `sinceSeconds`, `previous`, `timestamps`. |
| `tokenReview` | object | tokenreview | `token`, `audience` (must be exactly `kubemap-portal`; anything else is `bad_request`). |
| `graph` | object | graph | Optional: `around` (a graph node ID such as `Deployment/shop/web`) and `depth` (1–5, default 2). |
| `appmap` | object | appmap | Optional: `app`, an app ID such as `argocd/flask-vue` or `sre-lab-app/redis`: only that app, the links that touch it, and their far ends as `peers`. An unknown app → `not_found`. |
| `nodeMetrics` | array | node.pressure, health.check | The node agents' latest `metrics` payloads (see `metrics`), without `topProcesses` and `network`; at most 1000, unique `nodeName`. See Node pressure. |
| `rbac` | object | rbac.whocan | Required. `subject` (`kind` ServiceAccount, User or Group; `name`; `namespace` for a ServiceAccount; `groups` for a user), `verb`, `resource` (`pods`, `pods/exec`, `deployments/scale`), `apiGroup` (`""` for core), `namespace` (empty: cluster-wide), `resourceName`. A subject alone asks for its chain; a subject with a verb and resource is a check; a verb and resource alone ask who can. |
| `logSearch` | object | logs.search | Required. `app` (an app ID from the app map, such as `argocd/url-shortener`), optional `component` (a graph node ID of one of its components, such as `Deployment/sre-lab-app/url-shortener`), `text` (one line, at most 200 bytes, case-insensitive), `levels` (any of `error`, `warn`, `info`, `debug`, `unknown`; empty: all), `since` and `until` (RFC 3339; default the last hour, at most 7 days), `previous` (also read the previous run of restarted containers), `maxLines` (0–2000, default 500). An unknown app or component → `not_found`. See Log search. |
| `chain` | object | object.chain | Required. Exactly one of `id` (a graph node ID, `Kind/namespace/name`, `Kind//name` for cluster-scoped objects, as in the app map) and `query` (one line of at most 200 bytes: the objects whose ID contains it, ignoring case). An unknown `id` → `not_found`. See Object chain. |
| `netpolCheck` | object | netpol.check | Required. `from` (a pod: `namespace` + `pod`, or an `ip`), `to` (a pod, a Service: `namespace` + `service`, or an `ip`; exactly one kind per end), `port` (1–65535; for a Service, the Service port, mapped to each backend's targetPort) and `protocol` (`TCP` default, `UDP`, `SCTP`). An unknown pod or Service → `not_found`. |

### `response`

Either `error` is set, or at most one kind of result (an empty list has none).

| Field | Type | Op | Notes |
|---|---|---|---|
| `error` | object | any | `code`, `message`. See error codes. |
| `items` | array | list | Raw Kubernetes objects. |
| `continue` | string | list | Token for the next page; empty on the last page. |
| `object` | object | get, graph, appmap, netpol.check, pod.diagnose, pod.debug, node.pressure, service.check, health.check, pod.usage, rbac.whocan, logs.search, object.chain | `get`: one raw Kubernetes object. `graph`: the raw analysis, `{apps, links, dependencies, routes, routeFindings, datastores, graph}` (see `agent/cluster/query`). `appmap`: the map ready to draw, `protocol.AppMap` (see below). `netpol.check`: `protocol.NetpolResult` (see below). `pod.diagnose`: `protocol.PodDiagnosis` (see below). `node.pressure`: `protocol.NodePressureReport` (see below). Too big for one message → `too_large`; ask for one namespace or app. `service.check`: `protocol.ServiceCheckResult` (see below). `pod.debug`: `protocol.PodDebug` (see below). `health.check`: `protocol.HealthReport` (see below). `pod.usage`: `protocol.PodUsageReport` (see below). `rbac.whocan`: `protocol.RBACResult` (see below). `logs.search`: `protocol.LogSearchResult` (see below). `object.chain`: `protocol.ObjectChain` (see below). |
| `logs` | base64 | logs | Raw log bytes, at most 512 KiB. |
| `truncated` | bool | logs | True if older lines were cut to fit. |
| `tokenReview` | object | tokenreview | `authenticated`, `audiences` (from `status.audiences`), `username`, `podName`, `podUid`, `nodeName`, and `expiresAt`: the token's `exp` claim, read by the cluster agent only after the API server authenticated it. The portal ends the node agent's session then; without it, after an hour (review-km-44 L7). |

**Node accept rule (A8, `TokenReviewResult.AcceptNode`).** The portal accepts a node agent only when all of these hold; otherwise it answers `error` (`unauthorized`):
1. `authenticated` is true.
2. `audiences` contains `kubemap-portal`. An authenticator that ignores audiences answers with none, and such a token proves nothing.
3. `username` is `system:serviceaccount:kubemap-node:kubemap-node-agent` (`NodeAgentUsername`), so a token from any other pod is refused.
4. `nodeName` (from the token's bound node claim) equals the `nodeName` in `register`.

### App map object (`appmap`)

The `object` of an `appmap` response is `protocol.AppMap` (KM-53): the map ready to draw. Example: `testdata/response-appmap.json`.

| Field | Notes |
|---|---|
| `apps` | Each: `id`, `name`, `namespace`, `source` (`argocd`, `helm`, `label`, `workload`), `health` (display text), `state` (for colour: `ok`; `warn` for OutOfSync, Progressing or a missing reference; `fail` for Degraded), `problems` (`[{id, status}]`, `id` links to the object), `system` (infrastructure; the UI hides it by default), `components`. |
| `apps[].components` | Sorted by tier, then ID. Each: `id` (graph node ID), `kind`, `namespace`, `name`, `app`, `tier`, `tierReason` (why that tier, for a tooltip), `status`, `healthy`, `missing`; data stores add `engine`, `version`; entries add `routes`; workloads add `services` and `pods`. |
| `tier` | `entry` (Ingress, IngressRoute), `frontend`, `backend`, `database` (databases, poolers, queues, search), `cache`. Decided in this order: entry kind; data store detection (KM-52); a label `app.kubernetes.io/component`, `component`, `tier` or `role`; a word in the name; routed from an entry (frontend, or backend when it also calls a data store); otherwise backend. |
| `routes` | Per rule: `host`, `path`, `tls`, `entryPoints`, `service`, `port`, `readyEndpoints` (-1 unknown), `problems` (`service missing`, `no ready endpoints (requests get 503)`, conflicts, plain HTTP without a redirect). |
| `pods` | `name`, `node`, `status` (as `kubectl get pods`), `ready`, `restarts`, `primary` (CNPG). |
| `links` | One per component pair: `from`, `to` (component IDs), `fromApp`, `toApp`, `via` (the Services in between), `missing` (the Service is gone or selects no pods), `evidence`. |
| `links[].evidence` | `type`: `route`, `env` (a literal env var), `configmap` (an env var from a ConfigMap key), `file` (a mounted ConfigMap file) or `operator` (CNPG Pooler → Cluster). `object`: where it was found (the Ingress, the workload, the ConfigMap, the operator resource). `name`: host and path, env var or file name. `key`: the ConfigMap key when it differs. `port`. `detail`: one line for a tooltip. Secret values are never read, so a dependency held only in a Secret has no link. |
| `links[].traces` | Set when traces show the call really happens (KM-54): `spans` (how many), `lastSeen`, `detail` (one line, for example `1 server spans in flask-vue-backend for Host flask-vue.albertycliu.xyz`). |
| `traces` | What the agent learned from traces, or absent when there is no Tempo. `source` (`tempo <url>`), `window` (how far back, `1h0m0s`), `spans`, `probes` (kubelet health probe spans, ignored), `confirmed` (links with `traces`), `unmatched` (calls in the traces that no link explains, at most 20), `error` (Tempo could not be read; the map is still returned), `at`. See Traces below. |
| `peers` | Components outside the returned apps that a link reaches, without pods. |
| `unassigned` | Components no app owns, such as an Ingress to a deleted Service. |
| `incomplete` | Resources the agent could not read, so the map may lack parts. |

**Traces (KM-54).** The cluster agent asks Tempo's HTTP search API, in the cluster, which calls really happen. It uses no Kubernetes permission: Tempo is an HTTP service, and the agent sends it only GET searches.
- **Where Tempo is:** `KUBEMAP_TEMPO_URL` in the agent's environment, or else a Service named `tempo` (or labelled `app.kubernetes.io/name=tempo`) with TCP port 3200. `off` disables traces.
- **The searches:** two TraceQL searches over the last hour, at most 200 traces each, selecting only these attributes:
  - server spans: `service.name`, the caller's address (`net.peer.ip`, `client.address`), the Host (`http.host`, `server.address`) and the user agent;
  - client spans: `service.name` and the target (`peer.service`, `server.address`, `net.peer.name`, the URL's host).
  - Answers are kept for a minute; Tempo gets 5 s, and the map waits at most 6 s.
- **A link is confirmed when:**
  - a server span in the link's target workload was asked for a route host of the link's entry (Ingress or IngressRoute);
  - or it came from a pod of the link's source workload;
  - or a client span in the source workload called a Service the link goes through (by name, DNS name or ClusterIP).
- **What is left out or refused:**
  - **Kubelet health probes** (user agent `kube-probe/…`) are left out in the search itself. They come every few seconds per pod, so on the lab cluster they were all 200 of the newest traces and hid the real calls. Server spans without a user agent attribute are therefore not used (TraceQL 2.9 can't test for a missing attribute). Probes that still arrive (from a node's own address) are counted in `probes` and ignored.
  - A `service.name` that names no workload on the map, or several, confirms nothing: no guess.
  - **Unmatched calls** are listed when both ends are known but the map has no link.

### NetworkPolicy check (`netpol.check`)

The `object` of a `netpol.check` response is `protocol.NetpolResult` (KM-33). The cluster agent evaluates the NetworkPolicies in its cache with the Kubernetes API rules: nothing is sent into the cluster. Example: `testdata/response-netpol.check.json`.

| Field | Notes |
|---|---|
| `allowed` | True when every path is allowed. |
| `summary` | One line, naming the deciding policy when blocked. |
| `from`, `port`, `protocol` | The resolved source (`Pod/ns/name` or `IP a.b.c.d`) and what was checked. |
| `paths` | One per destination: a Service or a ClusterIP gives one path per backend (its pods, or the EndpointSlice addresses of a Service without a selector). Each: `to`, `port` (after targetPort mapping), `allowed`, `egress` (at the source) and `ingress` (at the destination). |
| `egress`, `ingress` | `direction`, `isolated` (some policy selects the pod for this direction), `allowed`, `decision` (one line), `selecting` (the policies that isolate the pod; when blocked, these decide), `allowedBy` (`[{policy, rule, detail}]`, the matching rules, first one decides). An end that is not a pod, or a host-network pod, is not subject to policies. |
| `notes` | Things the verdict depends on: a ClusterIP resolved to backends, a host-network source, a Service without a selector or port. |

Selectors match pods only, so traffic to an IP that is not a pod (the internet, a node, the API server) is allowed by an isolated pod's egress only through an `ipBlock`. `ipBlock` is also matched against pod IPs, as Calico does; other CNIs may differ.

### Pod diagnosis (`pod.diagnose`)

The `object` of a `pod.diagnose` response is `protocol.PodDiagnosis` (KM-56): what explains a crashing or stuck pod, in one answer. The pod comes from the agent's cache; its events, logs and usage are read live with `list`, `get` and `pods/log` only. What cannot be read (RBAC, no metrics-server) is listed in `notes`, never an error. Example: `testdata/response-pod.diagnose.json`.

| Field | Notes |
|---|---|
| `namespace`, `name`, `node`, `owner`, `phase`, `qosClass` | `owner` is the top controller (`Deployment/ns/name`). |
| `status`, `reason`, `message` | `status` as `kubectl get pods` shows it, except that an evicted pod says `Evicted` (on 1.26+ an evicted pod may end `Succeeded`; the `DisruptionTarget` condition tells). |
| `findings` | Conclusions, most severe first: `severity` (`critical`, `warning`, `info`), `container`, `title` (`Killed for memory (exit 137)`: said for exit 137 whether the runtime reports `OOMKilled` or `Error`, since some runtimes such as k3s with containerd 2.2 never set `OOMKilled`; `Killed by the liveness probe (exit 137)` only when the events show that kill; `CrashLoopBackOff`, `Image not on the node`, `Evicted`, `Liveness probe failing`, `Near the memory limit` …) and `detail` (evidence and the next step). One `info` "No problem found" when nothing is wrong. |
| `containers` | Init containers too (`init`). Each: `name`, `image`, `ready`, `restarts`, `state` (in words), `last` (how the previous run ended: `reason`, `exitCode`, `signal`, `meaning` such as "SIGKILL (137): …", `startedAt`, `finishedAt`, `message`), `requests`, `limits`, `usage` (`cpuMillis`, `memoryBytes`, `cpuOfLimit`, `memoryOfLimit` in percent; from the metrics API), `probes` (one line each), `logs` (last 100 lines, at most 64 KiB, of the previous run after a restart, else the current one), `logsFrom` (`previous` or `current`), `logsTruncated`. |
| `events` | The pod's events (all types, read live; the cache holds warnings only), newest first, at most 20: `type`, `reason`, `message`, `count`, `lastSeen`. |
| `notes` | What could not be read. |

### Node pressure (`node.pressure`)

The `object` of a `node.pressure` response is `protocol.NodePressureReport` (KM-60): per node, whether workloads really wait for a resource. The cluster agent combines the kubelet's node conditions (from its cache) with the kernel pressure stall information (PSI) in the **node agents' reports**. The portal passes these reports in the request's `nodeMetrics`: the latest `metrics` payload it holds for each node of the cluster, without `topProcesses` and `network`. The cluster agent makes **no kubelet calls** (it has no `nodes/stats` or `nodes/proxy` permission), so its cluster-wide read token is never sent to a node (review KM-60, approved 2026-09-28). Example: `testdata/request-node.pressure.json` and `testdata/response-node.pressure.json`.

| Field | Notes |
|---|---|
| `time` | When the report was made (UTC). |
| `nodes` | Sorted by name; every node in the cluster agent's cache (or the one in `name`). Each: `node`, `ready`, `psiAvailable` (false without a fresh node agent report with PSI: no node agent on the node, a report older than 2 minutes by its own `time`, or a kernel without PSI), `reportTime` (when the node agent took the sample used), `verdict` (the worst resource verdict), `summary` (one line), `resources`. |
| `nodes[].resources` | Always five, in order `cpu`, `memory`, `io`, `disk`, `pids`. Each: `resource`, `condition` (`MemoryPressure`, `DiskPressure`, `PIDPressure`; empty for cpu and io, which Kubernetes does not watch), `conditionStatus` (`True`, `False`, `Unknown`), `conditionSince`, `psi` (`some` and `full`, each `avg10`, `avg60`, `avg300` in percent; absent when not available; disk and pids have none), `verdict`, `detail` (the evidence in one line). |
| `verdict` | `ok`, `elevated`, `pressure` or `unknown`. A condition that is `True` gives `pressure`: the kubelet is already evicting. Otherwise PSI decides on the 60 s average: `some` ≥ 10 % is `elevated`, `some` ≥ 25 % or `full` ≥ 5 % (memory and io) is `pressure`. Without PSI, cpu and io are `unknown` ("not available"), never `ok`, and memory falls back to its condition. A condition in state `Unknown` gives `unknown`. |
| `notes` | Nodes without a node agent report, and reports too old to use. |

Request: `nodeMetrics` is allowed only with `node.pressure` and `health.check`: at most 1000 reports, each with a unique valid `nodeName`. Reports for nodes the cluster agent does not know are ignored.

### Health checks (`health.check`)

The `object` of a `health.check` response is `protocol.HealthReport` (KM-61, KM-62): the findings of Kubemap's built-in rules, each with what it means, why it matters, the fix and the command that proves the fix. The rules run in the cluster agent, on its cache and on the node agents' reports that the portal passes in `nodeMetrics` (as for `node.pressure`, with the same 2-minute age limit); the cluster agent makes no kubelet calls, and only the findings leave the cluster. The disk rules read the report's filesystems: nodefs is the mount that holds `/var/lib/kubelet`, imagefs the most specific mount of `/var/lib/rancher`, `/var/lib/containerd` or `/var/lib/docker` (the same as nodefs when none has its own). A node without a fresh report gets a note, and only its condition-based rules run. Example: `testdata/request-health.check.json` and `testdata/response-health.check.json`.

| Field | Notes |
|---|---|
| `time` | When the report was made (UTC). |
| `findings` | Most severe first, then in rule order, then by object; at most 200. Each: `rule`, `severity` (`critical`, `warning`, `info`), `object` (`Node/name`, `Pod/ns/name`, or `Cluster` / `Namespace/ns` for a grouped finding), `title`, `evidence` (the measured values), `meaning`, `why`, `fix`, `verify` (commands, one per line, with the expected result after `#`), and for grouped findings `items` (at most 20 `ns/pod`) and `count`. |
| `rules` | Every rule that ran, so no finding can be told from not checked: `node.not-ready`, `node.disk.eviction`, `node.imagefs.eviction`, `node.image-gc`, `node.inodes`, `node.memory`, `node.swap`, `node.pressure.cpu`, `node.pressure.memory`, `node.pressure.io`, `node.pids`, `pod.crashloop`, `pod.exit-137`, `pod.image-pull`, `pod.image-local-only`, `pod.pending`, `pod.evicted`, `pod.no-memory-limit`. |
| `thresholds` | The kubelet settings the node rules compare with: `evictionHard`, `evictionMinimumReclaim`, `imageGCHighPercent`, `source` (`kubelet defaults` or `agent settings`). The agent cannot read the kubelet configuration (that needs `nodes/proxy`), so a kubelet with other settings is described to the cluster agent with `KUBEMAP_EVICTION_HARD`, `KUBEMAP_EVICTION_MINIMUM_RECLAIM` and `KUBEMAP_IMAGE_GC_HIGH_PERCENT`, in the kubelet's own syntax. |
| `notes` | Kubelet summaries not read, findings cut, thresholds assumed. |

Disk rule (the lesson of the 2026-09-28 incident): warning when free space is under the eviction threshold + 10 points, critical under + 5 points or while `DiskPressure` is True. The evidence gives the minimum-reclaim target (threshold + `evictionMinimumReclaim`, for example 5 % + 10 % = 15 %): once the kubelet starts to evict, it continues until that much is free. Exit 137 is reported as "Killed for memory" whether the runtime says `OOMKilled` or `Error`. Swap over 90 % is a warning, and critical over 98 % when less than 20 % of RAM is available too.

### Service check (`service.check`)

The `object` of a `service.check` response is `protocol.ServiceCheckResult` (KM-77), from the agent's cache. Example: `testdata/response-service.check.json`.

| Field | Notes |
|---|---|
| `services` | Problems first (critical, then warning), then by ID. Each: `id`, `namespace`, `name`, `type`, `selector`, `podsMatched` (running or pending pods the selector matches), `podsReady`, `readyEndpoints` (from EndpointSlices; -1 unknown), `healthy`, `findings`. ExternalName Services are not checked. |
| `findings` | `severity`, `problem`, `port` (for port findings), `detail` (evidence), `fix`, `command` (what a person types to see it). Problems: `selector matches no pods` (with near misses: the label values pods have instead, a likely typo, pods in other namespaces that match), `no ready pods`, `some pods not ready`, `named targetPort not declared` (critical: no endpoint for that port), `named targetPort missing on some pods`, `targetPort not declared` (warning: declared ports are informational), `EndpointSlices show no ready endpoint`, `no endpoints` (a Service without a selector). |
| `problems` | How many Services have a critical or warning finding. |

### Guided debugging (`pod.debug`)

The `object` of a `pod.debug` response is `protocol.PodDebug` (KM-76): the SRE checklist for one pod, walked in order, each step with evidence, a verdict and the `kubectl` command a person would type. It combines `pod.diagnose` (steps 1–5), the node objects (6), `service.check` (7) and `netpol.check` (8); the connections it tests come from the dependencies found in configuration (KM-50). Reads are `get`, `list` and `pods/log` only. Example: `testdata/response-pod.debug.json` (the demo's netpol scenario).

| Field | Notes |
|---|---|
| `namespace`, `name`, `owner`, `status` | As in `pod.diagnose`. |
| `cause`, `causeStep` | The most specific problem found, as one line, and its step. Specific causes rank before symptoms: `netpol`, `service`, `resources`, `node`, `logs`, `exit-code`, `events`, `status`. Empty when every step is ok. |
| `steps` | Always eight, in this order: `status` (Status and reason), `events`, `logs` (previous run after a restart, else current; error lines), `exit-code`, `resources` (requests and limits vs usage; exit 137 counts as killed for memory), `node` (pressure conditions and taints; for an unscheduled pod, its requests against each node's allocatable and free CPU), `service` (Services that select the pod, a Service whose selector nearly matches it (typo), and the Services it depends on, checked like `service.check`), `netpol` (its dependencies' traffic and its known callers' traffic, checked like `netpol.check`, and which policies isolate it). Each: `key`, `title`, `verdict` (`ok`, `problem`, `unknown` when the data was not available), `finding` (one line), `evidence`, `commands`. |
| `notes` | What could not be read. |

### Pod usage (`pod.usage`)

The `object` of a `pod.usage` response is `protocol.PodUsageReport` (KM-28): what each pod really uses, from the **metrics API** (`pods.metrics.k8s.io`, served by metrics-server), next to the requests and limits from the pod spec in the agent's cache. The cluster agent reads PodMetrics with `list` (or `get` for one pod) only; its ClusterRole grants exactly `get` and `list` on `metrics.k8s.io/pods` (approved 2026-09-28). It makes no kubelet calls. Example: `testdata/response-pod.usage.json`.

| Field | Notes |
|---|---|
| `time` | When the report was made (UTC). |
| `pods` | Largest memory working set first, at most 500. Each: `namespace`, `name`, `node` (from the pod spec; empty when the spec is gone), `sampledAt` and `windowSeconds` (the interval metrics-server averaged CPU over), `cpuMillis` and `memoryBytes` (the sums of the containers; memory is the working set, what `kubectl top`, eviction and the OOM killer look at; CPU is rounded up to whole millicores), `containers`. Pods without metrics (Pending, restarting, or not scraped yet) are missing; running ones are counted in `notes`. |
| `pods[].containers` | In spec order. Each: `name`, `cpuMillis`, `memoryBytes`, `cpuRequestMillis`, `cpuLimitMillis`, `memoryRequestBytes`, `memoryLimitBytes` (0 when not set), and usage in percent: `cpuOfRequest`, `cpuOfLimit`, `memoryOfRequest`, `memoryOfLimit` (0 when there is no request or limit). |
| `notes` | Running pods without metrics yet, and pods cut. |

A pod name needs a namespace (`bad_request`). An unknown pod, or one without metrics, is `not_found`. No metrics API (metrics-server not installed) is `unavailable`; missing RBAC on `pods.metrics.k8s.io` is `forbidden`.

### Object chain (`object.chain`)

The `object` of an `object.chain` response is `protocol.ObjectChain` (KM-35): everything one object is connected to, from the agent's cache and the relationship graph, with no Kubernetes call. The pods are the centre: a workload comes down to the pods it owns, a Service to the pods it selects, a ConfigMap, Secret, claim, ServiceAccount or node to the pods that use it, a route to its Services' pods, an autoscaler to its target's pods, a NetworkPolicy to the pods it selects. Around those pods are the sections below. Secrets appear by name only; their values are never read. Example: `testdata/request-object.chain.json` and `testdata/response-object.chain.json`.

| Field | Notes |
|---|---|
| `object` | The object: `id`, `kind`, `namespace`, `name`, `status`, `missing`. |
| `sections` | Non-empty sections in this order, each `key`, `items` (at most 30) and `more` (items left out): `owners` (top owner first), `owned` (ReplicaSets, Jobs …, without pods), `pods`, `services`, `routes` (Ingress and IngressRoute rules; `detail` is host/path → Service:port), `config` (ConfigMaps; `detail` says how: `env DB_HOST`, `volume config`), `secrets` (names only), `volumes` (claims, then their volumes), `serviceaccounts`, `policies` (NetworkPolicies that isolate the pods; `detail` says ingress, egress or both), `autoscalers`, `nodes`, `calls` (Services the workloads' configuration names), `calledBy` (workloads whose configuration names the Services). Items have the fields of `object` plus `detail`. The object itself never appears in its own sections; a reference to something that does not exist has `missing`. |
| `apps` | The app map apps (IDs) the object or its workloads belong to. |
| `matches` | For a `query`: at most 20 objects, a name equal to the text first, then names starting with it, then shorter IDs. NetworkPolicies are included. |
| `notes` | For example: NetworkPolicies not read; a Service with no pods behind it. |

### Log search (`logs.search`)

The `object` of a `logs.search` response is `protocol.LogSearchResult` (KM-57): the matching log entries of every pod of an app, merged by time and tagged with their pod and container. The pods come from the app map (the app's components, or one of them), at most 30 per search. Each container's log is read with `pods/log` only (the same right as `logs`; no kubelet call), with the kubelet's timestamps, `sinceTime` at the start of the window and `tailLines` 10,000, so the newest lines of the window are read even from a chatty container; of those, the newest 1 MiB is searched, and a source that had more is `truncated` with a note. At most 2 searches run at once in one agent; a third gets `unavailable`. Lines that continue an entry (a Java, Python, Go or Node.js stack trace, or an indented message) are joined to it in `stack`. The level comes from the line: a JSON `level`/`severity` field (also pino numbers), logfmt `level=`, the klog prefix (`E0928`), a level word (`ERROR`, `[warn]`, `Error:`), else the shape of an exception or a stack trace. One unreadable container (not started, forbidden) does not fail the search: its source has an `error`. Example: `testdata/request-logs.search.json` and `testdata/response-logs.search.json`.

| Field | Notes |
|---|---|
| `app`, `since`, `until` | The app and the window searched (UTC). |
| `entries` | Oldest first; at most `maxLines`, the newest kept. Each: `time`, `namespace`, `pod`, `container`, `previous` (from the previous run), `level` (`error`, `warn`, `info`, `debug`, `unknown`), `text` (at most 4000 bytes), `stack` (at most 100 lines). |
| `levels` | Entries that match the text and the window, by level, before the level filter. |
| `matched`, `truncated` | How many entries matched, and whether older ones were left out (line limit or message size). |
| `sources` | Each container log read: `namespace`, `pod`, `container`, `previous`, `lines` (entries in the window), `matched`, `truncated` (the window held more than the newest 10,000 lines or 1 MiB that were searched), `error`. |
| `notes` | Pods not read, logs cut, matches left out. |

### RBAC questions (`rbac.whocan`)

The `object` of an `rbac.whocan` response is `protocol.RBACResult` (KM-34). The agent evaluates the RBAC objects in its cache with the RBAC authorizer's rules; it creates no SubjectAccessReview, so it needs no permission beyond reading RBAC objects. Other authorizers (Node, webhooks) and admission (PodSecurity, quota) are not included, and every answer says so in `notes`. Example: `testdata/response-rbac.whocan.json`.

| Field | Notes |
|---|---|
| `mode` | `chain` (a subject alone), `check` (a subject with a verb and resource), `whocan` (a verb and resource alone). |
| `subject`, `groups` | The subject, and the groups it is evaluated with: a ServiceAccount is in `system:serviceaccounts`, `system:serviceaccounts:<ns>` and `system:authenticated`; a user in its given groups and `system:authenticated`. |
| `allowed` | A check's answer. |
| `grants` | Chain: every binding that applies. Check: those that allow the action. Each: `binding` (`RoleBinding/ns/name` or `ClusterRoleBinding//name`), `role`, `scope` (the namespace, or `cluster`), `via` (`direct`, `direct (as user …)` or `group <name>`), `roleMissing` (a binding to a role that does not exist), `rules`, `risky`. |
| `risky` | Admin-equivalent rights: `read secrets`, `create pods` (also through Deployments, Jobs …), `exec into pods`, `change RBAC`, `bind or escalate`, `impersonate`, `nodes/proxy`, `create tokens`, `change workloads` (update or patch on pods, ephemeral containers, Deployments, DaemonSets, StatefulSets, ReplicaSets, Jobs, CronJobs: a changed pod template runs any image as any ServiceAccount of the namespace), `change admission control` (webhook configurations and admission policies), `create persistentvolumes` (hostPath), `approve certificates` (the CSR approval subresource plus `approve` on a signer), `change nodes` (labels and taints steer workloads), `wildcard`. |
| `subjects` | Who-can: each subject with the `binding`, `role` and `scope` that allow it; `everyone` for groups that contain every account or user (sorted first); `namespaceWide` for `system:serviceaccounts:<ns>`, every ServiceAccount of one namespace (also named in `notes`); `resourceNames` when only named objects are allowed. |
| `notes` | What the answer does not cover (other authorizers; node identities, which get their rights from the Node authorizer, are not shown); bindings to missing roles; everyone-groups and namespace-wide groups. |

### `stream.open`

| Field | Type | Notes |
|---|---|---|
| `kind` | string | `logs`. Unknown → `unsupported`. |
| `namespace` | string | Required. |
| `name` | string | The pod. Required. |
| `logs` | object | Same options as in `request`. |

### `stream.data`

| Field | Type | Notes |
|---|---|---|
| `seq` | int | Starts at 0 and grows by 1 per chunk, so gaps are visible. |
| `data` | base64 | Raw bytes, at most 512 KiB. |

### `stream.close`

| Field | Type | Notes |
|---|---|---|
| `reason` | string | For example `pod deleted`, `cancelled`, `container exited`. |
| `error` | object | Set when the stream failed. |

### `error`

| Field | Type | Notes |
|---|---|---|
| `code` | string | One of the codes below. |
| `message` | string | For people; do not parse it. |

## Error codes

| Code | Meaning |
|---|---|
| `bad_request` | Malformed message, wrong version, missing id or missing required field. |
| `unsupported` | Unknown message type, request op or stream kind. |
| `too_large` | Over 1 MiB (or a stream chunk over 512 KiB). |
| `unauthorized` | Token missing, wrong, expired, or bound to another node. |
| `forbidden` | Authenticated, but RBAC does not allow it. |
| `not_found` | The object or stream does not exist. |
| `timeout` | The Kubernetes API or the peer did not answer in time. |
| `unavailable` | Temporarily unable, for example the informer cache is not synced yet. Retry later. |
| `internal` | A bug or unexpected failure. |

## Sequences

### In-process cluster agent

The cluster agent is attached to the server at start (`Hub.AttachLocal`). `Hub.Request`, `Hub.OpenStream`, `Hub.Summary` and `Hub.Connected` work as they did over a WebSocket; a request is handed to the agent's query handler, and its `response`, `summary` and `stream.data` come back through the same hub tables. There is no registration, import token, credential or Accept step. The local cluster record is created Active at start.

### Node registration (TokenReview through the cluster agent)

1. The node agent sends `register` `{role: node, clusterId, nodeName, serviceAccountToken}` (id `reg-3`) to the server.
2. The server sends the in-process cluster agent a `request` `{op: tokenreview, tokenReview: {token, audience: kubemap-portal}}` (id `q-4`).
3. The cluster agent creates a TokenReview at the Kubernetes API, which answers with the authenticated user and the pod and node from the token's bound claims.
4. The cluster agent returns a `response` `{tokenReview: {authenticated, audiences, username, podName, podUid, nodeName}}` (id `q-4`).
5. The server runs `AcceptNode`: authenticated, audience `kubemap-portal`, the node agent ServiceAccount, and the node name matches the token.
6. If valid, the server sends `register.ack` `{status: active}` (id `reg-3`), and the node agent sends `metrics` every 10 s.
7. If invalid, or the cluster agent is not attached, the server sends `error` `{code: unauthorized or unavailable}` (id `reg-3`) and closes the connection; the node agent retries with backoff.

### Pod log stream

1. The browser asks the server to open a log view.
2. The server sends the in-process cluster agent `stream.open` `{kind: logs, namespace, name, logs: {tailLines: 100}}` (id `s-1`).
3. The cluster agent follows `GET pods/log?follow=true` at the Kubernetes API.
4. While the pod writes logs, the cluster agent sends `stream.data` `{seq: n, data}` (id `s-1`) and the server passes the lines to the browser.
5. If the user closes the view, the server sends `stream.close` `{reason: cancelled}`. If the pod ends or the API fails, the cluster agent sends `stream.close` `{reason, error?}`.
