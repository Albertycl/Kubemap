// Run with: npm test.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  lastSyncText,
  argoAttention,
  canICommand,
  healthLevel,
  parseAccount,
  refText,
  resourceHref,
  ruleText,
  splitRef,
  splitRisky,
  syncLevel,
} from "./access.ts";
import type { ArgoApp } from "./api";

test("accounts and references", () => {
  assert.deepEqual(parseAccount("monitoring/grafana"), { namespace: "monitoring", name: "grafana" });
  assert.equal(parseAccount("grafana"), null);
  assert.equal(parseAccount("a/b/c"), null);
  assert.equal(parseAccount("Mon/x"), null);
  assert.deepEqual(splitRef("RoleBinding/monitoring/grafana"), { kind: "RoleBinding", namespace: "monitoring", name: "grafana" });
  assert.equal(refText("ClusterRole//cluster-admin"), "ClusterRole cluster-admin");
  assert.equal(refText("Role/monitoring/cm"), "Role monitoring/cm");
});

test("rules as text", () => {
  assert.equal(ruleText({ verbs: ["get", "list"], apiGroups: [""], resources: ["secrets"], resourceNames: ["a"] }), "get, list · secrets (core) · only: a");
  assert.equal(ruleText({ verbs: ["get"], nonResourceURLs: ["/healthz"] }), "get · URLs /healthz");
});

test("kubectl can-i for a check", () => {
  assert.equal(
    canICommand({ subjectKind: "ServiceAccount", subject: "grafana", subjectNamespace: "monitoring", verb: "get", resource: "secrets", namespace: "sre-lab-app" }),
    "kubectl auth can-i get secrets -n sre-lab-app --as=system:serviceaccount:monitoring:grafana",
  );
  assert.equal(
    canICommand({ subjectKind: "User", subject: "alice", groups: "dev, ops", verb: "create", resource: "pods/exec", namespace: "shop" }),
    "kubectl auth can-i create pods --subresource=exec -n shop --as=alice --as-group=dev --as-group=ops",
  );
  assert.equal(
    canICommand({ subjectKind: "Group", subject: "system:authenticated", verb: "update", resource: "deployments/scale", apiGroup: "apps", resourceName: "web" }),
    "kubectl auth can-i update deployments.apps/web --subresource=scale --all-namespaces --as=kubemap-check --as-group=system:authenticated",
  );
  assert.equal(canICommand({ verb: "get", resource: "secrets" }), null);
  assert.equal(canICommand({ subjectKind: "ServiceAccount", subject: "x", verb: "get", resource: "pods" }), null);
});

const app = (over: Partial<ArgoApp>): ArgoApp => ({
  name: "a", namespace: "argocd", mapId: "argocd/a", project: "default", sources: [], autoSync: false, prune: false, selfHeal: false,
  sync: "Synced", health: "Healthy", resources: [], ...over,
});

test("argo levels and attention", () => {
  assert.equal(syncLevel("OutOfSync"), "warn");
  assert.equal(healthLevel("Degraded"), "fail");
  assert.equal(healthLevel(undefined), "info");
  assert.equal(argoAttention(app({})), null);
  assert.equal(
    argoAttention(app({ sync: "OutOfSync", resources: [{ kind: "Deployment", name: "web", status: "OutOfSync", health: "Healthy" }, { kind: "Pod", name: "p", health: "Degraded" }] })),
    "1 resource out of sync: Deployment/web; 1 not healthy: Pod/p (Degraded)",
  );
  assert.equal(argoAttention(app({ operationPhase: "Failed" })), "last sync failed");
});

test("resource links", () => {
  const a = app({ mapId: "argocd/url-shortener" });
  assert.equal(resourceHref("c-1", a, { kind: "Deployment", name: "u", namespace: "x" }), "#/clusters/c-1/apps/argocd%2Furl-shortener");
  assert.equal(resourceHref("c-1", a, { kind: "Pod", name: "p", namespace: "x" }), "#/clusters/c-1/namespaces/x/pods/p/debug");
});

test("own, everyone and platform risky rights", () => {
  const g = (via: string, risky: string[], binding = "b") => ({ binding, role: "r", scope: "cluster", via, risky });
  const calico = "ClusterRoleBinding//calico-tiered-policy-passthrough";
  assert.deepEqual(splitRisky([g("group system:authenticated", ["wildcard"], calico), g("direct", ["read secrets"])]), {
    own: ["read secrets"],
    everyone: [],
    platform: ["wildcard"],
  });
  // Any other grant to everyone is loud, even cluster-admin to system:authenticated.
  assert.deepEqual(splitRisky([g("group system:authenticated", ["wildcard", "read secrets"], "ClusterRoleBinding//oops")]), {
    own: [],
    everyone: ["read secrets", "wildcard"],
    platform: [],
  });
  assert.deepEqual(splitRisky([g("group system:serviceaccounts:monitoring", ["wildcard"]), g("group system:authenticated", ["wildcard"], calico)]), {
    own: ["wildcard"],
    everyone: [],
    platform: [],
  });
});

test("last sync text", () => {
  assert.equal(
    lastSyncText({
      operationRunning: false, operationPhase: "Succeeded", operationBy: "admin",
      operationRevision: "89ab3fbc67f8aa652fed162bec69bbfe36cf1da8",
      operationStarted: "2026-09-06T08:42:56Z", operationFinished: "2026-09-06T08:42:59Z",
    }),
    "Succeeded · by admin · revision 89ab3fb · took 3 s",
  );
  assert.equal(lastSyncText({ operationRunning: true, operationBy: "kubemap:admin" }), "running now (asked by kubemap:admin)");
  assert.equal(lastSyncText({ operationRunning: false }), "");
});
