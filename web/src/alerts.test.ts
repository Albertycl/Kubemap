// Run with: npm test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { alertText, criticalCount, loadClosed, saveClosed, visibleAlerts } from "./alerts.ts";
import type { Alert } from "./api";

const alert = (over: Partial<Alert>): Alert => ({
  clusterId: "c-1", clusterName: "k8server", since: "2026-09-28T07:32:00Z", draftId: "d", key: "k1",
  findings: [{ rule: "node.disk.eviction", object: "Node/k8server", title: "DiskPressure" }], ...over,
});

test("banner text", () => {
  assert.equal(alertText(alert({})), "k8server: DiskPressure (Node/k8server)");
  assert.equal(
    alertText(alert({ findings: [{ rule: "pod.crashloop", object: "Pod/shop/web", title: "CrashLoopBackOff: web (Pod/shop/web)" }, { rule: "x", object: "y", title: "z" }] })),
    "k8server: CrashLoopBackOff: web (Pod/shop/web), and 1 more",
  );
});

test("closed alerts come back when the findings change", () => {
  const a = alert({ key: "k1" });
  assert.deepEqual(visibleAlerts([a], new Set(["k1"])), []);
  assert.equal(visibleAlerts([alert({ key: "k2" })], new Set(["k1"])).length, 1);
});

test("counts per cluster", () => {
  assert.equal(criticalCount([alert({})], "c-1"), 1);
  assert.equal(criticalCount([alert({})], "c-2"), 0);
  assert.equal(criticalCount(undefined, "c-1"), 0);
});

test("closed keys in storage", () => {
  const mem = new Map<string, string>();
  const st = { getItem: (k: string) => mem.get(k) ?? null, setItem: (k: string, v: string) => void mem.set(k, v) };
  saveClosed(st, new Set(["k1", "gone"]), [alert({ key: "k1" })]);
  assert.deepEqual([...loadClosed(st)], ["k1"]);
  assert.deepEqual([...loadClosed({ getItem: () => "not json" })], []);
  assert.deepEqual([...loadClosed(undefined)], []);
  saveClosed({ setItem: () => { throw new Error("full"); } }, new Set(["k1"]), [alert({})]);
});
