// Pure helpers for the guided debugging page (KM-76 UI), free of React and
// the DOM so node --test can check them.
import type { DebugStep, MapApp, MapPod } from "./api";

export type Level = "ok" | "warn" | "fail";

/** A map pod: ready is fine; starting or not ready yet is a warning; anything else (CrashLoopBackOff, Error, ...) fails. */
export function mapPodLevel(p: MapPod): Level {
  if (p.ready || p.status === "Succeeded" || p.status === "Completed") return "ok";
  if (/^(Running|Pending|ContainerCreating|PodInitializing|Terminating)$|^Running, not ready$/.test(p.status)) return "warn";
  return "fail";
}

export interface AppPod {
  namespace: string;
  name: string;
  component: string; // the workload or data store it belongs to
  status: string;
  ready: boolean;
  restarts: number;
  level: Level;
}

const order: Record<Level, number> = { fail: 0, warn: 1, ok: 2 };

/**
 * An app's pods, worst first: failing, then not ready, then fine; more
 * restarts first inside each. The first one is the pod to debug.
 */
export function appPods(app: MapApp): AppPod[] {
  const out: AppPod[] = [];
  const seen = new Set<string>();
  for (const c of app.components) {
    for (const p of c.pods ?? []) {
      const key = c.namespace + "/" + p.name;
      if (seen.has(key)) continue;
      seen.add(key);
      out.push({
        namespace: c.namespace, name: p.name, component: c.name, status: p.status,
        ready: p.ready, restarts: p.restarts, level: mapPodLevel(p),
      });
    }
  }
  return out.sort((a, b) => order[a.level] - order[b.level] || b.restarts - a.restarts || a.name.localeCompare(b.name));
}

/** Steps to show open: the cause, and every problem. */
export function openByDefault(step: DebugStep, causeStep?: string): boolean {
  return step.key === causeStep || step.verdict === "problem";
}

/** "3 problems, 4 ok, 1 not checked". */
export function tally(steps: DebugStep[]): string {
  const n = { problem: 0, ok: 0, unknown: 0 };
  for (const s of steps) n[s.verdict]++;
  const parts: string[] = [];
  if (n.problem) parts.push(`${n.problem} ${n.problem === 1 ? "problem" : "problems"}`);
  if (n.ok) parts.push(`${n.ok} ok`);
  if (n.unknown) parts.push(`${n.unknown} not checked`);
  return parts.join(", ");
}
