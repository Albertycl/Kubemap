// Run with: npm test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { chainHref, itemLevel, parseId, SECTION_PLACE, SECTION_TEXT } from "./chain.ts";

test("chain links encode the object ID as one route segment", () => {
  assert.equal(chainHref("c-1"), "#/clusters/c-1/chain");
  assert.equal(chainHref("c-1", "Deployment/shop/web"), "#/clusters/c-1/chain/Deployment%2Fshop%2Fweb");
  assert.equal(chainHref("c-1", "Node//k8server"), "#/clusters/c-1/chain/Node%2F%2Fk8server");
});

test("IDs split into kind, namespace and name", () => {
  assert.deepEqual(parseId("Cluster.postgresql.cnpg.io/sre-lab-app/url-shortener-db"), {
    kind: "Cluster.postgresql.cnpg.io",
    shortKind: "Cluster",
    namespace: "sre-lab-app",
    name: "url-shortener-db",
  });
  assert.deepEqual(parseId("Node//k8server"), { kind: "Node", shortKind: "Node", namespace: "", name: "k8server" });
});

test("item levels follow the status words", () => {
  assert.equal(itemLevel({ status: "Running" }), "ok");
  assert.equal(itemLevel({ status: "2/2 ready" }), "ok");
  assert.equal(itemLevel({ status: "1/2 ready" }), "warn");
  assert.equal(itemLevel({ status: "0/1 ready" }), "fail");
  assert.equal(itemLevel({ status: "0/0 ready" }), "warn");
  assert.equal(itemLevel({ status: "CrashLoopBackOff" }), "fail");
  assert.equal(itemLevel({ status: "no pods match selector" }), "fail");
  assert.equal(itemLevel({ status: "Pending" }), "warn");
  assert.equal(itemLevel({ status: "Bound" }), "ok");
  assert.equal(itemLevel({}), "none");
  assert.equal(itemLevel({ status: "Running", missing: true }), "fail");
});

test("every section the agent sends has a title and a place", () => {
  const keys = ["owners", "owned", "pods", "services", "routes", "config", "secrets", "volumes", "serviceaccounts",
    "policies", "autoscalers", "nodes", "calls", "calledBy"];
  for (const k of keys) {
    assert.ok(SECTION_TEXT[k], k);
    assert.ok(SECTION_PLACE[k], k);
  }
  assert.equal(Object.keys(SECTION_TEXT).length, keys.length);
});
