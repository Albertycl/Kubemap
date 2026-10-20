// Pure helpers for the chain view (KM-35), free of React and the DOM so
// node --test can check them.
import type { ChainItem } from "./api";

export type Level = "ok" | "warn" | "fail" | "none";

/** The section titles, in the agent's order: what each list is, seen from the object. */
export const SECTION_TEXT: Record<string, string> = {
  owners: "Owned by",
  owned: "Owns",
  pods: "Pods",
  services: "Services that select the pods",
  routes: "Routes to those Services",
  config: "ConfigMaps the pods use",
  secrets: "Secrets the pods refer to (names only)",
  volumes: "Volumes",
  serviceaccounts: "Runs as",
  policies: "NetworkPolicies that isolate the pods",
  autoscalers: "Autoscalers",
  nodes: "Runs on",
  calls: "Calls (from its configuration)",
  calledBy: "Called by (from their configuration)",
};

/** Where each section goes in the picture: above the object (what leads to it), below (what it runs on and uses), beside (who talks to it). */
export const SECTION_PLACE: Record<string, "up" | "down" | "side"> = {
  owners: "up",
  routes: "up",
  services: "up",
  calledBy: "up",
  owned: "down",
  pods: "down",
  config: "down",
  secrets: "down",
  volumes: "down",
  serviceaccounts: "down",
  nodes: "down",
  policies: "side",
  autoscalers: "side",
  calls: "side",
};

/** #/clusters/<id>/chain[/<object id>] */
export function chainHref(clusterId: string, objectId?: string): string {
  const base = `#/clusters/${encodeURIComponent(clusterId)}/chain`;
  return objectId ? `${base}/${encodeURIComponent(objectId)}` : base;
}

/** "Kind/namespace/name" → its parts; a custom resource's kind loses its group for display. */
export function parseId(id: string): { kind: string; shortKind: string; namespace: string; name: string } {
  const [kind = "", namespace = "", ...rest] = id.split("/");
  return { kind, shortKind: kind.split(".")[0], namespace, name: rest.join("/") };
}

/** A colour for an item's status: missing and failing states are red, not-ready amber, ready green. */
export function itemLevel(it: Pick<ChainItem, "status" | "missing">): Level {
  if (it.missing) return "fail";
  const s = it.status ?? "";
  if (s === "") return "none";
  if (/CrashLoopBackOff|Error|Failed|ImagePull|ErrImage|OOMKilled|Evicted|NotReady|Lost|no pods match/i.test(s)) return "fail";
  const m = s.match(/^(\d+)\/(\d+) ready$/);
  if (m) return m[1] === m[2] ? (m[2] === "0" ? "warn" : "ok") : m[1] === "0" ? "fail" : "warn";
  if (/^(Running|Ready|Bound|Succeeded|Completed|Active|Healthy)/.test(s)) return "ok";
  return "warn";
}
