// Helpers for the RBAC and Argo CD pages (KM-37), free of React and the DOM
// so node --test can check them.

import type { ArgoApp, ArgoResource, RBACGrant, RBACQueryForm, RBACRule } from "./api";

/** What each risky right lets an account do (names as the agent reports them). */
export const RISKY_TEXT: Record<string, string> = {
  "read secrets": "reads passwords, tokens and keys",
  "create pods": "runs any image as any ServiceAccount of the namespace (also through Deployments, Jobs …)",
  "exec into pods": "opens a shell in running containers",
  "change RBAC": "can give itself or others more rights",
  "bind or escalate": "can grant roles stronger than its own",
  impersonate: "acts as another user or account",
  "nodes/proxy": "reaches the kubelet API of the nodes",
  "create tokens": "gets tokens for other ServiceAccounts",
  wildcard: "a * rule: everything in its scope, including rights added later",
  "change workloads": "changes the image or command of running pods and workloads",
  "change admission control": "can switch off or rewrite the checks the API server runs on every change",
  "create persistentvolumes": "can mount any host path through a PersistentVolume",
  "change nodes": "can change node labels, taints and scheduling",
  "approve certificates": "can sign client certificates, including for admin groups",
};

/** A grant every account has, through a group that contains everyone. */
export function everyoneGrant(g: RBACGrant): boolean {
  return ["group system:authenticated", "group system:unauthenticated", "group system:serviceaccounts"].includes(g.via);
}

/**
 * Known grants to everyone that are not admin-equivalent, by binding, with
 * the reason (as portal/access.PlatformGrants, review KM-37 N-1).
 */
export const PLATFORM_GRANTS: Record<string, string> = {
  "ClusterRoleBinding//calico-tiered-policy-passthrough":
    "Calico's tier passthrough: * on Calico's own policy resources; Calico's API server still checks tiers",
};

/**
 * An account's risky rights: its own; those it has because every account
 * has them (loud: the most dangerous kind); and those from known platform
 * grants (quiet). As portal/access.
 */
export function splitRisky(grants: RBACGrant[]): { own: string[]; everyone: string[]; platform: string[] } {
  const own = new Set<string>();
  const all = new Set<string>();
  const plat = new Set<string>();
  for (const g of grants)
    for (const r of g.risky ?? []) (!everyoneGrant(g) ? own : PLATFORM_GRANTS[g.binding] ? plat : all).add(r);
  return {
    own: [...own].sort(),
    everyone: [...all].filter((r) => !own.has(r)).sort(),
    platform: [...plat].filter((r) => !own.has(r) && !all.has(r)).sort(),
  };
}

/** Common questions for the who-can form. */
export const PRESETS: { label: string; form: RBACQueryForm }[] = [
  { label: "Read Secrets", form: { verb: "get", resource: "secrets" } },
  { label: "Exec into pods", form: { verb: "create", resource: "pods/exec" } },
  { label: "Create pods", form: { verb: "create", resource: "pods" } },
  { label: "Delete pods", form: { verb: "delete", resource: "pods" } },
  { label: "Scale Deployments", form: { verb: "update", resource: "deployments/scale", apiGroup: "apps" } },
  { label: "Grant roles", form: { verb: "create", resource: "rolebindings", apiGroup: "rbac.authorization.k8s.io" } },
  { label: "Change NetworkPolicies", form: { verb: "update", resource: "networkpolicies", apiGroup: "networking.k8s.io" } },
];

/** "monitoring/grafana" → the ServiceAccount; null if not "ns/name". */
export function parseAccount(s: string): { namespace: string; name: string } | null {
  const m = /^([a-z0-9]([-a-z0-9]*[a-z0-9])?)\/([a-z0-9]([-a-z0-9.]*[a-z0-9])?)$/.exec(s.trim());
  return m ? { namespace: m[1], name: m[3] } : null;
}

/** "RoleBinding/ns/name" → { kind, namespace, name }; ClusterRoleBinding//name has no namespace. */
export function splitRef(ref: string): { kind: string; namespace: string; name: string } {
  const [kind = "", namespace = "", ...rest] = ref.split("/");
  return { kind, namespace, name: rest.join("/") };
}

/** A readable reference: "ClusterRole cluster-admin", "Role monitoring/grafana". */
export function refText(ref: string): string {
  const r = splitRef(ref);
  return r.namespace ? `${r.kind} ${r.namespace}/${r.name}` : `${r.kind} ${r.name}`;
}

/** One rule as a line: "get, list · pods, services (core) · only: a, b". */
export function ruleText(r: RBACRule): string {
  if (r.nonResourceURLs?.length) return `${r.verbs.join(", ")} · URLs ${r.nonResourceURLs.join(", ")}`;
  const groups = (r.apiGroups ?? []).map((g) => (g === "" ? "core" : g)).join(", ");
  let s = `${r.verbs.join(", ")} · ${(r.resources ?? []).join(", ")}${groups ? ` (${groups})` : ""}`;
  if (r.resourceNames?.length) s += ` · only: ${r.resourceNames.join(", ")}`;
  return s;
}

/**
 * The kubectl command that asks the API server the same check, or null for
 * a chain or who-can (kubectl has no such command).
 */
export function canICommand(f: RBACQueryForm): string | null {
  if (!f.verb || !f.resource || !f.subjectKind || !f.subject) return null;
  const [res, sub] = f.resource.split("/");
  let target = f.apiGroup ? `${res}.${f.apiGroup}` : res;
  if (f.resourceName) target += `/${f.resourceName}`;
  const parts = ["kubectl auth can-i", f.verb, target];
  if (sub) parts.push(`--subresource=${sub}`);
  parts.push(f.namespace ? `-n ${f.namespace}` : "--all-namespaces");
  if (f.subjectKind === "ServiceAccount") {
    if (!f.subjectNamespace) return null;
    parts.push(`--as=system:serviceaccount:${f.subjectNamespace}:${f.subject}`);
  } else if (f.subjectKind === "User") {
    parts.push(`--as=${f.subject}`);
    for (const g of (f.groups ?? "").split(",").map((x) => x.trim()).filter(Boolean)) parts.push(`--as-group=${g}`);
  } else {
    parts.push(`--as=kubemap-check --as-group=${f.subject}`);
  }
  return parts.join(" ");
}

export type Level = "ok" | "warn" | "fail" | "info";

export function syncLevel(s: string): Level {
  return s === "Synced" ? "ok" : s === "OutOfSync" ? "warn" : "info";
}

export function healthLevel(h?: string): Level {
  switch (h) {
    case "Healthy":
      return "ok";
    case "Degraded":
    case "Missing":
      return "fail";
    case "Progressing":
    case "Suspended":
      return "warn";
  }
  return "info";
}

/** One line on what needs attention in an app, or null when all is well. */
export function argoAttention(a: ArgoApp): string | null {
  const out = a.resources.filter((r) => r.status === "OutOfSync");
  const bad = a.resources.filter((r) => r.health && r.health !== "Healthy");
  const parts: string[] = [];
  if (a.operationPhase === "Failed" || a.operationPhase === "Error") parts.push(`last sync ${a.operationPhase.toLowerCase()}`);
  if (out.length) parts.push(`${out.length} resource${out.length > 1 ? "s" : ""} out of sync: ${out.map(resourceText).join(", ")}`);
  if (bad.length) parts.push(`${bad.length} not healthy: ${bad.map((r) => `${resourceText(r)} (${r.health})`).join(", ")}`);
  if (a.sync === "OutOfSync" && !out.length) parts.push("out of sync");
  return parts.length ? parts.join("; ") : null;
}

export function resourceText(r: ArgoResource): string {
  return `${r.kind}/${r.name}`;
}

/** The Kubemap page for a managed resource, if there is one. */
export function resourceHref(clusterId: string, app: ArgoApp, r: ArgoResource): string {
  const c = encodeURIComponent(clusterId);
  if (r.kind === "Pod" && r.namespace) return `#/clusters/${c}/namespaces/${encodeURIComponent(r.namespace)}/pods/${encodeURIComponent(r.name)}/debug`;
  if (r.kind === "NetworkPolicy") return `#/clusters/${c}/netpol`;
  return `#/clusters/${c}/apps/${encodeURIComponent(app.mapId)}`;
}

/** The commands that show the drift and the app. */
export function argoCommands(a: ArgoApp): string[] {
  return [
    `kubectl -n ${a.namespace} get application ${a.name} -o jsonpath='{.status.sync.status} {.status.health.status}{"\\n"}'`,
    `argocd app diff ${a.name}`,
    `argocd app get ${a.name} --show-operation`,
  ];
}

/** The last sync in one line (KM-41): who asked, which commit, how long. */
export function lastSyncText(a: Pick<ArgoApp, "operationPhase" | "operationBy" | "operationRevision" | "operationStarted" | "operationFinished" | "operationRunning">): string {
  if (a.operationRunning) return "running now" + (a.operationBy ? ` (asked by ${a.operationBy})` : "");
  if (!a.operationPhase) return "";
  const parts = [a.operationPhase];
  if (a.operationBy) parts.push(`by ${a.operationBy}`);
  if (a.operationRevision) parts.push(`revision ${a.operationRevision.slice(0, 7)}`);
  if (a.operationStarted && a.operationFinished) {
    const s = Math.round((Date.parse(a.operationFinished) - Date.parse(a.operationStarted)) / 1000);
    if (s >= 0) parts.push(`took ${s} s`);
  }
  return parts.join(" · ");
}
