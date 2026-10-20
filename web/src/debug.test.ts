// Run with: npm test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { appPods, mapPodLevel, openByDefault, tally } from "./debug.ts";
import type { DebugStep, MapApp, MapPod } from "./api.ts";

const pod = (name: string, status: string, ready: boolean, restarts = 0): MapPod => ({ name, status, ready, restarts });

test("map pod levels", () => {
  assert.equal(mapPodLevel(pod("a", "Running", true)), "ok");
  assert.equal(mapPodLevel(pod("a", "Running, not ready", false)), "warn");
  assert.equal(mapPodLevel(pod("a", "CrashLoopBackOff (OOMKilled)", false)), "fail");
  assert.equal(mapPodLevel(pod("a", "Succeeded", false)), "ok");
});

test("an app's pods, worst first, each once", () => {
  const app = {
    id: "argocd/shop", name: "shop", namespace: "shop", source: "argocd", health: "Degraded", state: "fail",
    components: [
      { id: "Deployment/shop/web", kind: "Deployment", namespace: "shop", name: "web", tier: "frontend", tierReason: "", healthy: true,
        pods: [pod("web-1", "Running", true, 9), pod("web-2", "Running, not ready", false, 1)] },
      { id: "Deployment/shop/api", kind: "Deployment", namespace: "shop", name: "api", tier: "backend", tierReason: "", healthy: false,
        pods: [pod("api-1", "CrashLoopBackOff", false, 3), pod("api-2", "CrashLoopBackOff", false, 12)] },
      { id: "Service/shop/api", kind: "Service", namespace: "shop", name: "api", tier: "backend", tierReason: "", healthy: true,
        pods: [pod("api-1", "CrashLoopBackOff", false, 3)] },
    ],
  } as unknown as MapApp;
  const got = appPods(app);
  assert.deepEqual(got.map((p) => p.name), ["api-2", "api-1", "web-2", "web-1"]);
  assert.equal(got[0].component, "api");
  assert.equal(got[0].namespace, "shop");
});

test("problem and cause steps start open; tally", () => {
  const s = (key: string, verdict: DebugStep["verdict"]): DebugStep => ({ key, title: key, verdict, finding: "", commands: [] });
  const steps = [s("status", "ok"), s("logs", "problem"), s("resources", "unknown"), s("netpol", "problem")];
  assert.equal(openByDefault(steps[0], "netpol"), false);
  assert.equal(openByDefault(steps[1], "netpol"), true);
  assert.equal(openByDefault(steps[3], "netpol"), true);
  assert.equal(tally(steps), "2 problems, 1 ok, 1 not checked");
  assert.equal(tally([s("a", "problem")]), "1 problem");
});
