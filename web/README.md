# web

React + TypeScript UI for the portal (A2). Built with Vite into `web/dist/` and embedded into the `kubemap-portal` server binary by `web/embed.go`.

## Build

```
make web          # npm ci (when package-lock.json changed) + type-check + vite build
make portal       # embeds whatever is in web/dist
```

`make build` and `make all` run `make web` first. Without Node.js the portal still compiles and shows a "UI not built" page at `/`.

## Develop

Run the server on `http://127.0.0.1:8080` (for example `bin/kubemap-portal -data-dir /tmp/km`, with a kubeconfig that reaches a cluster), then `cd web && npm run dev`. Vite proxies `/api` and `/v1` to it.

## Pages (KM-20)

- **Login** and **Set a new password** (forced after the first login, KM-16).
- **Clusters** (`#/`): the cluster with its status, Kubernetes version, nodes and last contact. It refreshes every 5 s.
- **Cluster** (`#/clusters/<id>`): the overview, with links to the other pages.

## Application map (KM-53)

- **Application map** (`#/clusters/<id>/apps`, linked from the cluster page): every application the agent found, worst first. Each app is a card with one column per tier (entry → frontend → backend → database → cache). Boxes show the workload's status, one square per pod (green ready, amber starting or not ready, red failing) and a restart badge from 5 restarts. Arrows are the links the agent found in configuration; a broken link is red and dashed. Links to other apps are listed under the map with their evidence (route, env var, ConfigMap key, file, operator). Click a box for its pods, routing rules and every link with its evidence.
- Filters: namespace, search, and system apps (hidden by default). Objects no app owns are listed under "Not part of any application".
- **One app** (`#/clusters/<id>/apps/<app id>`): the same card alone, asked from the agent with `appmap.app`.
- The page refreshes every 15 s.

## Pods and logs (KM-80)

- **Pods** (`#/clusters/<id>/pods`, one namespace: `#/clusters/<id>/pods/<ns>`): status as `kubectl get pods` shows it, coloured; ready containers, restarts, node, age. A namespace picker, search, and "only pods with problems". It refreshes every 10 s.
- **Logs** (`#/clusters/<id>/namespaces/<ns>/pods/<pod>/logs`): container picker (init containers marked), last 100 to 10,000 lines, the previous run (before the last restart), timestamps, line wrap, and **Follow live**. Terminal colour codes are removed. The view keeps the newest 2 MB and stays at the bottom unless you scroll up. When a stream ends, the reason is shown (for example the kubelet cannot follow the file).
- Pod names in the application map's detail panel open the pod's logs.

Routes use the URL hash, so the server serves only `/` and `/assets/*`. The page has a strict Content Security Policy (only the portal's own scripts, styles and API).
