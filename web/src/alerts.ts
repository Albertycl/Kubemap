// Helpers for the alert banner and badges (KM-31), free of React and the
// DOM so node --test can check them.

import type { Alert } from "./api";

/** The alerts the user has not closed. A closed alert comes back when its findings change (a new key). */
export function visibleAlerts(alerts: Alert[], closed: Set<string>): Alert[] {
  return alerts.filter((a) => !closed.has(a.key));
}

/** One line for the banner: "k8server: DiskPressure (Node/k8server), and 2 more". */
export function alertText(a: Alert): string {
  const [first, ...rest] = a.findings;
  if (!first) return a.clusterName;
  let s = `${a.clusterName}: ${first.title}`;
  if (first.object && !first.title.includes(first.object)) s += ` (${first.object})`;
  if (rest.length) s += `, and ${rest.length} more`;
  return s;
}

/** Critical findings on one cluster now. */
export function criticalCount(alerts: Alert[] | undefined, clusterId: string): number {
  return alerts?.find((a) => a.clusterId === clusterId)?.findings.length ?? 0;
}

/** Closed alert keys, kept in this browser; storage may be unavailable. */
const STORE = "kubemap:closed-alerts";

export function loadClosed(storage: Pick<Storage, "getItem"> | undefined): Set<string> {
  try {
    const v = JSON.parse(storage?.getItem(STORE) ?? "[]");
    return new Set(Array.isArray(v) ? v.filter((x) => typeof x === "string") : []);
  } catch {
    return new Set();
  }
}

/** Saves the closed keys that still matter (those of current alerts). */
export function saveClosed(storage: Pick<Storage, "setItem"> | undefined, closed: Set<string>, alerts: Alert[]): void {
  const live = new Set(alerts.map((a) => a.key));
  try {
    storage?.setItem(STORE, JSON.stringify([...closed].filter((k) => live.has(k))));
  } catch {
    // Private mode or full storage: the banner just comes back next time.
  }
}
