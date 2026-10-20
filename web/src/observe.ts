// Links into Grafana and the "debug in this order" text. Pure, so it is tested.

const WORKLOAD_DASHBOARD = "a164a7f0339f99e89cea5cb47e9be617"; // kube-prometheus-stack: Compute Resources / Workload

/** The base URL without a trailing slash, or null unless it is http(s). */
export function safeBase(base: string): string | null {
  try {
    const u = new URL(base.trim());
    return u.protocol === "http:" || u.protocol === "https:" ? base.trim().replace(/\/+$/, "") : null;
  } catch {
    return null;
  }
}

export function grafanaWorkloadUrl(base: string, namespace: string, name: string, kind: string): string {
  const q = new URLSearchParams({ "var-namespace": namespace, "var-workload": name, "var-type": kind.toLowerCase() });
  return `${base.replace(/\/+$/, "")}/d/${WORKLOAD_DASHBOARD}/?${q}`;
}

export function grafanaTracesUrl(base: string, service: string): string {
  const panes = {
    a: {
      datasource: "tempo",
      queries: [{ refId: "A", datasource: { type: "tempo", uid: "tempo" }, queryType: "traceql", query: `{resource.service.name="${service.replace(/["\\]/g, "")}"}` }],
      range: { from: "now-1h", to: "now" },
    },
  };
  return `${base.replace(/\/+$/, "")}/explore?schemaVersion=1&orgId=1&panes=${encodeURIComponent(JSON.stringify(panes))}`;
}

export function isWorkloadKind(kind: string): boolean {
  return ["Deployment", "StatefulSet", "DaemonSet"].includes(kind.split(".")[0]);
}

/** Last value of a series, or undefined. */
export function lastValue(values: (number | null)[] | undefined): number | undefined {
  if (!values) return undefined;
  for (let i = values.length - 1; i >= 0; i--) if (values[i] != null) return values[i]!;
  return undefined;
}

export interface DebugStep {
  title: string;
  hint: string;
  level: "ok" | "warn" | "bad" | "unknown";
}

export interface DebugInput {
  podsReady: number;
  podsTotal: number;
  restartsDelta?: number; // restarts during the selected time range
  errorRate?: number; // failed requests per second
  requestRate?: number;
  failedTraces: number;
  tracesKnown: boolean;
}

/** The order to look in: pods, then restarts, then errors in requests, then the failing traces. */
export function debugSteps(i: DebugInput): DebugStep[] {
  const steps: DebugStep[] = [];
  steps.push(
    i.podsTotal === 0
      ? { title: "1. Are the pods running?", hint: "No pods found for this workload. Check the Deployment's events and its replicas.", level: "bad" }
      : i.podsReady < i.podsTotal
        ? { title: "1. Are the pods running?", hint: `${i.podsReady} of ${i.podsTotal} pods are ready. Open a not-ready pod's debug page.`, level: "bad" }
        : { title: "1. Are the pods running?", hint: `All ${i.podsTotal} pods are ready.`, level: "ok" },
  );
  steps.push(
    i.restartsDelta === undefined
      ? { title: "2. Did a container restart?", hint: "No restart data from Prometheus.", level: "unknown" }
      : i.restartsDelta > 0
        ? { title: "2. Did a container restart?", hint: `${i.restartsDelta} restarts in this time range. Read the previous run's logs (OOMKilled? crash?).`, level: "bad" }
        : { title: "2. Did a container restart?", hint: "No restarts in this time range.", level: "ok" },
  );
  steps.push(
    i.errorRate === undefined
      ? { title: "3. Are requests failing?", hint: "No request metrics from traces for this service.", level: "unknown" }
      : i.errorRate > 0
        ? {
            title: "3. Are requests failing?",
            hint: i.requestRate ? `About ${((i.errorRate / i.requestRate) * 100).toFixed(0)}% of requests fail. Open a failed trace to see which call broke.` : "Failed requests seen. Open a failed trace.",
            level: "bad",
          }
        : { title: "3. Are requests failing?", hint: "No failed requests in the traces.", level: "ok" },
  );
  steps.push(
    !i.tracesKnown
      ? { title: "4. Which call broke?", hint: "No trace data.", level: "unknown" }
      : i.failedTraces > 0
        ? { title: "4. Which call broke?", hint: `${i.failedTraces} failed traces below. Open one in Grafana and follow the red span to the service that returned the error.`, level: "bad" }
        : { title: "4. Which call broke?", hint: "No failed traces in this time range.", level: "ok" },
  );
  return steps;
}

export interface TraceSpan {
  id: string;
  parentId: string;
  service: string;
  scope: string;
  name: string;
  startUs: number;
  durationUs: number;
  error: boolean;
  detail: string;
}

export interface TimelineRow extends TraceSpan {
  depth: number;
  leftPct: number;
  widthPct: number;
  /** How far this span was moved to fix a clock difference between two services (0 = not moved). */
  skewUs: number;
}

/**
 * Spans in tree order (a parent, then its children by start time), with each bar's position in percent of the whole trace.
 * A browser and a server rarely agree on the time. When a span of another service does not fit inside the span
 * that called it, it is moved (with its same-service children) to the middle of the caller; its length stays real.
 */
export function timelineRows(spans: TraceSpan[]): TimelineRow[] {
  if (spans.length === 0) return [];
  const ids = new Set(spans.map((s) => s.id));
  const kids = new Map<string, TraceSpan[]>();
  for (const s of spans) {
    const key = ids.has(s.parentId) ? s.parentId : "";
    kids.set(key, [...(kids.get(key) ?? []), s]);
  }
  type Adj = { startUs: number; durationUs: number; service: string; skewUs: number };
  const flat: { s: TraceSpan; depth: number; startUs: number; skewUs: number }[] = [];
  const seen = new Set<string>();
  const walk = (key: string, depth: number, parent: Adj | null) => {
    for (const s of (kids.get(key) ?? []).sort((a, b) => a.startUs - b.startUs)) {
      if (seen.has(s.id)) continue;
      seen.add(s.id);
      let skew = parent && parent.service === s.service ? parent.skewUs : 0;
      let start = s.startUs + skew;
      if (parent && parent.service !== s.service && (start < parent.startUs || start + s.durationUs > parent.startUs + parent.durationUs)) {
        skew = parent.startUs + Math.max(0, (parent.durationUs - s.durationUs) / 2) - s.startUs;
        start = s.startUs + skew;
      }
      flat.push({ s, depth, startUs: start, skewUs: skew });
      walk(s.id, depth + 1, { startUs: start, durationUs: s.durationUs, service: s.service, skewUs: skew });
    }
  };
  walk("", 0, null);
  const min = Math.min(...flat.map((f) => f.startUs));
  const total = Math.max(1, ...flat.map((f) => f.startUs - min + f.s.durationUs));
  return flat.map((f) => {
    const startUs = f.startUs - min;
    return { ...f.s, startUs, depth: f.depth, skewUs: f.skewUs, leftPct: (startUs / total) * 100, widthPct: Math.max(0.5, (f.s.durationUs / total) * 100) };
  });
}

export function formatDuration(us: number): string {
  return us >= 1000 ? `${(us / 1000).toFixed(us >= 10000 ? 0 : 1)} ms` : `${us} µs`;
}

export type RangeChoice = "1h" | "6h" | "24h" | "custom";

const HOUR = 3600000;
/** Longest range the portal accepts: Prometheus keeps 7 days (Tempo only 24 hours). */
export const MAX_RANGE_MS = 7 * 24 * HOUR;

/** The window for a quick choice, counted back from nowMs. "1h" means the default (no query values). */
export function presetRange(choice: "1h" | "6h" | "24h", nowMs: number): { from: number; to: number } | undefined {
  if (choice === "1h") return undefined;
  return { from: nowMs - (choice === "6h" ? 6 : 24) * HOUR, to: nowMs };
}

/**
 * Turns the two datetime-local values ("2026-10-20T15:00", the browser's own
 * time zone) into a window, or says in plain words what is wrong.
 */
export function parseCustomRange(start: string, end: string, nowMs: number): { range: { from: number; to: number } } | { error: string } {
  if (!start || !end) return { error: "Enter both a start time and an end time." };
  const from = new Date(start).getTime();
  const to = new Date(end).getTime();
  if (Number.isNaN(from) || Number.isNaN(to)) return { error: "The start or end time is not a valid date and time." };
  if (to <= from) return { error: "The end time must be after the start time." };
  if (to - from > MAX_RANGE_MS) return { error: "The range can be at most 7 days." };
  if (from > nowMs) return { error: "The start time is in the future." };
  return { range: { from, to: Math.min(to, nowMs) } };
}

/** Value for a datetime-local input, in the browser's time zone. */
export function toLocalInput(ms: number): string {
  const d = new Date(ms - new Date(ms).getTimezoneOffset() * 60000);
  return d.toISOString().slice(0, 16);
}

/** SLO targets the panel offers, in percent. */
export const SLO_TARGETS = [99, 99.5, 99.9, 99.95];
export const DEFAULT_SLO = 99.9;

const sum = (v: (number | null)[] | undefined) => (v ?? []).reduce<number>((a, x) => a + (x ?? 0), 0);

/** Failed share of all requests over a range, from two rate series that share one time axis. Null without traffic. */
export function errorShare(errors: (number | null)[] | undefined, requests: (number | null)[] | undefined): number | null {
  const total = sum(requests);
  return total > 0 ? sum(errors) / total : null;
}

export type Level = "ok" | "warn" | "bad" | "none";

/** 1x spends the budget exactly as fast as the SLO allows; 14.4x empties a 7-day budget in about 12 hours. */
export function burnLevel(b: number | null | undefined): Level {
  if (b == null) return "none";
  return b >= 14.4 ? "bad" : b >= 1 ? "warn" : "ok";
}

export function budgetLevel(remaining: number | null | undefined): Level {
  if (remaining == null) return "none";
  return remaining < 0 ? "bad" : remaining < 0.5 ? "warn" : "ok";
}

/** "99.95%" style text; more digits near 100 so 99.9 and 99.99 differ. */
export function percentText(share: number | null): string {
  if (share == null) return "no traffic";
  const p = share * 100;
  return `${p >= 99 ? p.toFixed(3) : p.toFixed(1)}%`;
}

export function hoursText(h: number): string {
  return h < 1 ? `${Math.round(h * 60)} min` : h < 48 ? `${h.toFixed(1)} h` : `${(h / 24).toFixed(1)} days`;
}
