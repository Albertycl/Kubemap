# Demo scenarios (namespace `kubemap-demo`)

Nine small workloads that fail on purpose, for practice and the live interview demo: five from KM-71 (approved A9) and four from KM-87. All of them are in the namespace `kubemap-demo`. It is the only namespace Kubemap's builders may change.

```
deploy/demo/demo.sh create          # namespace + all nine scenarios, broken
deploy/demo/demo.sh status --wait   # waits until all nine are broken (about 1 minute)
deploy/demo/demo.sh reset           # undo your fixes: back to the broken state
deploy/demo/demo.sh delete          # delete the whole namespace
deploy/demo/demo.sh budget          # memory limits per container and in total
```

- **Budget:** the memory limits add up to 118 MiB: 66 MiB for the KM-71 labs, 52 MiB for the KM-87 labs, where the reporter's 16 MiB counts only while a run is active. A ResourceQuota `demo-budget` caps the namespace at 160 MiB of limits and 16 pods, so no fix or mistake can use more. It was 100 MiB before KM-87, which allows up to 160 MiB. With every fix applied, the total is at most 142 MiB.
- **Safety:** every Deployment uses `strategy: Recreate`, so a rollout never needs extra quota. The namespace enforces Pod Security `restricted`: every container runs as UID 65534 with a read-only root filesystem and no capabilities. `demo.sh` refuses to apply any manifest object that is outside `kubemap-demo`.
- **Images:** `busybox:1.36`, `traefik/whoami:v1.10`, `nginxinc/nginx-unprivileged:1.27-alpine` (nginx as non-root on port 8080, because Pod Security `restricted` forbids root), and `curlimages/curl:8.10.1` (busybox `wget` cannot do TLS with the API server).
- **Traefik:** the 503 and 502 labs are reached through Traefik (`kube-system/traefik`, entry point `web`) with the hosts `demo-503.local` and `demo-502.local`. From the VM: `curl -H 'Host: demo-503.local' http://192.168.32.240/`. `demo.sh status` does the same with the LoadBalancer address it finds.
- **Labels:** every object has `app.kubernetes.io/part-of=kubemap-demo`. Each scenario's objects also have `kubemap.io/scenario=<name>`.

In the commands below, `k` means `kubectl -n kubemap-demo`.

Tip: right after a restart, `k logs … --previous` can fail once with `unable to retrieve container logs`, because the old container is being removed. Run it again after a few seconds. While the pod waits in back-off, plain `k logs` also shows the last run.

---

## 1. `oom`: container killed for using too much memory

**Symptom:** pod `oom-…` is in `CrashLoopBackOff`, its restart count keeps growing, and its log stops after `building monthly report (needs 32 MiB)`. It never prints `report done`.

**Root cause:** the container `report-builder` has a memory limit of 24 MiB, but the job needs a 32 MiB buffer. When the container reaches its limit, the kernel's memory cgroup OOM killer kills it with SIGKILL, so the exit code is 137 (128 + 9).

**Find it:**
```
k get pods -l app.kubernetes.io/name=oom
k describe pod -l app.kubernetes.io/name=oom      # Last State: Terminated, Exit Code: 137
k logs deploy/oom --previous                      # stops before "report done"
k get deploy oom -o jsonpath='{.spec.template.spec.containers[0].resources}'
journalctl -k | grep -i 'memory cgroup out of memory'   # on the node: "Killed process … (awk)"
```

**Note for this cluster:** `kubectl describe` normally shows `Reason: OOMKilled`, exit code 137. On 2026-09-28, before k3s was restarted at 12:10 GMT+8, k8server (k3s v1.34.8, containerd 2.2.3-k3s1) showed **`Reason: Error`** with exit code 137 for the same kill, although the kernel log proved a memory cgroup OOM kill. Since the restart it shows `OOMKilled` again. So **exit code 137 plus a memory limit close to the job's needs** is the clue, and the kernel log is the proof. This is a good interview point: never rely only on the `reason` field.

**Fix:**
```
k set resources deploy/oom -c report-builder --requests=memory=48Mi --limits=memory=48Mi
k logs deploy/oom          # "report done, 33554432 bytes"
```

---

## 2. `crashloop`: a required setting is missing

**Symptom:** pod `checkout-…` is in `CrashLoopBackOff` with exit code 1.

**Root cause:** the app needs `DATABASE_URL`. It reads it from ConfigMap `checkout-config` (`optional: true`, so the pod starts). But the ConfigMap only has `DATABASE_HOST` and `LOG_LEVEL`. The app exits at once with `FATAL: required setting DATABASE_URL is not set`.

**Find it:**
```
k get pods -l app.kubernetes.io/name=checkout
k logs deploy/checkout --previous                  # FATAL: required setting DATABASE_URL is not set
k get deploy checkout -o jsonpath='{.spec.template.spec.containers[0].env}'
k get configmap checkout-config -o yaml            # no DATABASE_URL key
```

**Fix:** add the key, then restart. Environment variables are read only when the container starts.
```
k patch configmap checkout-config --type merge -p '{"data":{"DATABASE_URL":"postgres://checkout@postgres.kubemap-demo.svc:5432/checkout"}}'
k rollout restart deploy/checkout
```

---

## 3. `netpol`: a NetworkPolicy blocks frontend → backend

**Symptom:** both pods are Running and Ready, but the frontend `web` logs `ERROR GET http://api.kubemap-demo.svc/api failed: wget: download timed out` every 5 s.

**Root cause:** NetworkPolicy `api-allow-monitoring-only` selects the backend `api`. It allows ingress on port 8080 only from pods labelled `role=monitoring`. Because a policy selects `api`, all other ingress to `api` is denied, including the frontend's. Calico drops the packets, so the client sees a timeout, not "connection refused".

**Find it:**
```
k logs deploy/web --tail=5                         # ERROR … timed out
k get endpointslices -l kubernetes.io/service-name=api   # the Service HAS a ready endpoint
k get networkpolicy
k describe networkpolicy api-allow-monitoring-only # PodSelector app.kubernetes.io/name=api; From: role=monitoring
k get pod -l app.kubernetes.io/name=web --show-labels   # web has no role=monitoring
k exec deploy/web -- wget -q -T 3 -O - http://api.kubemap-demo.svc/api   # times out
```

**Fix:** allow the frontend in the policy.
```
k patch networkpolicy api-allow-monitoring-only --type json \
  -p '[{"op":"add","path":"/spec/ingress/0/from/-","value":{"podSelector":{"matchLabels":{"app.kubernetes.io/name":"web"}}}}]'
k logs deploy/web --tail=2                         # "… GET http://api.kubemap-demo.svc/api ok"
```

---

## 4. `badsvc`: a Service whose selector matches no pods

**Symptom:** pod `catalog-…` is Running and Ready, but Service `catalog` has no endpoints. Every call to `catalog` fails with "connection refused" or a timeout.

**Root cause:** the Service selects `app.kubernetes.io/name=catalog-api`, but the pods are labelled `app.kubernetes.io/name=catalog`.

**Find it:**
```
k get endpointslices -l kubernetes.io/service-name=catalog   # ENDPOINTS <unset>
k get service catalog -o jsonpath='{.spec.selector}'          # {"app.kubernetes.io/name":"catalog-api"}
k get pods --show-labels -l app.kubernetes.io/name=catalog    # label is "catalog"
k get pods -l app.kubernetes.io/name=catalog-api              # No resources found
```

**Fix:**
```
k patch service catalog --type merge -p '{"spec":{"selector":{"app.kubernetes.io/name":"catalog"}}}'
k get endpointslices -l kubernetes.io/service-name=catalog    # one ready address
```

---

## 5. `pending`: a pod asks for more CPU than the node has

**Symptom:** pod `batch-worker-…` stays `Pending` and never starts.

**Root cause:** it requests 8 CPUs, but k8server has 4 allocatable CPUs. The scheduler reports `0/1 nodes are available: 1 Insufficient cpu`.

**Find it:**
```
k get pods -l app.kubernetes.io/name=batch-worker
k describe pod -l app.kubernetes.io/name=batch-worker   # Events: FailedScheduling … Insufficient cpu
kubectl get node k8server -o jsonpath='{.status.allocatable.cpu}'   # 4
kubectl describe node k8server | grep -A6 'Allocated resources'
```

**Fix:**
```
k set resources deploy/batch-worker -c worker --requests=cpu=50m --limits=cpu=100m
```

---

## 6. `readiness503`: the readiness probe fails, so Traefik answers 503

**Symptom:** `curl -H 'Host: demo-503.local' http://192.168.32.240/` answers **503 Service Unavailable**. The pod `shop-web-…` is `Running`, but `READY 0/1`, and it never restarts.

**Root cause:** the readiness probe asks nginx for `/healthz`, which nginx doesn't serve, so it answers 404. A pod that fails readiness is removed from the Service's endpoints, so the Service `shop-web` has no ready endpoints. Traefik (with `allowEmptyServices`) keeps the route and answers 503. A readiness failure never restarts the container; only a liveness failure does. That's why nothing looks "crashed".

**Find it:**
```
curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: demo-503.local' http://192.168.32.240/   # 503
k get pods -l app.kubernetes.io/name=shop-web                   # Running, READY 0/1, RESTARTS 0
k describe pod -l app.kubernetes.io/name=shop-web | grep -A3 Readiness   # Readiness probe failed: HTTP probe failed with statuscode: 404
k get endpointslices -l kubernetes.io/service-name=shop-web -o wide      # the address is there, but not ready
k get ingressroute shop-web -o yaml                              # Host(`demo-503.local`) -> shop-web:80
k exec deploy/shop-web -- wget -qO- http://127.0.0.1:8080/healthz        # 404 Not Found: the path does not exist
```

**Fix:** point the probe at a path the app serves.
```
k patch deploy shop-web --type json -p '[{"op":"replace","path":"/spec/template/spec/containers/0/readinessProbe/httpGet/path","value":"/"}]'
curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: demo-503.local' http://192.168.32.240/   # 200
```

**Prevention:** give every app a real health endpoint, and test the probe path in CI (for example `curl -f localhost:PORT/healthz` in the image test). Alert on Services with zero ready endpoints (Kubemap KM-77, `kube_endpoint_address_available` in Prometheus). Keep readiness (may I get traffic?) and liveness (should I be restarted?) separate.

---

## 7. `upstream502`: the upstream closes the connection, so Traefik answers 502

**Symptom:** `curl -H 'Host: demo-502.local' http://192.168.32.240/` answers **502 Bad Gateway**. The pod `orders-…` is `Running` and `READY 1/1`, so Kubernetes shows nothing wrong. Its log shows a `panic: … nil pointer dereference [handler /api/orders]` for every request.

**Root cause:** the container's command starts `nc` (netcat), not a web server. It accepts each TCP connection and runs a "handler" that only logs a panic and exits, so the connection closes without any HTTP answer. The readiness probe is a `tcpSocket` probe: it only checks that the port accepts connections, so the pod counts as Ready and gets traffic. Traefik reached the upstream but got no valid answer, so it returns **502** (and **503** would mean "no upstream at all", as in lab 6).

**Find it:**
```
curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: demo-502.local' http://192.168.32.240/   # 502
k get pods -l app.kubernetes.io/name=orders                     # Running, READY 1/1
k logs deploy/orders --tail=3                                   # panic: runtime error … [handler /api/orders]
k get deploy orders -o jsonpath='{.spec.template.spec.containers[0].command}'   # nc -lk -p 8080 -e …: not an HTTP server
k get deploy orders -o jsonpath='{.spec.template.spec.containers[0].readinessProbe}'   # tcpSocket only
kubectl -n kube-system logs deploy/traefik --tail=20 | grep -i orders   # Traefik's side (if access logs are on)
k run tmp-curl --rm -it --restart=Never --image=busybox:1.36 --overrides='{"spec":{"securityContext":{"runAsNonRoot":true,"runAsUser":65534,"seccompProfile":{"type":"RuntimeDefault"}},"containers":[{"name":"tmp-curl","image":"busybox:1.36","command":["wget","-S","-O-","http://orders"],"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}},"resources":{"limits":{"memory":"8Mi"}}}]}}'   # wget: error getting response: Connection reset by peer
```

**Fix:** run the real server command.
```
k patch deploy orders --type json -p '[{"op":"replace","path":"/spec/template/spec/containers/0/command","value":["sh","-c","mkdir -p /www && echo '"'"'{\"orders\":[]}'"'"' > /www/index.html && exec httpd -f -v -p 8080 -h /www"]}]'
curl -s -H 'Host: demo-502.local' http://192.168.32.240/   # {"orders":[]}  (200)
```

**Prevention:** use an HTTP readiness probe on a real endpoint (`httpGet`), not `tcpSocket`, for HTTP services: then a broken handler takes the pod out of the Service before users see 502s. Watch the 5xx rate per route in Traefik's metrics. Run a smoke test (`curl -f`) after each rollout.

---

## 8. `imagepull`: the image tag does not exist

**Symptom:** pod `invoice-…` shows `ErrImagePull`, then `ImagePullBackOff`, and never starts.

**Root cause:** the Deployment asks for `busybox:1.36.99`. That tag does not exist on Docker Hub, so the pull fails with "not found", and the kubelet retries with a growing wait (up to 5 minutes).

**Find it:**
```
k get pods -l app.kubernetes.io/name=invoice                    # ErrImagePull / ImagePullBackOff
k describe pod -l app.kubernetes.io/name=invoice | grep -A6 Events   # Failed to pull image "busybox:1.36.99": … not found
k get deploy invoice -o jsonpath='{.spec.template.spec.containers[0].image}'   # busybox:1.36.99
k get events --field-selector reason=Failed --sort-by=.lastTimestamp | tail -3
```
The message tells the causes apart: `not found` means a wrong name or tag; `unauthorized` or `denied` means credentials (`imagePullSecrets`); `i/o timeout` or `dial tcp` means the node cannot reach the registry; `toomanyrequests` means a Docker Hub rate limit.

**Fix:**
```
k set image deploy/invoice invoice=busybox:1.36
k rollout status deploy/invoice
```

**Prevention:** pin images by tag and digest, and check in CI that the image exists (`crane manifest`, `docker manifest inspect`) before the manifest is merged. Promote the same image from test to production, rather than typing tags by hand. Alert on pods in `ImagePullBackOff` for more than a few minutes (Kubemap KM-61 `pod.image-pull`).

---

## 9. `rbacforbidden`: the reporter job gets 403 Forbidden

**Symptom:** the CronJob `reporter` fails every run (`demo.sh create` also starts one run, `reporter-first`, at once). Its log shows `ERROR: GET pods in sre-lab-app: HTTP 403` and the API answer `pods is forbidden: User "system:serviceaccount:kubemap-demo:reporter" cannot list resource "pods" in API group "" in the namespace "sre-lab-app"`.

**Root cause:** the job calls the Kubernetes API with its ServiceAccount token (`reporter`), and that ServiceAccount has no permission. RBAC denies everything that is not granted. It also asks about the wrong namespace: `TARGET_NAMESPACE` in the ConfigMap `reporter-config` is `sre-lab-app`, but the report is about kubemap-demo.

**Find it:**
```
k get cronjob,jobs -l app.kubernetes.io/name=reporter           # jobs with COMPLETIONS 0/1, Failed
k logs job/reporter-first                                      # HTTP 403 … cannot list resource "pods" … "sre-lab-app"
k get cronjob reporter -o jsonpath='{.spec.jobTemplate.spec.template.spec.serviceAccountName}'   # reporter
kubectl auth can-i list pods -n sre-lab-app --as=system:serviceaccount:kubemap-demo:reporter      # no
kubectl auth can-i --list -n kubemap-demo --as=system:serviceaccount:kubemap-demo:reporter        # only the basics every account has
k get rolebindings -o wide                                     # nothing binds reporter
k get configmap reporter-config -o yaml                        # TARGET_NAMESPACE: sre-lab-app
```

**Fix (kubemap-demo only; grant nothing in any other namespace, and no ClusterRole binding):**
```
k create role pod-reader --verb=get,list --resource=pods
k create rolebinding reporter-pod-reader --role=pod-reader --serviceaccount=kubemap-demo:reporter
k patch configmap reporter-config --type merge -p '{"data":{"TARGET_NAMESPACE":"kubemap-demo"}}'
kubectl auth can-i list pods -n kubemap-demo --as=system:serviceaccount:kubemap-demo:reporter   # yes
kubectl auth can-i list pods -n sre-lab-app  --as=system:serviceaccount:kubemap-demo:reporter   # still no
k create job reporter-manual-1 --from=cronjob/reporter          # run it now
k logs job/reporter-manual-1                                    # report: N pods in kubemap-demo
```

**Prevention:** give each workload its own ServiceAccount with the least rights it needs, written next to the workload (Role and RoleBinding in the same manifests), and test it with `kubectl auth can-i --as=…` in CI. Prefer a Role in the app's own namespace over a ClusterRole. Alert on failed Jobs (`kube_job_status_failed`). A 403 in an app log is always an RBAC or identity question, never a network one.

---

## Checking the budget
```
deploy/demo/demo.sh budget
kubectl -n kubemap-demo get resourcequota demo-budget
```
