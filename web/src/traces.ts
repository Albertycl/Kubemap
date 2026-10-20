// App map links confirmed by traces (KM-54): the summary line.
import type { AppMapTraces } from "./api.ts";

/** One line for the app map page, or "" when there is no Tempo. */
export function traceSummary(t: AppMapTraces | undefined): { text: string; level: "ok" | "warn" | "muted" } | null {
  if (!t) return null;
  if (t.error) return { text: `Traces: could not read ${t.source} (${t.error}). Links are shown from configuration only.`, level: "warn" };
  const parts = [`Traces (${t.source}, last ${t.window.replace(/0m0s$/, "").replace(/0s$/, "")}):`];
  parts.push(`${t.confirmed} link${t.confirmed === 1 ? "" : "s"} confirmed by real calls`);
  const app = t.spans - (t.probes ?? 0);
  parts.push(`from ${app} span${app === 1 ? "" : "s"}`);
  if (t.probes) parts.push(`(${t.probes} kubelet probe span${t.probes === 1 ? "" : "s"} ignored)`);
  let text = parts.join(" ") + ".";
  // Tempo's query keeps only server spans with a user agent: say so, or an
  // unconfirmed link reads as "no traffic" (review km-54 N-3).
  if (t.confirmed === 0) text += " Only spans with a user agent are used, so a link without one is not proof of no traffic.";
  if (t.unmatched?.length) text += ` ${t.unmatched.length} call${t.unmatched.length === 1 ? "" : "s"} in the traces have no link on the map.`;
  return { text, level: t.unmatched?.length ? "warn" : t.confirmed > 0 ? "ok" : "muted" };
}
