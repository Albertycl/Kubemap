# Kubemap user guide



## What Kubemap is

Kubemap is a web tool for your Kubernetes clusters. It shows how your apps connect, how healthy each server is, and why a pod fails. It tells you how to fix each problem, and it keeps a record of real incidents and what you decided.

It runs inside the cluster it shows. One server pod serves the web page (for example https://kubemap.albertycliu.xyz) and reads the cluster through the Kubernetes API. A small node agent runs on each server (node) and reports its health to the server. There is nothing to register: open the page, log in, and the cluster is there. By default Kubemap can only read.

## What it can do, and what is ready

| Feature | What it does | Status |
|---|---|---|
| App map | Each app from its web address to its frontend, backend and database pods, with the health of each pod. It finds the links from routes, Services, environment settings, ConfigMaps and database operators (CNPG). | Ready |
| Server health | CPU, memory, swap, disk, inodes and pressure (PSI) for each server, the top processes, and the pod that owns each process. | Ready |
| Pods and logs | Pods of every namespace with status and restarts. Logs of a pod, live, without kubectl. | Ready |
| Problem finder | Checks the cluster and each server for known problems (disk near the eviction threshold, full swap, crash loops, exit 137, Pending pods, Services with no endpoints) and gives the fix and the command that proves it. | Ready |
| Crash helper and guided debugging | For a failing pod, the checklist an SRE follows, in order: status, events, previous logs, exit code, limits and usage, node pressure, Service endpoints, NetworkPolicy. Each step shows the evidence, a verdict and the kubectl command. | Ready |
| NetworkPolicy checker | Can pod A reach pod B on a port? The answer names the policy that decides. | Ready |
| Practice labs | Namespace kubemap-demo with apps that fail on purpose: OOM kill, crash loop, blocked traffic, Service with no pods, Pending, 503, 502, ImagePullBackOff, RBAC forbidden. One command breaks, resets or deletes them. | Ready |
| Scenario library | Real SRE questions ("I get a 503, what do I do?") with the investigation steps, live read-only checks on your cluster, causes, fixes, prevention and senior-level talking points. | Ready |
| Incident journal | Your real problems: timeline, actions, decisions with reasons and trade-offs, result and lessons, with a STAR view for interviews. | Ready |
| Who can do what (RBAC) | For any account, which roles apply through which bindings, and who can do a given action. | Ready |
| Read-only by design | Kubemap only reads the cluster. It has no fix buttons; you make the change with kubectl or in Git. | Ready |

## How to use it to manage your cluster

A short daily routine, from the big picture to the detail:

1. Open the servers overview. Look at the worst server first: pressure, swap, disk and inodes.
2. Open the problem finder. Read the critical findings first. Each finding says what it means, how to fix it and how to prove the fix.
3. Open the app map of each important app. A red box shows exactly which part is broken: the route, the frontend, the backend or the database.
4. Click a broken pod. Start guided debugging. Follow the steps until a step shows the problem.
5. Read the logs from the same page, live if needed.
6. Decide the fix. If Argo CD manages the app, change it in Git. If not, make the fix yourself with kubectl (Kubemap is read-only).
7. If it was a real incident, write it in the incident journal: what happened, what you did, what you decided and why.

### What does this mean? (diagnosis hints)

On the pod list, on problem finder findings about a pod or a node, and on the object chain and app map pages (for a Service, Deployment, StatefulSet, PersistentVolumeClaim or pod whose status shows a problem), a "What does this mean?" link opens a short hint: what the status means, read-only `kubectl` commands with the real pod, namespace or node name already filled in (press Copy and run them yourself), the usual causes, and the title of the study note the hint comes from. Hints exist for Pending, ContainerCreating, Init, ImagePullBackOff, CrashLoopBackOff, OOMKilled, Evicted, config errors, Terminating or Unknown, a running pod that is not ready, a pod without a memory limit, and node not ready or under pressure. From the chain and app map: a Service whose selector matches no pods or that does not exist, a Deployment or StatefulSet with fewer ready replicas than wanted, and a Pending PersistentVolumeClaim. Where a command needs a name the page does not know (for example a container), it shows a line such as `CONTAINER=CHANGE_ME` first and uses `"$CONTAINER"` in the command: set it, then paste both lines. A hint never runs anything: the commands are text only.

When something changes that you did not expect, for example a pod that restarts many times, start from the app map, not from kubectl. The app map shows the blast radius at once: which other parts of the app depend on the broken part.

## How to demo it in an interview (about 10 minutes)

Before the interview: run deploy/demo/demo.sh reset, open the web app, log in, and open the servers overview. Keep the backup recording ready in case the live demo fails.

| Minutes | Show | Say (key points) |
|---|---|---|
| 0–1 | The board overview drawing | "I built a Rancher-style tool as a web app that runs inside the cluster it shows, behind Traefik with a cert-manager certificate. It reads through its own read-only ServiceAccount, so there is no cluster credential to store." |
| 1–2 | Cluster list → the cluster | "There is no registration step. Node agents prove their identity with short-lived projected ServiceAccount tokens, checked by TokenReview." |
| 2–4 | App map of url-shortener and flask-vue | "Kubemap builds this from the API: owner references, EndpointSlices, Traefik routes, environment variables and the CNPG operator. I can see that url-shortener goes through pgbouncer to a two-node Postgres cluster." |
| 4–6 | Servers overview and problem finder | "Kubernetes said the node was Ready while swap was full. The node agent reads PSI and maps each process to its pod through cgroups." Then tell the 28 Sep disk-pressure incident (STAR). |
| 6–8 | A practice lab: the 503 or the OOM kill | Start guided debugging on the broken pod. Read each step and its kubectl command aloud. Fix it, and show it turn healthy. |
| 8–9 | NetworkPolicy checker | "Can the frontend reach the backend? No, and this policy decides." Mention the deny-egress finding you fixed and the pod pool overlap. |
| 9–10 | Permissions and safety section | "Kubemap is read-only, enforced by Kubernetes RBAC: 581 checks pass, including no Secret reads, no exec, no escalate. I prove it in the home lab before any production install." |

Questions to expect, and where the answer is: "How does it find the links?" (app map, ARCHITECTURE.md). "What if the web app is hacked?" (permissions section: the limits are in Kubernetes, not in the web app). "Why not Prometheus?" (next section). "Tell me about an incident" (incident journal, 28 Sep).

## Kubemap compared with Prometheus and Grafana

They answer different questions, so you use both. Prometheus and Grafana answer "how much, and how did it change over time?" Kubemap answers "what is connected to what, what is broken, why, and what do I do now?"

|  | Prometheus + Grafana | Kubemap |
|---|---|---|
| Main data | Time series: numbers sampled every few seconds, kept for weeks | The live object graph: pods, Services, routes, policies, RBAC, operators, and their links |
| Best at | Trends, SLOs, capacity planning, alerting on thresholds | Relationships, root cause of one problem, the next step, and the kubectl command for it |
| Typical question | "Did p99 latency rise after the deploy yesterday?" | "Why does this app return 503 now, and which part is broken?" |
| Context | You build dashboards and write PromQL | Built in: it knows how Kubernetes objects relate |
| History | Long, detailed metric history | Short rollups, plus the incident journal for decisions |
| Actions | None: it observes | Guided fixes you run yourself; Kubemap never changes the cluster |
| Learning | You must already know what to look for | The scenario library and practice labs teach what to check and why |

How they fit together: Prometheus alerts you that something is wrong. Kubemap helps you find why and fix it. A later version can link each Kubemap finding to the matching Grafana panel. Compared with Rancher, Lens or k9s, Kubemap focuses less on editing resources and more on explaining relationships, debugging steps and decisions.

## Login safety: two-factor lockout

With TOTP on, 10 wrong second-factor codes (a recovery code counts too) within 15 minutes lock that account's second step for 15 minutes, from any address. While locked, even a correct code is refused. Open sessions are not affected, and the counter lives in memory, so a restart clears it. Failed logins from a shared address (all users behind the ingress look the same) get a higher limit of 50 per 15 minutes. Turning TOTP on ends the user's other sessions.

## Safety in one paragraph

The server uses one service account (`kubemap`): it can only read, plus create TokenReviews. The node agent has no Kubernetes API permissions. Both run as non-root with a read-only file system and no Linux capabilities. Kubernetes enforces these limits, so they hold even if the web app is broken into. The permission proof kit checks 581 dangerous actions and all must say no. See the permissions section on the Build board tab.
