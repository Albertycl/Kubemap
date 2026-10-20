// Run with: npm test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { findingSignals, hintsPath, objectSignals, podNeedsHint, podSignals } from "./hints.ts";

test("hints path carries only what is set", () => {
  assert.equal(
    hintsPath({ kind: "Pod", namespace: "shop", name: "web-1", status: "CrashLoopBackOff" }),
    "/api/hints?kind=Pod&name=web-1&namespace=shop&status=CrashLoopBackOff",
  );
  assert.equal(hintsPath({ kind: "Node", name: "n1", rules: ["node.not-ready", "node.pressure.cpu"] }),
    "/api/hints?kind=Node&name=n1&rule=node.not-ready&rule=node.pressure.cpu");
});

test("only pods that are not fine offer the panel", () => {
  const p = (status: string, ready: number, total: number) => ({ status, ready, total });
  assert.equal(podNeedsHint(p("Running", 1, 1)), false);
  assert.equal(podNeedsHint(p("Completed", 0, 1)), false);
  assert.equal(podNeedsHint(p("Running", 0, 1)), true);
  assert.equal(podNeedsHint(p("Pending", 0, 1)), true);
  assert.equal(podNeedsHint(p("CrashLoopBackOff", 0, 1)), true);
});

test("signals from a pod row and from findings", () => {
  assert.equal(podSignals({ namespace: "a", name: "p", status: "Running", ready: 0, total: 1 }).notReady, true);
  assert.equal(podSignals({ namespace: "a", name: "p", status: "Pending", ready: 0, total: 1 }).notReady, false);
  assert.deepEqual(findingSignals({ object: "Pod/a/p", rule: "pod.image-pull" }), { kind: "Pod", namespace: "a", name: "p", rules: ["pod.image-pull"] });
  assert.deepEqual(findingSignals({ object: "Node/n1", rule: "node.not-ready" }), { kind: "Node", name: "n1", rules: ["node.not-ready"] });
  assert.equal(findingSignals({ object: "Namespace/a", rule: "x" }), undefined);
  assert.equal(findingSignals({ object: "Cluster", rule: "x" }), undefined);
});

test("pod row passes node and the container that is not ready", () => {
  const s = podSignals({
    namespace: "a",
    name: "p",
    status: "Init:0/1",
    ready: 0,
    total: 2,
    node: "n1",
    containers: [
      { name: "app", ready: true },
      { name: "side", ready: false },
      { name: "wait-db", init: true, ready: false },
    ],
  });
  assert.equal(s.node, "n1");
  assert.equal(s.container, "side");
  assert.equal(s.initContainer, "wait-db");
  assert.equal(
    hintsPath(s),
    "/api/hints?kind=Pod&name=p&namespace=a&status=Init%3A0%2F1&node=n1&container=side&initContainer=wait-db",
  );
  assert.equal(podSignals({ namespace: "a", name: "p", status: "Pending", ready: 0, total: 1 }).container, undefined);
});

test("signals of chain and app map objects", () => {
  const o = (kind: string, status?: string, missing?: boolean) => objectSignals({ kind, namespace: "a", name: "x", status, missing });
  // Service
  assert.equal(o("Service", "no pods match selector")?.kind, "Service");
  assert.equal(o("Service", "", true)?.missing, true);
  assert.equal(o("Service"), undefined);
  // Deployment and StatefulSet
  assert.equal(o("Deployment", "2/3 ready")?.status, "2/3 ready");
  assert.equal(o("Deployment", "3/3 ready"), undefined);
  assert.equal(o("Deployment", "0/0 ready"), undefined);
  assert.equal(o("StatefulSet", "0/2 ready")?.kind, "StatefulSet");
  // PVC
  assert.equal(o("PersistentVolumeClaim", "Pending")?.kind, "PersistentVolumeClaim");
  assert.equal(o("PersistentVolumeClaim", "Bound"), undefined);
  // Pod
  assert.equal(o("Pod", "Running"), undefined);
  assert.equal(o("Pod", "Completed"), undefined);
  assert.equal(o("Pod", "Running, not ready")?.kind, "Pod");
  assert.equal(o("Pod", "Pending (Unschedulable)")?.kind, "Pod");
  // Not covered: no signal in the data
  assert.equal(o("HorizontalPodAutoscaler", "x"), undefined);
  assert.equal(o("Ingress", "x"), undefined);
});
