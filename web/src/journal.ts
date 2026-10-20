// Helpers for the incident journal pages (KM-89, the interview view KM-91),
// free of React and the DOM so node --test can check them.

import type {
  Decision,
  JournalKind,
  JournalRecord,
  JournalSection,
  SectionKind,
  Star,
} from "./api";

/** Section kinds that hold Markdown text. */
const TEXT_KINDS: SectionKind[] = ["intro", "summary", "detection", "rootcause", "actions", "result", "lessons", "text"];

export function isTextKind(k: SectionKind): boolean {
  return TEXT_KINDS.includes(k);
}

/** The sections a person can add, with their default headings (as portal/journal). */
export const SECTION_KINDS: { kind: SectionKind; label: string; heading: string }[] = [
  { kind: "summary", label: "Summary", heading: "Summary" },
  { kind: "timeline", label: "Timeline (time, event)", heading: "Timeline" },
  { kind: "rootcause", label: "Root cause", heading: "Root cause" },
  { kind: "detection", label: "Detection: how it was found", heading: "How it was found" },
  { kind: "actions", label: "Actions: what was done", heading: "Actions" },
  { kind: "decisions", label: "Decisions (why, trade-off)", heading: "Actions and decisions" },
  { kind: "result", label: "Result", heading: "Result" },
  { kind: "followups", label: "Follow-ups (owner, status)", heading: "Fixes and follow-ups" },
  { kind: "star", label: "Interview summary (STAR)", heading: "Interview summary (STAR)" },
  { kind: "lessons", label: "Lessons", heading: "Lessons" },
  { kind: "changes", label: "Change rows", heading: "Changes" },
  { kind: "text", label: "Other text", heading: "Notes" },
];

export const SEVERITIES = ["critical", "major", "minor"];

export const STATUS_TEXT: Record<string, string> = { todo: "To do", doing: "In progress", done: "Done" };

export function emptySection(kind: SectionKind, heading?: string): JournalSection {
  const h = heading ?? SECTION_KINDS.find((s) => s.kind === kind)?.heading ?? "";
  switch (kind) {
    case "timeline":
      return { kind, heading: h, timeline: [{ time: "", event: "" }] };
    case "decisions":
      return { kind, heading: h, decisions: [{ decision: "", why: "", tradeOff: "" }] };
    case "followups":
      return { kind, heading: h, followUps: [{ status: "todo", owner: "", text: "" }] };
    case "star":
      return { kind, heading: h, star: { situation: "", task: "", action: "", result: "" } };
    case "changes":
      return { kind, heading: h, changes: [{ time: "", change: "", why: "", checked: "", undo: "" }] };
    default:
      return { kind, heading: kind === "intro" ? "" : h, text: "" };
  }
}

/** A new record with the sections a good write-up has, ready to fill in. */
export function newRecord(kind: JournalKind, today: string): JournalRecord {
  if (kind === "changes") {
    return { kind, title: `Cluster changes on ${today}`, date: today, cluster: "", severity: "", sections: [emptySection("changes", "")] };
  }
  const kinds: SectionKind[] = ["summary", "timeline", "rootcause", "detection", "decisions", "followups", "star", "lessons"];
  return { kind, title: "", date: today, cluster: "", severity: "", sections: kinds.map((k) => emptySection(k)) };
}

/** Moves item i by delta (-1 up, +1 down); the same array if it cannot move. */
export function move<T>(list: T[], i: number, delta: number): T[] {
  const j = i + delta;
  if (i < 0 || i >= list.length || j < 0 || j >= list.length) return list;
  const out = list.slice();
  [out[i], out[j]] = [out[j], out[i]];
  return out;
}

/** Replaces item i. */
export function replaceAt<T>(list: T[], i: number, v: T): T[] {
  return list.map((x, k) => (k === i ? v : x));
}

/** Removes item i. */
export function removeAt<T>(list: T[], i: number): T[] {
  return list.filter((_, k) => k !== i);
}

/** What the page can say about a record before it is sent; null if fine. */
export function recordProblem(r: JournalRecord): string | null {
  if (!r.title.trim()) return "Give it a title.";
  if (r.date && !/^\d{4}-\d{2}-\d{2}$/.test(r.date)) return "The date must be YYYY-MM-DD.";
  for (const s of r.sections) {
    if (s.kind === "star" && s.star && Object.values(s.star).some((v) => v.includes("\n")))
      return "Each STAR part must be one paragraph.";
    if (s.kind === "followups" && s.followUps?.some((f) => f.owner.includes(")")))
      return "A follow-up owner cannot contain ')'.";
    if (s.text && headingInText(s.text))
      return `"${s.heading || "Intro"}" has a "## " line; add a new section instead.`;
  }
  return null;
}

/** A "## " line outside code fences would start a new section on a new import. */
function headingInText(text: string): boolean {
  let fence = false;
  for (const l of text.split("\n")) {
    if (l.trim().startsWith("```")) fence = !fence;
    if (!fence && l.startsWith("## ")) return true;
  }
  return false;
}

/** What the interview view shows (KM-91): STAR, the decisions, the lessons. */
export interface Interview {
  star: Star | null;
  decisions: Decision[];
  lessons: string[];
  summary: string;
  /** What to add for a complete interview story. */
  missing: string[];
}

export function interview(r: JournalRecord): Interview {
  const star = r.sections.find((s) => s.kind === "star")?.star ?? null;
  const decisions = r.sections.filter((s) => s.kind === "decisions").flatMap((s) => s.decisions ?? []);
  const lessons = r.sections.filter((s) => s.kind === "lessons" && s.text).map((s) => s.text!);
  const summary = r.sections.find((s) => s.kind === "summary")?.text ?? "";
  const missing: string[] = [];
  const parts: [keyof Star, string][] = [
    ["situation", "Situation"],
    ["task", "Task"],
    ["action", "Action"],
    ["result", "Result"],
  ];
  const empty = parts.filter(([k]) => !star?.[k]?.trim()).map(([, label]) => label);
  if (empty.length === 4) missing.push("a STAR summary");
  else if (empty.length > 0) missing.push(`the STAR ${empty.join(", ")}`);
  if (decisions.filter((d) => d.decision.trim()).length === 0) missing.push("decisions with their reasons and trade-offs");
  else if (decisions.some((d) => d.decision.trim() && (!d.why.trim() || !d.tradeOff.trim())))
    missing.push("a reason and a trade-off for every decision");
  return { star, decisions: decisions.filter((d) => d.decision.trim()), lessons, summary, missing };
}

/** Journal routes in the URL hash. */
export type JournalRoute =
  | { page: "list" }
  | { page: "new"; kind: JournalKind }
  | { page: "view"; id: string }
  | { page: "edit"; id: string }
  | { page: "interview"; id: string }
  | { page: "audit"; cluster?: string };

export function journalRoute(hash: string): JournalRoute | null {
  if (hash === "#/journal") return { page: "list" };
  const n = /^#\/journal\/new(?:\?kind=(incident|changes))?$/.exec(hash);
  if (n) return { page: "new", kind: (n[1] as JournalKind | undefined) ?? "incident" };
  const m = /^#\/journal\/([^/?]+)(?:\/(edit|interview))?$/.exec(hash);
  if (m) {
    const id = safeDecode(m[1]);
    return m[2] === "edit" ? { page: "edit", id } : m[2] === "interview" ? { page: "interview", id } : { page: "view", id };
  }
  const a = /^#\/audit(?:\?cluster=([^&]+))?$/.exec(hash);
  if (a) return a[1] ? { page: "audit", cluster: safeDecode(a[1]) } : { page: "audit" };
  return null;
}

function safeDecode(s: string): string {
  try {
    return decodeURIComponent(s);
  } catch {
    return s;
  }
}

export const journalHref = (id: string, sub?: "edit" | "interview") =>
  `#/journal/${encodeURIComponent(id)}${sub ? "/" + sub : ""}`;

/** Only in-app links (#/...) from evidence are followed. */
export function safeLink(link: string): string | null {
  return /^#\/[A-Za-z0-9\-._~%/?=&]*$/.test(link) ? link : null;
}

// A small Markdown reader for the record's text: paragraphs, lists,
// tables and code blocks, with `code` and **bold** inline. It returns data,
// never HTML, so React escapes everything.

export type Block =
  | { type: "p"; text: string }
  | { type: "ul"; items: string[] }
  | { type: "ol"; items: string[] }
  | { type: "table"; head: string[]; rows: string[][] }
  | { type: "code"; text: string };

export function blocks(text: string): Block[] {
  const out: Block[] = [];
  const lines = text.replace(/\r\n/g, "\n").split("\n");
  let i = 0;
  while (i < lines.length) {
    const l = lines[i];
    if (l.trim() === "") {
      i++;
    } else if (l.trim().startsWith("```")) {
      const body: string[] = [];
      i++;
      while (i < lines.length && !lines[i].trim().startsWith("```")) body.push(lines[i++]);
      i++;
      out.push({ type: "code", text: body.join("\n") });
    } else if (/^\s*[-*] /.test(l)) {
      const items: string[] = [];
      while (i < lines.length && /^\s*[-*] /.test(lines[i])) items.push(lines[i++].replace(/^\s*[-*] /, ""));
      out.push({ type: "ul", items });
    } else if (/^\s*\d+\. /.test(l)) {
      const items: string[] = [];
      while (i < lines.length && /^\s*\d+\. /.test(lines[i])) items.push(lines[i++].replace(/^\s*\d+\. /, ""));
      out.push({ type: "ol", items });
    } else if (l.trim().startsWith("|") && i + 1 < lines.length && /^\s*\|[\s|:-]+\|\s*$/.test(lines[i + 1])) {
      const head = cells(l);
      i += 2;
      const rows: string[][] = [];
      while (i < lines.length && lines[i].trim().startsWith("|")) rows.push(cells(lines[i++]));
      out.push({ type: "table", head, rows });
    } else {
      const p: string[] = [];
      while (i < lines.length && lines[i].trim() !== "" && !/^\s*([-*] |\d+\. |```|\|)/.test(lines[i])) p.push(lines[i++]);
      if (p.length === 0) p.push(lines[i++]);
      out.push({ type: "p", text: p.join(" ") });
    }
  }
  return out;
}

function cells(l: string): string[] {
  const t = l.trim().replace(/^\|/, "").replace(/\|$/, "");
  return t.split(/(?<!\\)\|/).map((c) => c.trim().replace(/\\\|/g, "|"));
}

export type Span = { t: "text" | "code" | "b"; v: string };

/** Splits text into plain, `code` and **bold** spans. */
export function inline(text: string): Span[] {
  const out: Span[] = [];
  const re = /`([^`]+)`|\*\*([^*]+)\*\*/g;
  let last = 0;
  for (let m = re.exec(text); m; m = re.exec(text)) {
    if (m.index > last) out.push({ t: "text", v: text.slice(last, m.index) });
    out.push(m[1] !== undefined ? { t: "code", v: m[1] } : { t: "b", v: m[2] });
    last = m.index + m[0].length;
  }
  if (last < text.length) out.push({ t: "text", v: text.slice(last) });
  return out;
}

/**
 * A browser file name as the portal accepts it for an import's source
 * (letters, digits, ".", "_", "-", ending in .md); "" if nothing is left.
 */
export function sourceName(fileName: string): string {
  let n = fileName.replace(/\.markdown$/i, ".md").replace(/[^A-Za-z0-9._-]+/g, "-").replace(/^[^A-Za-z0-9]+/, "");
  if (!/\.md$/i.test(n)) n = n ? n + ".md" : "";
  else n = n.slice(0, -3) + ".md";
  return n.length > 103 ? n.slice(0, 100).replace(/[^A-Za-z0-9]+$/, "") + ".md" : n;
}

/** Where an entry came from: an imported file, or a Kubemap draft ("draft:<id>"). */
export function sourceText(source: string): string {
  return source.startsWith("draft:") ? "written up from a Kubemap draft" : `imported from ${source}`;
}

/** "2026-09-28" as "28 Sep 2026"; other text as it is. */
export function showDate(d: string): string {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(d);
  if (!m) return d;
  const months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
  return `${Number(m[3])} ${months[Number(m[2]) - 1] ?? m[2]} ${m[1]}`;
}

/** Today in the browser's time zone, as YYYY-MM-DD. */
export function today(now = new Date()): string {
  const p = (n: number) => String(n).padStart(2, "0");
  return `${now.getFullYear()}-${p(now.getMonth() + 1)}-${p(now.getDate())}`;
}
