# portal

Go code for `kubemap-portal`, the web server that runs as the Deployment `kubemap` (see `cmd/kubemap-portal`).

- `localcluster/`: the cluster agent inside the server process: it reads the cluster it runs in (informers, `get`/`list`/`logs`/actions) and answers the same requests a remote agent would.
- `server/`: HTTP routes (`GET /healthz` so far).
- `store/`: SQLite storage (KM-15).
- `auth/`: admin login and sessions (KM-16).
- `clusters/`: read-only cluster API (the one local cluster, auto-created Active).
- `httpjson/`: small JSON helpers shared by the API packages.
- `tunnel/`: the hub: the in-process cluster agent and the node agents' endpoint `GET /v1/connect` (KM-18).
- `appmap/`: `GET /api/clusters/{id}/appmap[?namespace=][&app=]`, the application map, asked from the cluster agent with a `request` of op `appmap` (KM-53). Errors: `agent_offline` (503), `unsupported` (501, agent too old), `not_found` (404, unknown app), `too_large` (422), `timeout` (504).
- `resources/`: `GET /api/clusters/{id}/namespaces`, `GET /api/clusters/{id}/pods[?namespace=]` and `GET /api/clusters/{id}/namespaces/{ns}/pods/{pod}/logs[?container=&previous=&timestamps=&tailLines=&follow=]` (KM-80). Lists come from the agent's `list` request, all pages up to 5,000 items. Logs without `follow` come from its `logs` request as `text/plain` (`X-Kubemap-Truncated` when older lines were cut). With `follow=true` the portal opens a `stream.open` log stream and answers `application/x-ndjson`: `{"data":"<base64>"}` lines, then one `{"end":{"reason":…,"error":…}}`. Closing the request closes the stream on the agent.
- `access/`: RBAC and Argo CD views (KM-37): an account's roles through its bindings, may-it and who-can questions (agent op `rbac.whocan`, KM-34), one namespace's permissions (the risky actions × who holds them, and its ServiceAccounts' own risky rights), and Argo CD applications with their managed resources, sync and health (read with the `list` op). `GET /api/clusters/{id}/rbac`, `/rbac/accounts`, `/rbac/namespaces/{ns}`, `/argocd`.
- `alerts/`: alerts (KM-31). `GET /api/alerts` lists every cluster with a critical finding now, read from the newest draft of each cluster (KM-90's drafter checks each cluster every minute), so an alert comes within about a minute and clears with the finding; namespaces the drafter ignores raise none. The page shows a banner under the top bar (closed per set of findings, back when they change), a count in the tab title, and a "N critical" badge on the cluster list and page.
- `journal/`: the incident journal (KM-89) and its interview view (KM-91); see below.
- `agentcall/`: the shared tunnel/agent error mapping for these APIs.

## Running

```
kubemap-portal [run] [-listen :8080] [-data-dir DIR] [-namespace kubemap-system] [-log-file FILE] [-audit-retention-days 365] [-incident-ignore-namespaces a,b]
kubemap-portal reset-admin-password [-data-dir DIR]
kubemap-portal journal-import [-data-dir DIR] [-replace] FILE.md...
kubemap-portal version
```

The server speaks plain HTTP. TLS ends at Traefik (`deploy/ingress.yaml`); the session cookie is `Secure` unless the request says `X-Forwarded-Proto: http`. Install: `docs/install.md`.

## Login

- **First start:** the portal creates the account `admin` with a random password and writes it to `<data dir>/initial-admin-password` (mode 0600). The image has no shell, so read it by running `reset-admin-password` as above. Log in, then set a new password (12 to 72 bytes). Until you do, the API allows only `/api/me`, `/api/password` and `/api/logout`. The file is deleted when the password changes.
- **Forgot the password:** run `kubectl -n kubemap-system exec deploy/kubemap -- /usr/local/bin/kubemap-portal reset-admin-password` (the server may keep running). It prints a new random password, ends all admin sessions and asks for a change at the next login.
- **Sessions:** cookie `kubemap_session` (HttpOnly, Secure behind the ingress, SameSite=Strict). They end after 12 hours without use, 7 days after login at the latest, on logout, and on every password change. Only a SHA-256 hash of the session token is stored.
- **Brute force:** 5 failed logins from one address block that address for 15 minutes. Failed and successful logins, logouts and password changes go to the audit log.
- API: `POST /api/login`, `POST /api/logout`, `GET /api/me`, `POST /api/password` (JSON only). Cross-site `POST`s are refused, and so is any write that carries neither a JSON body nor the header `X-Kubemap-Request` (the web app sends it; review KM-44 L15).
- **Failed logins:** a name that is not an account is logged and audited only by its length, since it is often a password typed into the wrong field (L8). The per-address limit keys IPv6 clients by /64; after 5 failures for one username, from any address, each try waits 1 s, doubling up to 30 s (L13). A delay, not a block, so nobody can lock the admin out.
- **HTTP:** every response has `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'` unless the page sets its own (L14); headers are limited to 64 KiB and request bodies to 1 MiB, read within 30 s (L12).
- **Retention** (L9): audit entries after `-audit-retention-days` (365), drafts after 90 days (each cluster's newest stays); at most 50 evidence snapshots per journal entry.

## The cluster and the node agents

- **One cluster.** At start the server creates the cluster record `local` ("this cluster") as Active and attaches the in-process agent (`localcluster`). There is nothing to register or accept, and no import token.
- **In-process agent.** It uses the server's ServiceAccount `kubemap` (ClusterRoles `kubemap-readonly` and `kubemap-tokenreview`). Requests go to it as function calls, not over a socket.
- **Node agents** (the DaemonSet `kubemap-node`) dial `ws://kubemap.kubemap-system.svc:8080/v1/connect` (protocol v1, `docs/protocol.md`). The first message is `register` with the node's projected ServiceAccount token (audience `kubemap-portal`). The server has the in-process agent check it with a TokenReview and applies `protocol.TokenReviewResult.AcceptNode` (audience, ServiceAccount, bound node name). Every refusal is one generic `registration refused` error; the log has the reason, never a token. A `register` with role `cluster` is refused.
- **Health.** Both sides send heartbeats every 10 s; a connection silent for 30 s is closed and the node shows as disconnected. Each node is tracked on its own, so several nodes work side by side: "node down" (the Kubernetes node is NotReady) is told apart from "agent down" (the node is Ready but its agent is silent).
- **Registrations are rate-limited** before any token review: 6 at once, then one every 10 s, per client address; 30 at once, then one per second, per cluster. A node session ends when its token expires (at most an hour) and the agent reconnects with its refreshed token.
- **Known limits.** The PVC is ReadWriteOnce with one replica (strategy Recreate), so the server stays on one node. The node agents' connection is plain HTTP inside the cluster; the NetworkPolicy (`deploy/networkpolicy.yaml`) limits who can reach the server's port.

## Incident journal (KM-89, KM-91)

A record of real incidents and changes, stored in the portal database (migration `0005_journal.sql`). An entry is an incident or one day's change record: title, date, cluster, severity, and sections in order. A section is Markdown text (summary, detection, root cause, actions, result, lessons, other) or rows: the timeline (time, event), the decisions (decision, why, trade-off), the follow-ups (status, owner), the STAR summary, or change rows (time, change, why, how it was checked, undo).

- **Import and export:** the Markdown layout of `docs/incidents/` and `docs/changes/`. A table or list in that layout becomes rows; anything else stays text, so nothing is lost. `docs/incidents/2026-09-28-disk-pressure.md` and `docs/changes/2026-09-28.md` export byte for byte (tested). Import in the page (Journal, Import Markdown) or with `kubemap-portal journal-import docs/incidents/*.md docs/changes/*.md`. There is one entry per file name; import again with `-replace` (or the page's Replace) to update it and keep its evidence.
- **Drafts (KM-90):** the drafter's draft incidents are stored in the journal's tables (`journal_drafts`), so they survive a restart. The journal page lists them; **Write it up** (`POST /api/journal-drafts/{id}/write-up`, with the browser's time zone) makes an incident with the draft's title, date, cluster, severity (critical → critical, warning → major), detection and timeline, empty sections for the rest, and the draft's finding snapshots as evidence. Once per draft.
- **Evidence:** a snapshot taken by the portal, stored with the entry (256 KiB at most): the problem finder's findings, the warning events, node metrics (the latest report and the last hour of rollups) or audit log entries, each with a link to the Kubemap page it came from.
- **Views:** the full record (`#/journal/<id>`), the interview view in STAR form with the decisions table (`#/journal/<id>/interview`), the editor, and the audit log (`#/audit`).
- **Edits:** logged-in users only. Every create, edit, import, delete and evidence change goes to the audit log. An edit names the version it started from; if someone saved in between, it is refused (409) instead of overwriting.
- API: `GET/POST /api/journal`, `POST /api/journal/import`, `GET/PUT/DELETE /api/journal/{id}`, `GET /api/journal/{id}/markdown[?evidence=1]`, `POST /api/journal/{id}/evidence`, `DELETE /api/journal/{id}/evidence/{eid}`, `GET /api/audit`.

## YAML view and edit (KM-39)

`portal/yamlview`, behind the login:

- `GET /api/clusters/{id}/yaml?group=&version=&resource=&namespace=&name=` shows any object's YAML, fetched with the agent's `get` op.
- `POST /api/clusters/{id}/yaml/diff {…, edited}` compares an edited copy with the **live** object (fetched again) and returns a line diff, a minimal patch and two `kubectl patch` commands: `--dry-run=server`, then the apply.

**Kubemap never applies an edit** (decision 2026-09-28). A server-side dry run needs the same RBAC as the write, and the agent stays within KM-38's four actions. So the dry run and the apply are commands for the admin to run with their own credentials. The agent only ever receives `get`.

- **Redacted in the view** (`<redacted by Kubemap>`):
  - Secret data, although the agent refuses Secrets anyway, and the page says Kubemap never shows them;
  - ConfigMap values under secret-looking keys (password, token, secret, api key, credential, DSN…), and all `binaryData`;
  - literal env values under such names, or holding a `user:password@` URL, in every container list of Pods, pod templates and CronJobs;
  - kubectl's `last-applied-configuration` annotation.
  - `managedFields` are dropped as noise.
- **The patch** is a strategic merge patch for built-in kinds (list items change by merge key, so a redacted env entry stays untouched) and a JSON merge patch for custom resources.
  - It holds only the changed fields, plus `metadata.resourceVersion` as an optimistic lock: if the object changed since the view, `kubectl` gets a Conflict.
  - A patch that would write the placeholder back is refused (checked on the decoded patch, so JSON escaping can't hide it). So is an edit that changes `apiVersion`, `kind`, name or namespace.
  - Status and server-set metadata are ignored, and a note says so.
- **Notes:** Argo CD-managed objects get a "change it in Git" note, and Pods a note that most pod fields are immutable.

## Data directory

Default `./data` for development; in the cluster it is `/data/kubemap` on the PVC mounted at `/data`. Files: `portal.db` (+ `-wal`, `-shm`; password hashes, sessions, the audit log and the journal) and, until first login, `initial-admin-password` (secret).

The folder is made private at every start of `run`, `reset-admin-password` and `journal-import` (`secureDataDir`): umask 077, so new files (SQLite's too) are `0600`, and the folder and everything in it are set to `0700`/`0600`.
