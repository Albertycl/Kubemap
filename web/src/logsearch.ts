// Pure helpers for the log search page (KM-57), kept free of React and the
// DOM so node --test can check them.
import type { LogLevel, LogSearchQuery } from "./api";

const LEVELS: LogLevel[] = ["error", "warn", "info", "debug", "unknown"];

/** Windows the page offers, as durations back from now. */
export const WINDOWS = ["15m", "1h", "6h", "24h", "168h"];

export const WINDOW_TEXT: Record<string, string> = {
  "15m": "last 15 min",
  "1h": "last hour",
  "6h": "last 6 hours",
  "24h": "last 24 hours",
  "168h": "last 7 days",
};

/** A piece of text, marked when it matches the search. */
export interface Piece {
  text: string;
  match: boolean;
}

/** Splits text into pieces around every case-insensitive match of q. */
export function highlight(text: string, q: string): Piece[] {
  if (!q) return [{ text, match: false }];
  const out: Piece[] = [];
  const hay = text.toLowerCase();
  const needle = q.toLowerCase();
  let at = 0;
  for (;;) {
    const i = hay.indexOf(needle, at);
    if (i < 0) break;
    if (i > at) out.push({ text: text.slice(at, i), match: false });
    out.push({ text: text.slice(i, i + needle.length), match: true });
    at = i + needle.length;
  }
  if (at < text.length) out.push({ text: text.slice(at), match: false });
  return out.length ? out : [{ text, match: false }];
}

/**
 * What a stack line is, for colour: the exception or its cause ("Caused
 * by: …", Python's final "ValueError: …", Go's "goroutine 1 [running]:"),
 * a frame ("at …", "File …", a Go function and its file), or other text.
 */
export function stackLineKind(line: string): "cause" | "frame" | "text" {
  const t = line.trim();
  if (/^Caused by: /.test(t) || /^goroutine \d+ \[/.test(t) || /^[A-Za-z_][\w.$]*(Error|Exception)\b.*:/.test(t)) return "cause";
  if (/^at\s/.test(t) || /^File "/.test(t) || /^\.\.\. \d+ more/.test(t) || /\.go:\d+/.test(t) || /^[\w./*()[\]-]+\(.*\)$/.test(t)) return "frame";
  return "text";
}

/** The viewer's local time of an entry: HH:MM:SS, with the date when it is not today. */
export function entryTime(iso: string, now = new Date()): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  const time = `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  const sameDay = d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate();
  return sameDay ? time : `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${time}`;
}

/**
 * The search as it is kept in the page's URL, after the "?" of
 * #/clusters/<id>/log-search/<app>?…, so a search can be shared and
 * survives a reload. Unknown or bad values fall back to the defaults.
 */
export function readSearch(app: string, query: string | undefined): LogSearchQuery {
  const p = new URLSearchParams(query ?? "");
  const levels = (p.get("levels") ?? "")
    .split(",")
    .filter((l): l is LogLevel => (LEVELS as string[]).includes(l));
  const since = p.get("since") ?? "1h";
  return {
    app,
    component: p.get("component") || undefined,
    q: p.get("q") || undefined,
    // No levels in the URL: errors and warnings. "levels=" (empty): every level.
    levels: p.has("levels") ? levels : ["error", "warn"],
    since: WINDOWS.includes(since) ? since : "1h",
    previous: p.get("previous") !== "0",
  };
}

/** The URL query for a search (the inverse of readSearch). */
export function writeSearch(s: LogSearchQuery): string {
  const p = new URLSearchParams();
  if (s.component) p.set("component", s.component);
  if (s.q) p.set("q", s.q);
  p.set("levels", (s.levels ?? []).join(","));
  if (s.since) p.set("since", s.since);
  if (!s.previous) p.set("previous", "0");
  return p.toString();
}
