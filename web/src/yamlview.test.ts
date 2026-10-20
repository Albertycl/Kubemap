// Run with: npm test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { complete, diffStats, parseYamlQuery, yamlHref } from "./yamlview.ts";

test("yaml route round trip", () => {
  const t = { group: "apps", version: "v1", resource: "deployments", namespace: "sre-lab-app", name: "flask-vue-backend" };
  const href = yamlHref("c-1", t);
  assert.equal(href, "#/clusters/c-1/yaml?group=apps&version=v1&resource=deployments&namespace=sre-lab-app&name=flask-vue-backend");
  assert.deepEqual(parseYamlQuery(href.split("?")[1]), t);
  // Core group and cluster-scoped: empty fields are left out and read back empty.
  const node = { group: "", version: "v1", resource: "nodes", namespace: "", name: "k8server" };
  assert.equal(yamlHref("c-1", node), "#/clusters/c-1/yaml?version=v1&resource=nodes&name=k8server");
  assert.deepEqual(parseYamlQuery("version=v1&resource=nodes&name=k8server"), node);
  assert.ok(complete(node));
  assert.ok(!complete(parseYamlQuery(undefined)));
});

test("diff stats", () => {
  assert.deepEqual(
    diffStats([
      { op: " ", text: "a" },
      { op: "-", text: "b" },
      { op: "+", text: "c" },
      { op: "+", text: "d" },
    ]),
    { added: 2, removed: 1 },
  );
});
