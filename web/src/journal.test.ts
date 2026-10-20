// Run with: npm test.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  blocks,
  emptySection,
  inline,
  interview,
  journalRoute,
  move,
  newRecord,
  recordProblem,
  removeAt,
  safeLink,
  showDate,
  sourceName,
  sourceText,
  today,
} from "./journal.ts";
import type { JournalRecord } from "./api";

test("routes", () => {
  assert.deepEqual(journalRoute("#/journal"), { page: "list" });
  assert.deepEqual(journalRoute("#/journal/new"), { page: "new", kind: "incident" });
  assert.deepEqual(journalRoute("#/journal/new?kind=changes"), { page: "new", kind: "changes" });
  assert.deepEqual(journalRoute("#/journal/inc-1"), { page: "view", id: "inc-1" });
  assert.deepEqual(journalRoute("#/journal/inc-1/edit"), { page: "edit", id: "inc-1" });
  assert.deepEqual(journalRoute("#/journal/inc-1/interview"), { page: "interview", id: "inc-1" });
  assert.deepEqual(journalRoute("#/journal/%E0"), { page: "view", id: "%E0" });
  assert.deepEqual(journalRoute("#/audit"), { page: "audit" });
  assert.deepEqual(journalRoute("#/audit?cluster=c-1"), { page: "audit", cluster: "c-1" });
  assert.equal(journalRoute("#/journal/inc-1/other"), null);
  assert.equal(journalRoute("#/clusters"), null);
});

test("new records have the sections of a good write-up", () => {
  const r = newRecord("incident", "2026-10-01");
  assert.deepEqual(
    r.sections.map((s) => s.kind),
    ["summary", "timeline", "rootcause", "detection", "decisions", "followups", "star", "lessons"],
  );
  assert.equal(r.sections[1].heading, "Timeline");
  assert.equal(recordProblem(r), "Give it a title.");
  const c = newRecord("changes", "2026-10-01");
  assert.equal(c.sections[0].heading, "");
  assert.equal(c.sections[0].changes?.length, 1);
  assert.equal(recordProblem(c), null);
});

test("record problems", () => {
  const r: JournalRecord = { kind: "incident", title: "x", date: "1/10/2026", cluster: "", severity: "", sections: [] };
  assert.match(recordProblem(r)!, /YYYY-MM-DD/);
  r.date = "";
  r.sections = [{ ...emptySection("text"), text: "a\n## b" }];
  assert.match(recordProblem(r)!, /## /);
  r.sections = [{ ...emptySection("text"), text: "```\n## in code\n```" }];
  assert.equal(recordProblem(r), null);
  r.sections = [{ kind: "followups", heading: "F", followUps: [{ status: "todo", owner: "a)", text: "" }] }];
  assert.match(recordProblem(r)!, /owner/);
});

test("list edits", () => {
  assert.deepEqual(move([1, 2, 3], 0, 1), [2, 1, 3]);
  const same = [1, 2];
  assert.equal(move(same, 0, -1), same);
  assert.deepEqual(removeAt([1, 2, 3], 1), [1, 3]);
});

test("interview view: STAR, decisions and what is missing", () => {
  const r = newRecord("incident", "2026-10-01");
  let v = interview(r);
  assert.equal(v.star?.situation, "");
  assert.deepEqual(v.missing, ["a STAR summary", "decisions with their reasons and trade-offs"]);
  const star = r.sections.find((s) => s.kind === "star")!;
  star.star = { situation: "Every app stopped", task: "Restore", action: "", result: "" };
  const dec = r.sections.find((s) => s.kind === "decisions")!;
  dec.decisions = [{ decision: "Stop builds", why: "Stop the damage", tradeOff: "" }];
  r.sections.find((s) => s.kind === "lessons")!.text = "Ready is not healthy.";
  v = interview(r);
  assert.deepEqual(v.missing, ["the STAR Action, Result", "a reason and a trade-off for every decision"]);
  assert.equal(v.decisions.length, 1);
  assert.deepEqual(v.lessons, ["Ready is not healthy."]);
});

test("markdown blocks", () => {
  const b = blocks(
    "First line\nsame paragraph.\n\n- one\n- two `x`\n\n| A | B |\n|---|---|\n| 1 | a \\| b |\n\n```\n## code\n```\n\n1. first\n2. second",
  );
  assert.deepEqual(b, [
    { type: "p", text: "First line same paragraph." },
    { type: "ul", items: ["one", "two `x`"] },
    { type: "table", head: ["A", "B"], rows: [["1", "a | b"]] },
    { type: "code", text: "## code" },
    { type: "ol", items: ["first", "second"] },
  ]);
});

test("inline spans escape nothing themselves", () => {
  assert.deepEqual(inline("Run `df -h /` then **stop** <b>"), [
    { t: "text", v: "Run " },
    { t: "code", v: "df -h /" },
    { t: "text", v: " then " },
    { t: "b", v: "stop" },
    { t: "text", v: " <b>" },
  ]);
});

test("evidence links stay in the app", () => {
  assert.equal(safeLink("#/clusters/c-1/problems"), "#/clusters/c-1/problems");
  assert.equal(safeLink("#/audit?cluster=c-1"), "#/audit?cluster=c-1");
  assert.equal(safeLink("javascript:alert(1)"), null);
  assert.equal(safeLink("https://evil.example/"), null);
  assert.equal(safeLink('#/x" onclick="y'), null);
});

test("dates", () => {
  assert.equal(showDate("2026-09-28"), "28 Sep 2026");
  assert.equal(showDate(""), "");
  assert.equal(today(new Date(2026, 8, 5)), "2026-09-05");
});

test("import file names", () => {
  assert.equal(sourceName("2026-09-28-disk-pressure.md"), "2026-09-28-disk-pressure.md");
  assert.equal(sourceName("My incident (copy).MD"), "My-incident-copy-.md");
  assert.equal(sourceName("notes.markdown"), "notes.md");
  assert.equal(sourceName("../x"), "x.md");
  assert.equal(sourceName("..."), "");
  assert.match(sourceName("a".repeat(300) + ".md"), /^a{100}\.md$/);
});

test("entry sources", () => {
  assert.equal(sourceText("draft:draft-c-1-1790000000"), "written up from a Kubemap draft");
  assert.equal(sourceText("2026-09-28.md"), "imported from 2026-09-28.md");
});
