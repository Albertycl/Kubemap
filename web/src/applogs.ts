// Pure helpers for app-level logs (KM-55): which pods an app has, one
// colour for each, and merging the lines that "follow" brings in. Free of
// React and the DOM so node --test can check them.
import type { LogEntry, MapApp } from "./api";

/** Colours in the palette (styles.css .slot-0 … .slot-11). */
export const PALETTE_SIZE = 12;

/** The order parts of an app are listed in: from the entry to the data. */
const TIER_ORDER = ["entry", "frontend", "backend", "database", "cache"];

export interface AppPod {
  key: string; // "namespace/pod"
  namespace: string;
  pod: string;
  component: string; // "Deployment web"
  tier: string;
}

/** The pods of an app, from the entry to the data, then by component and name. */
export function appPods(app: MapApp | undefined): AppPod[] {
  if (!app) return [];
  const out: AppPod[] = [];
  const comps = [...app.components].sort(
    (a, b) => TIER_ORDER.indexOf(a.tier) - TIER_ORDER.indexOf(b.tier) || a.id.localeCompare(b.id),
  );
  for (const c of comps) {
    for (const p of [...(c.pods ?? [])].sort((a, b) => a.name.localeCompare(b.name))) {
      out.push({ key: `${c.namespace}/${p.name}`, namespace: c.namespace, pod: p.name, component: `${c.kind.split(".")[0]} ${c.name}`, tier: c.tier });
    }
  }
  return out;
}

/**
 * One colour slot per pod, in the app's order, so pods never share a colour
 * while the app has at most PALETTE_SIZE of them. A pod the map does not
 * know (a previous run of a replaced pod) gets the next free slots, in the
 * order it is first seen.
 */
export function podColours(known: string[], seen: string[] = []): Map<string, number> {
  const out = new Map<string, number>();
  for (const k of [...known, ...seen]) {
    if (!out.has(k)) out.set(k, out.size % PALETTE_SIZE);
  }
  return out;
}

/**
 * Compares two RFC 3339 times. Plain text comparison is wrong: Go drops the
 * trailing zeros of the fraction, so "…:00Z" would sort after "…:00.1Z".
 * Milliseconds decide; the nanoseconds left over break ties.
 */
export function compareTime(a: string, b: string): number {
  const d = Date.parse(a) - Date.parse(b);
  if (d !== 0 && !Number.isNaN(d)) return d;
  return nanos(a) - nanos(b);
}

function nanos(t: string): number {
  const m = t.match(/\.(\d+)/);
  return m ? Number((m[1] + "000000000").slice(3, 9)) : 0;
}

/** The identity of a log line, to drop the ones a follow read twice. */
export function entryKey(e: LogEntry): string {
  return [e.time, e.namespace, e.pod, e.container, e.previous ? "p" : "", e.text].join("\u0000");
}

/**
 * Adds newly read lines to the ones shown: no line twice, oldest first, and
 * at most cap lines (the newest kept). Returns the lines and how many are new.
 */
export function mergeEntries(shown: LogEntry[], fresh: LogEntry[], cap: number): { entries: LogEntry[]; added: number } {
  const have = new Set(shown.map(entryKey));
  const add = fresh.filter((e) => !have.has(entryKey(e)));
  if (add.length === 0) return { entries: shown, added: 0 };
  const all = [...shown, ...add].sort((a, b) => compareTime(a.time, b.time));
  return { entries: all.length > cap ? all.slice(all.length - cap) : all, added: add.length };
}

/** The time a follow reads from: the newest line shown, or the end of the last window. */
export function followFrom(entries: LogEntry[], until: string): string {
  const last = entries.length ? entries[entries.length - 1].time : "";
  if (!until) return last;
  if (!last) return until;
  return compareTime(last, until) > 0 ? last : until;
}

/** Follow's pace: every FOLLOW_EVERY_MS, backing off to FOLLOW_MAX_MS while the agent is busy. */
export const FOLLOW_EVERY_MS = 10000;
export const FOLLOW_MAX_MS = 60000;

/**
 * The wait before the next follow read (review-km-55 N-1). A follow read
 * takes one of the agent's two search slots; when the agent says it is
 * busy (other searches or follows running), wait twice as long, up to a
 * minute, instead of stopping; after a good read, go back to the normal pace.
 */
export function nextFollowDelay(prev: number, busy: boolean): number {
  return busy ? Math.min(Math.max(prev, FOLLOW_EVERY_MS) * 2, FOLLOW_MAX_MS) : FOLLOW_EVERY_MS;
}
