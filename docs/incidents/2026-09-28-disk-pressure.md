# Incident: disk pressure evicted every app on k8server

Date: 28 September 2026 (times in GMT+8). Single-node K3s cluster `k8server`.

## Summary

Build jobs filled the root disk for a short time. The kubelet saw less than 5% free space and started to evict pods. The disk recovered to 13.7% free, but the kubelet continued to evict, because it evicts until 15% is free. All application pods stopped for about 40 minutes. After the pods returned, url-shortener could not start, because the kubelet had also deleted its local image.

## Timeline

| Time | Event |
|---|---|
| 07:32 | DiskPressure=True. The eviction manager starts to reclaim ephemeral-storage. |
| 07:32–07:47 | Pods are evicted in sre-lab-app, argocd, monitoring, cert-manager and others. The tigera-operator Deployment loops: each new pod is evicted at once (about 2,840 dead pods). |
| 07:39 | A build pane notices the evictions and stops its own builds. |
| 07:44 | The Go build cache is cleared (7.4 GB). Free space: 21 GB (22%). |
| 07:46 | The Docker build cache is pruned (12.2 GB). Free space: 32 GB. |
| 07:49 | DiskPressure=False, after the 5-minute pressure transition period. The pods are scheduled again. |
| 08:05 | url-shortener fails with ErrImageNeverPull. Its image was local only (`imagePullPolicy: Never`), and image garbage collection deleted it. |
| 08:10 | The image is imported again from Docker into K3s containerd. |
| 08:12 | All pods run. |

## Root cause

- The kubelet settings are `evictionHard: nodefs.available<5%, imagefs.available<5%` and `evictionMinimumReclaim: 10%`. After a trigger, the kubelet continues to evict until 5% + 10% = 15% is free.
- Build caches (Go 7.4 GB, Docker 14.9 GB) share the root disk with containerd and the pods.
- The url-shortener image existed only on the node, with no registry to pull it from again.

## How it was found

`kubectl describe node` showed DiskPressure. `journalctl -u k3s` showed "attempting to reclaim ephemeral-storage". The kubelet `configz` endpoint showed the thresholds and the minimum reclaim. The kubelet `stats/summary` endpoint showed the available bytes.

## Fixes and follow-ups

- Done: build panes check `df -h /` before each build and stop below 18% free.
- Done: dead pod records deleted.
- To do: push url-shortener to a registry instead of `imagePullPolicy: Never`.
- To do: move build caches off the root disk, or give the node a separate disk for `/var/lib/rancher`.
- To do (Kubemap): the node health checks (KM-61) warn before free disk reaches the eviction threshold. They also show the minimum-reclaim target, so the operator knows how much space to free.

## Actions and decisions

| # | Decision | Why | Trade-off |
|---|---|---|---|
| 1 | Stop all build jobs first. | Stop making the problem worse before you fix it. | Development paused for about 20 minutes. |
| 2 | Clear the Go build cache first (7.4 GB). | It belonged only to this project, it is only a cache, and it rebuilds by itself. It was the fastest safe space, with no approval needed from other owners. | The next builds were slower once. |
| 3 | Prune the Docker build cache (12.2 GB), after the owner approved. | It gave a safety margin well above the 15% target. | Part of it belonged to other projects, so the owner had to decide. Their next image builds are slower once. |
| 4 | Do not restart k3s, and do not change the eviction thresholds. | The thresholds worked as designed. Changing them during an incident removes the node's protection. The real problem was disk use. | Recovery waited for the 5-minute pressure transition period. |
| 5 | Delete the dead pod records only after recovery. | They cost no disk or CPU, so they were not urgent. Deleting them later kept the evidence available during the incident. | kubectl output was noisy for 30 minutes. |
| 6 | Re-import the url-shortener image, instead of changing `imagePullPolicy`. | This restored service in minutes without changing the app's manifests. The right long-term fix, a registry, needs a separate change. | The weakness (a local-only image) is still there until the follow-up. |

## Interview summary (STAR)

- **Situation:** On a single-node K3s cluster, every app stopped. Kubernetes still reported the node as Ready.
- **Task:** Restore service and find out why eviction continued after the disk recovered.
- **Action:** I stopped the builds and freed disk in order of risk (my own cache first, shared caches after the owner approved). I read the kubelet `configz` endpoint and found the 5% threshold plus the 10% minimum reclaim. I waited for the pressure to clear instead of changing thresholds, then fixed the ErrImageNeverPull that followed.
- **Result:** All pods ran again after about 40 minutes. Follow-ups: a disk guard for builds, a registry instead of local-only images, and Kubemap health checks that warn before the threshold.

## What this shows

Kubernetes reported "Ready" the whole time. The only signals were the DiskPressure condition, evicted pods and kubelet logs. Kubemap's server health page and problem finder exist to show this situation early and explain it.
