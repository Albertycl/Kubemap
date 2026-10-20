// YAML view and edit (KM-39): which object, and the diff summary.
import type { YamlDiffLine } from "./api.ts";

export interface YamlTarget {
  group: string;
  version: string;
  resource: string;
  namespace: string;
  name: string;
}

/** Common kinds for the picker; any other resource can be typed. */
export const YAML_KINDS: { label: string; group: string; version: string; resource: string; namespaced: boolean }[] = [
  { label: "Deployment", group: "apps", version: "v1", resource: "deployments", namespaced: true },
  { label: "StatefulSet", group: "apps", version: "v1", resource: "statefulsets", namespaced: true },
  { label: "DaemonSet", group: "apps", version: "v1", resource: "daemonsets", namespaced: true },
  { label: "Pod", group: "", version: "v1", resource: "pods", namespaced: true },
  { label: "Service", group: "", version: "v1", resource: "services", namespaced: true },
  { label: "ConfigMap", group: "", version: "v1", resource: "configmaps", namespaced: true },
  { label: "Ingress", group: "networking.k8s.io", version: "v1", resource: "ingresses", namespaced: true },
  { label: "NetworkPolicy", group: "networking.k8s.io", version: "v1", resource: "networkpolicies", namespaced: true },
  { label: "CronJob", group: "batch", version: "v1", resource: "cronjobs", namespaced: true },
  { label: "HorizontalPodAutoscaler", group: "autoscaling", version: "v2", resource: "horizontalpodautoscalers", namespaced: true },
  { label: "Node", group: "", version: "v1", resource: "nodes", namespaced: false },
];

const FIELDS: (keyof YamlTarget)[] = ["group", "version", "resource", "namespace", "name"];

/** The page's route: #/clusters/<id>/yaml?group=…&version=…&resource=…&namespace=…&name=… */
export function yamlHref(clusterId: string, t: YamlTarget): string {
  const q = new URLSearchParams();
  for (const f of FIELDS) if (t[f]) q.set(f, t[f]);
  return `#/clusters/${encodeURIComponent(clusterId)}/yaml?${q.toString()}`;
}

/** Reads the target from the route's query; missing fields are empty. */
export function parseYamlQuery(query: string | undefined): YamlTarget {
  const q = new URLSearchParams(query ?? "");
  const t = { group: "", version: "", resource: "", namespace: "", name: "" };
  for (const f of FIELDS) t[f] = q.get(f) ?? "";
  return t;
}

/** Whether the target names one object (namespace may be empty for cluster-scoped kinds). */
export function complete(t: YamlTarget): boolean {
  return t.version !== "" && t.resource !== "" && t.name !== "";
}

/** "+2 −1": the added and removed lines of a diff. */
export function diffStats(lines: YamlDiffLine[]): { added: number; removed: number } {
  let added = 0;
  let removed = 0;
  for (const l of lines) {
    if (l.op === "+") added++;
    else if (l.op === "-") removed++;
  }
  return { added, removed };
}
