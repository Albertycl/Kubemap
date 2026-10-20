# Kubemap interview demo script (10 minutes)

The 10-minute live demo from the board's User guide, with the exact clicks, the lab to break, what to say, and what to do if something fails. Each step was walked on 28 Sep 2026 against the pages on main, with k8server's real data (see "How this script was checked" at the end). The times are targets; the whole demo fits in 10 minutes only if you practise it twice.

Words in **bold** are the exact labels on the screen. `Code` is what you type.

---

## Before the interview (30 minutes before)

Do these in order. Stop at the first thing that fails and use its fallback.

| # | Where | Do | Expect |
|---|---|---|---|
| 1 | k8server | `df -h /` | At least 20% free. Below that, free space first (a disk under 15% makes the problem finder show a disk warning you'll have to explain). |
| 2 | k8server | `deploy/demo/demo.sh reset` then `deploy/demo/demo.sh status --wait` | About 1 minute. Ends with **all 9 scenarios broken as expected**. |
| 3 | k8server | `kubectl -n kubemap-system get pods` | The `kubemap` pod is `Running` and `1/1`. |
| 4 | Browser | Open `https://kubemap.albertycliu.xyz`, log in. | The cluster list shows your cluster **Active**. |
| 5 | Browser | Open the cluster, then **Applications**. | `url-shortener` and `flask-vue` are on the map, green. |
| 6 | Browser | **Journal**. If the 28 Sep incident is not there: **Import Markdown**, **Or paste the text**, paste `docs/incidents/2026-09-28-disk-pressure.md`. | The entry "disk pressure evicted every app on k8server", with an **Interview view** button. |
| 7 | Browser | Open these tabs, left to right, so you never wait for a page during the demo: 1 **Servers**, 2 **Problems**, 3 the cluster's **Applications**, 4 **Guided debugging**, 5 **NetworkPolicy checker**, 6 **Who can do what**, 7 the journal entry's **Interview view**. | All load. |
| 8 | Terminal | A terminal on k8server with a big font, in the repository folder. Paste the fix commands below into a text file, ready to copy. | |
| 9 | | Start the backup recording player, paused at 0:00, on the second screen. | |

Close chat apps and notifications. Zoom the browser to 125% so the interviewer can read it.

The fix commands you'll paste (from docs/demo-scenarios.md):

```bash
# 503 lab (main choice)
curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: demo-503.local' http://192.168.32.240/
kubectl -n kubemap-demo patch deploy shop-web --type json -p '[{"op":"replace","path":"/spec/template/spec/containers/0/readinessProbe/httpGet/path","value":"/"}]'
curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: demo-503.local' http://192.168.32.240/

# OOM lab (second choice)
kubectl -n kubemap-demo set resources deploy/oom -c report-builder --requests=memory=48Mi --limits=memory=48Mi
kubectl -n kubemap-demo logs deploy/oom
```

---

## The demo

### 0:00–1:00 · What it is (board tab)

**Show:** the board's overview drawing (tab 1).

**Say:** "I built a Rancher-style tool for Kubernetes. It is a web app that runs inside the cluster it shows, behind Traefik with a cert-manager certificate. It reads through its own ServiceAccount, which can only read. A small agent on every node reports node health to it."

### 1:00–2:00 · How it is deployed (cluster list)

**Click:** **Clusters** → "this cluster". There is no Add cluster step and no Accept.

**Say:**
- "Kubemap runs in the cluster as a Deployment. The cluster record is created by the server itself, and it reads the Kubernetes API with a read-only ServiceAccount."
- "Node agents don't need any secret: they prove who they are with short-lived Kubernetes ServiceAccount tokens, which the server checks with a TokenReview."
- "The server has one volume, so it runs on one node. That is a known limit; the node agents run on every node."

**Show (if time, 20 s):** `kubectl -n kubemap-node get pods -o wide` in the terminal. **Don't change anything.**

### 2:00–4:00 · The app map (tab 4)

**Click:** in **Find an app or workload** type `url-shortener`.

**Show:** the app from its Traefik route to the `url-shortener` Deployment, then the CNPG Pooler `url-shortener-pooler` (pgbouncer), then the Postgres cluster `url-shortener-db` with its two pods (the primary is marked), and Redis `redis-master` next to it.

**Say:** "The agent builds this from the API alone. Owner references give the pods, EndpointSlices the Services, the Traefik IngressRoutes the entry, environment variables and ConfigMaps the dependencies, and the CNPG operator the database. Here url-shortener goes through pgbouncer to a two-instance Postgres cluster."

**Then:** type `flask-vue`: the route `/` goes to the frontend and `/api` to the backend.

**Click:** **Logs** in flask-vue's header. **Show:** "Logs · flask-vue": the frontend and backend pods merged by time, one colour per pod, and the legend by part (frontend, backend). Click **only** next to a backend pod, then **Show all pods**. Tick **Follow new lines**.

**Say:** "One click instead of a `kubectl logs` per pod: every pod of the app in time order, so I see the frontend error and the backend line that caused it next to each other."

### 4:00–6:00 · Server health and the problem finder (tabs 2 and 3)

**Show tab 2 (Servers):** k8server with CPU, memory, **swap about 100%**, disk, and the pressure (PSI) verdict.

**Say:** "Kubernetes said this node was Ready, while swap was full and tasks were waiting for CPU. The node agent reads PSI from the kernel and maps every process to its pod through cgroups."

**Show tab 3 (Problems):** the critical findings (the lab crash loop, the exit 137 and the image pull), then the warnings, including **5 pods were evicted** and the disk and swap findings.

**Say:** "Each finding says what it means, why it matters, how to fix it and the command that proves the fix." Then **tell the 28 Sep incident** in STAR form. Switch to tab 8 (**Interview view**) only if you need the notes:
- **Situation:** every app stopped, and Kubernetes still said the node was Ready.
- **Task:** restore service and find why eviction continued after the disk recovered.
- **Action:** stopped the builds, freed disk in order of risk, read the kubelet config (5% threshold + 10% minimum reclaim), waited instead of changing thresholds, fixed the lost image.
- **Result:** back after about 40 minutes, with three follow-ups: a disk guard for builds, a registry, and Kubemap health checks that warn before the threshold. "The 5 evicted pods on that page are the records of that day."

### 6:00–8:00 · A practice lab: the 503 (tabs 5 and terminal)

**Terminal:** run the first `curl`: **503**.

**Click (tab 5):** **Guided debugging**. Under **A pod** it lists the pods with problems; click `shop-web-…` (namespace `kubemap-demo`).

**Show:** the checklist from top to bottom:
1. **status**: problem (Running, not ready);
2. **events**: problem (the readiness probe keeps failing);
3. **exit code**: ok (it never crashed);
4. **service endpoints**: problem, "Service kubemap-demo/shop-web: no ready pods".

**Say:** read the kubectl command under each step aloud. "A readiness failure never restarts the container, so nothing looks crashed. The pod is removed from the Service's endpoints, and Traefik answers 503. The probe asks for `/healthz`, which nginx doesn't serve."

**Terminal:** paste the `kubectl patch`, wait about 5 seconds, run `curl` again: **200** (it took 8 seconds in the test).

**Click:** **← Guided debugging**. `shop-web` is no longer in the list of pods with problems. To show it green: tick **Show all pods, not only those with problems** and click the new `shop-web-…` pod; every step is **ok**. (The patch replaced the pod, so **Run again** on the old page would only say the pod is gone.)

**Say:** "I fix it with kubectl because this lab isn't managed by Argo CD; for an Argo CD app the fix goes into Git. Kubemap itself is read-only by design: it shows the cause and the fix, and a person makes the change."

### 8:00–9:00 · NetworkPolicy checker (tab 6)

**Click:**
- **Can A reach B?**
- **From:** **A pod**, namespace `kubemap-demo`, pod `web-…`
- **To:** **A Service**, namespace `kubemap-demo`, Service `api`, port 80
- **Check**

**Show:** **blocked**, and the deciding policy: `api-allow-monitoring-only` isolates the `api` pod, and no ingress rule matches `web`.

**Say:** "The answer names the policy that decides. That's the question behind most 'it worked yesterday' tickets after someone locks down a namespace." If there's time, open **Namespace matrix** for `kubemap-demo`.

### 9:00–10:00 · Safety (tab 7)

**Click:** **Who can do what** → **Who can …** → **Read Secrets** → namespace `sre-lab-app` → **Find who can**.

**Show:** the list: `system:masters`, Argo CD, cert-manager and others. **The Kubemap agent is not in it.**

**Say:** "The agent can only read, and Kubernetes RBAC enforces that, not my code. It can't read Secrets, can't exec and can't escalate. A proof script checks 581 dangerous actions, and all say no. So even if the web app is broken into, the cluster limits hold. There are no fix buttons and no operator mode: Kubemap is read-only by design, so a fix is made with kubectl or in Git."

**End:** "Questions?"

---

## If something fails

| What fails | Do this | Say |
|---|---|---|
| The web app does not load | `kubectl -n kubemap-system get pods`; `kubectl -n kubemap-system rollout restart deploy/kubemap`. Still down after 30 s: switch to the recording. | "Let me show the recording while it starts; the service restarts by itself after a crash." |
| The cluster shows **Disconnected** | Keep going with the recording for the live parts. After the interview, see the operator guide, section 4. | "The agent reconnects by itself; this is exactly the 'node down vs agent down' case Kubemap tells apart." |
| A node shows **Node agent reconnecting** | Wait 30 s. It is normal right after the web app starts. | |
| The app map is empty or slow | Reload once. Then use tab 3 (Problems) and continue. | |
| The 503 lab already answers 200 | Someone fixed it: run `deploy/demo/demo.sh reset` in the terminal (about 1 minute), and meanwhile do the NetworkPolicy step first. Or use the OOM lab. | |
| **OOM lab instead** (second choice) | **Guided debugging** → under **A pod**, click `oom-…`: **exit code** problem (137), **resources** problem ("killed ... when it reached its 24Mi memory limit"). Fix: the `kubectl set resources` line; after about 5 s `kubectl logs deploy/oom` prints `report done, 33554432 bytes`. | "Exit 137 is SIGKILL. On this node the reason once showed Error instead of OOMKilled, so I trust exit 137 plus the limit, and the kernel log as proof." |
| `curl` to 192.168.32.240 times out | Traefik's address is unreachable from where you are. Show the Service step in guided debugging instead: **no ready pods** is the same proof. | |
| The patch fails (typo) | Copy it again from the prepared file. Never type it live. | |
| Everything fails | Play the recording from the start of the current section. | "Here is the same flow recorded yesterday." |

## After the interview

Put the labs back for the next time: `deploy/demo/demo.sh reset`. It takes about 1 minute and changes only the namespace `kubemap-demo`.

## Questions to expect

| Question | Short answer | Where to show it |
|---|---|---|
| How does it find the links between parts of an app? | From the API only: owner references, EndpointSlices, routes, env vars and ConfigMaps, operator CRDs. No traffic sniffing. | App map; ARCHITECTURE.md |
| What if the web app is hacked? | The limits are in Kubernetes RBAC, not in the web app: read-only, no Secrets, no exec. Node agents are accepted only with a short-lived, audience-bound token. | Who can do what; the permission proof |
| Why not Prometheus and Grafana? | They answer "how much, over time". Kubemap answers "what is connected, what is broken, why, what now". Use both. | User guide, comparison table |
| What if a node agent's token leaks? | It has audience `kubemap-portal`, lives one hour, is bound to its pod and node, and gives no Kubernetes API rights. The server accepts it only after a TokenReview. | Operator guide |
| Tell me about an incident. | 28 Sep, disk pressure: STAR above. | Journal, **Interview view** |
| How would this scale to many clusters? | Today it is one server per cluster. Several clusters would need a design of their own (the server would need a way to reach each cluster). The next steps are a registry for the image and a database that does not pin the server to one node. | Operator guide |

---

## How this script was checked

On 28 Sep 2026 (18:30–18:45 GMT+8), with the code on main:

- **The pages:**
  - The earlier version of the script was checked on 28 Sep 2026 by running the server locally against k8server, read-only. The web-in-cluster version has not been rehearsed in a browser yet.
  - Everything the demo shows came back as the script says: the cluster list shows the cluster Active; the app map for url-shortener (Pooler → CNPG cluster with 2 pods, Redis) and flask-vue (`/` and `/api`); the servers overview (swap 99.5%, pressure on CPU); the problem finder (crash loop, exit 137 and image pull critical; 5 evicted pods); guided debugging on `shop-web` (status, events and Service endpoints are problems) and `oom` (exit code and resources are problems, "24Mi"); the NetworkPolicy check web → api:80 (blocked by `api-allow-monitoring-only`); "Who can … Read Secrets in sre-lab-app" (22 subjects); the imported 28 Sep journal entry with its STAR section.
  - The button and field labels come from the page sources on main.
- **The labs** (namespace `kubemap-demo` only):
  - The 503 fix turned 503 into 200 after 8 s, and guided debugging then showed every step ok.
  - The OOM fix printed `report done` after about 9 s.
  - Both were put back broken, then `demo.sh reset` + `status --wait` took 65 s and ended with all 9 broken.
- **Not checked:**
  - The deployed server and a browser (there's no browser here), so the screen layout, the zoom and the backup recording are yours to try in a rehearsal.
  - The Kubemap agent missing from the Read Secrets list needs the real install: install Kubemap first, then check the list.
