// Run with: npm test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { entryTime, highlight, readSearch, stackLineKind, writeSearch } from "./logsearch.ts";

test("highlight marks every match, ignoring case, and keeps the text", () => {
  const p = highlight("Pool exhausted; pool=20 POOL", "pool");
  assert.deepEqual(
    p.map((x) => (x.match ? `[${x.text}]` : x.text)).join(""),
    "[Pool] exhausted; [pool]=20 [POOL]",
  );
  assert.deepEqual(highlight("abc", ""), [{ text: "abc", match: false }]);
  assert.deepEqual(highlight("abc", "x"), [{ text: "abc", match: false }]);
  assert.equal(highlight("aaaa", "aa").filter((x) => x.match).length, 2);
});

test("stack lines are told apart: causes, frames, text", () => {
  assert.equal(stackLineKind("Caused by: java.net.ConnectException: refused"), "cause");
  assert.equal(stackLineKind("psycopg2.OperationalError: FATAL:  too many clients"), "cause");
  assert.equal(stackLineKind("goroutine 1 [running]:"), "cause");
  assert.equal(stackLineKind("\tat com.example.Pool.get(Pool.java:42)"), "frame");
  assert.equal(stackLineKind('  File "/app/app.py", line 12, in handler'), "frame");
  assert.equal(stackLineKind("\t/src/main.go:42 +0x1c"), "frame");
  assert.equal(stackLineKind("main.main()"), "frame");
  assert.equal(stackLineKind("\t... 12 more"), "frame");
  assert.equal(stackLineKind("    conn = pool.getconn()"), "text");
  assert.equal(stackLineKind("  port: 8080"), "text");
});

test("entry times are local, with the date only when not today", () => {
  const now = new Date(2026, 8, 28, 18, 0, 0);
  assert.equal(entryTime(new Date(2026, 8, 28, 7, 5, 9).toISOString(), now), "07:05:09");
  assert.equal(entryTime(new Date(2026, 8, 27, 23, 59, 0).toISOString(), now), "2026-09-27 23:59:00");
  assert.equal(entryTime("not a time", now), "");
});

test("the search round-trips through the URL, with safe defaults", () => {
  const d = readSearch("argocd/url-shortener", undefined);
  assert.deepEqual(d, { app: "argocd/url-shortener", component: undefined, q: undefined, levels: ["error", "warn"], since: "1h", previous: true });
  const s = { app: "a/b", component: "Deployment/ns/web", q: "pool exhausted", levels: ["error" as const], since: "24h", previous: false };
  assert.deepEqual(readSearch("a/b", writeSearch(s)), s);
  // Every level (an empty list) survives too; bad values fall back.
  assert.deepEqual(readSearch("a/b", writeSearch({ ...s, levels: [] })).levels, []);
  const bad = readSearch("a/b", "levels=loud,error&since=10y");
  assert.deepEqual(bad.levels, ["error"]);
  assert.equal(bad.since, "1h");
});
