// Grouping for the problem finder (KM-61/KM-62 UI), free of React and the
// DOM so node --test can check it.
import type { HealthFinding, Severity } from "./api";

export const SEVERITIES: Severity[] = ["critical", "warning", "info"];

export function rank(s: string): number {
  const i = SEVERITIES.indexOf(s as Severity);
  return i < 0 ? SEVERITIES.length : i;
}

export interface Group {
  /** "server:<node>", "cluster" or "ns:<namespace>". */
  key: string;
  kind: "server" | "cluster" | "namespace";
  label: string;
  findings: HealthFinding[];
  worst: number;
}

/** Where a finding belongs: its server (node), the whole cluster, or a namespace. */
export function groupOf(f: HealthFinding): { key: string; kind: Group["kind"]; label: string } {
  const [kind, a] = f.object.split("/");
  if (kind === "Node" && a) return { key: "server:" + a, kind: "server", label: a };
  if ((kind === "Pod" || kind === "Namespace") && a) return { key: "ns:" + a, kind: "namespace", label: a };
  return { key: "cluster", kind: "cluster", label: "Whole cluster" };
}

const kindOrder: Record<Group["kind"], number> = { server: 0, cluster: 1, namespace: 2 };

/**
 * Groups findings by server, cluster and namespace. Servers come first
 * (a sick node affects everything on it), then cluster-wide findings, then
 * namespaces; inside each kind the worst group first, then by name. Inside
 * a group the findings keep their order (the agent sends them worst first).
 */
export function groupFindings(findings: HealthFinding[]): Group[] {
  const groups = new Map<string, Group>();
  for (const f of findings) {
    const g = groupOf(f);
    let e = groups.get(g.key);
    if (!e) {
      e = { ...g, findings: [], worst: SEVERITIES.length };
      groups.set(g.key, e);
    }
    e.findings.push(f);
    e.worst = Math.min(e.worst, rank(f.severity));
  }
  for (const g of groups.values()) g.findings.sort((x, y) => rank(x.severity) - rank(y.severity));
  return [...groups.values()].sort(
    (x, y) => kindOrder[x.kind] - kindOrder[y.kind] || x.worst - y.worst || x.label.localeCompare(y.label),
  );
}

/** How many findings of each severity. */
export function counts(findings: HealthFinding[]): Record<Severity, number> {
  const c: Record<Severity, number> = { critical: 0, warning: 0, info: 0 };
  for (const f of findings) if (f.severity in c) c[f.severity]++;
  return c;
}

/**
 * A verify command that is not kubectl runs on the node (journalctl, du,
 * cat /proc/...): the page says so and only offers to copy it (review km-61
 * N-5, km-62 N-4).
 */
export function runsOnNode(cmd: string): boolean {
  return !/^\s*(kubectl|argocd|curl|helm)\b/.test(cmd);
}
