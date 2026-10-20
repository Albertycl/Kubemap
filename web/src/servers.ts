// Ranking for the all-servers overview (KM-66 UI), free of React and the
// DOM so node --test can check it.
import type { ServerInfo } from "./api";

export type Level = "fail" | "warn" | "ok";

/**
 * A server's state and the reason, in words:
 * fail: node down, unreachable or NotReady (the KM-65 state), the kubelet is
 *   under pressure, or the node agent is gone (without a state);
 * warn: elevated pressure, stale numbers, no node agent, unknown pressure;
 * ok otherwise.
 */
export function serverLevel(s: ServerInfo): { level: Level; reason: string } {
  const p = s.pressure;
  // KM-65: node down, node agent down and unreachable come first, in words
  // that say which one it is.
  if (s.state && s.state.severity === "fail") return { level: "fail", reason: s.state.title };
  if (s.state && (s.state.code === "agent-down" || s.state.code === "reconnecting" || s.state.code === "removed")) {
    return { level: "warn", reason: s.state.title };
  }
  if (p && !p.ready) return { level: "fail", reason: "NotReady" };
  if (s.status !== "none" && !s.connected) return { level: "fail", reason: "node agent not reporting" };
  if (p?.verdict === "pressure") return { level: "fail", reason: p.summary };
  if (p?.verdict === "elevated") return { level: "warn", reason: p.summary };
  if (s.status === "none") return { level: "warn", reason: "no node agent on this node" };
  if (s.stale) return { level: "warn", reason: "numbers are not current" };
  if (p?.verdict === "unknown") return { level: "warn", reason: p.summary || "pressure unknown" };
  return { level: "ok", reason: p?.summary || "" };
}

const rank: Record<Level, number> = { fail: 0, warn: 1, ok: 2 };

/** Worst first, then by cluster and name. */
export function sortServers(list: ServerInfo[]): ServerInfo[] {
  return [...list].sort(
    (a, b) =>
      rank[serverLevel(a).level] - rank[serverLevel(b).level] ||
      a.clusterName.localeCompare(b.clusterName) ||
      a.name.localeCompare(b.name),
  );
}
