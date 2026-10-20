// Run with: npm test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { counts, groupFindings, groupOf, runsOnNode } from "./problems.ts";
import type { HealthFinding } from "./api.ts";

const f = (object: string, severity: HealthFinding["severity"], rule = "r"): HealthFinding => ({
  rule, severity, object, title: rule, evidence: "", meaning: "", why: "", fix: "", verify: "",
});

test("findings go to their server, the cluster, or their namespace", () => {
  assert.deepEqual(groupOf(f("Node/k8server", "critical")), { key: "server:k8server", kind: "server", label: "k8server" });
  assert.equal(groupOf(f("Pod/shop/web-1", "warning")).key, "ns:shop");
  assert.equal(groupOf(f("Namespace/shop", "info")).key, "ns:shop");
  assert.equal(groupOf(f("Cluster", "warning")).key, "cluster");
  assert.equal(groupOf(f("Deployment", "warning")).key, "cluster");
});

test("servers first, then the cluster, then namespaces; worst first inside", () => {
  const g = groupFindings([
    f("Pod/b/x", "info", "b1"),
    f("Pod/a/x", "warning", "a1"),
    f("Cluster", "warning", "c1"),
    f("Node/n2", "warning", "n2"),
    f("Node/n1", "critical", "n1"),
    f("Namespace/a", "critical", "a2"),
  ]);
  assert.deepEqual(g.map((x) => x.key), ["server:n1", "server:n2", "cluster", "ns:a", "ns:b"]);
  assert.deepEqual(g[3].findings.map((x) => x.rule), ["a2", "a1"]);
});

test("counts per severity", () => {
  assert.deepEqual(counts([f("Cluster", "critical"), f("Cluster", "info"), f("Cluster", "info")]), { critical: 1, warning: 0, info: 2 });
});

test("verify commands that run on the node (review km-61 N-5)", () => {
  assert.equal(runsOnNode("journalctl -k --since -1h | grep -i oom"), true);
  assert.equal(runsOnNode("sudo du --inodes -x / | sort -n | tail"), true);
  assert.equal(runsOnNode("kubectl describe node k8server"), false);
  assert.equal(runsOnNode("  kubectl get pods -A"), false);
});
