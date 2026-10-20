import test from "node:test";
import assert from "node:assert/strict";
import { budgetLevel, burnLevel, debugSteps, errorShare, hoursText, percentText, grafanaTracesUrl, grafanaWorkloadUrl, isWorkloadKind, lastValue, parseCustomRange, presetRange } from "./observe.ts";

test("workload link carries namespace, name and lower-case type", () => {
  const u = new URL(grafanaWorkloadUrl("http://g/", "otel-demo", "demo-backend", "Deployment"));
  assert.equal(u.pathname, "/d/a164a7f0339f99e89cea5cb47e9be617/");
  assert.equal(u.searchParams.get("var-workload"), "demo-backend");
  assert.equal(u.searchParams.get("var-type"), "deployment");
});

test("traces link is TraceQL on the service and cannot break out of the quotes", () => {
  const u = new URL(grafanaTracesUrl("http://g", 'a"b'));
  const panes = JSON.parse(u.searchParams.get("panes")!);
  assert.equal(panes.a.queries[0].query, '{resource.service.name="ab"}');
});

test("kinds and last value", () => {
  assert.ok(isWorkloadKind("Deployment.apps"));
  assert.ok(!isWorkloadKind("Service"));
  assert.equal(lastValue([1, null, 3, null]), 3);
  assert.equal(lastValue([null]), undefined);
});

test("debug steps go from pods to traces and mark the first problem", () => {
  const s = debugSteps({ podsReady: 1, podsTotal: 2, restartsDelta: 0, errorRate: 0.5, requestRate: 5, failedTraces: 3, tracesKnown: true });
  assert.deepEqual(s.map((x) => x.level), ["bad", "ok", "bad", "bad"]);
  assert.match(s[2].hint, /10%/);
  const none = debugSteps({ podsReady: 0, podsTotal: 0, failedTraces: 0, tracesKnown: false });
  assert.deepEqual(none.map((x) => x.level), ["bad", "unknown", "unknown", "unknown"]);
});

test("timelineRows orders a tree and positions the bars", async () => {
  const { timelineRows, formatDuration } = await import("./observe.ts");
  const sp = (id: string, parentId: string, startUs: number, durationUs: number) => ({ id, parentId, service: "s", scope: "x", name: id, startUs, durationUs, error: false, detail: "" });
  const rows = timelineRows([sp("c2", "a", 9000, 1000), sp("c1", "a", 1000, 8000), sp("a", "", 0, 10000)]);
  assert.deepEqual(rows.map((r) => [r.id, r.depth]), [["a", 0], ["c1", 1], ["c2", 1]]);
  assert.equal(rows[1].leftPct, 10);
  assert.equal(rows[1].widthPct, 80);
  assert.equal(formatDuration(500), "500 µs");
  assert.equal(formatDuration(15117), "15 ms");
  assert.deepEqual(timelineRows([]), []);
});

test("timelineRows moves a span of another service into its caller when the clocks differ", async () => {
  const { timelineRows } = await import("./observe.ts");
  const sp = (id: string, parentId: string, service: string, startUs: number, durationUs: number) => ({ id, parentId, service, scope: "x", name: id, startUs, durationUs, error: false, detail: "" });
  const rows = timelineRows([sp("b", "f", "backend", 843000, 3000), sp("q", "b", "backend", 844000, 1000), sp("f", "", "frontend", 0, 34000)]);
  const by = Object.fromEntries(rows.map((r) => [r.id, r]));
  assert.equal(by.b.startUs, 15500);
  assert.equal(by.q.startUs, 16500);
  assert.equal(by.b.skewUs, by.q.skewUs);
  assert.equal(by.f.skewUs, 0);
  const fine = timelineRows([sp("f", "", "frontend", 0, 34000), sp("b", "f", "backend", 10000, 3000)]);
  assert.equal(fine[1].skewUs, 0);
});

test("parseCustomRange accepts a window and says what is wrong otherwise", () => {
  const now = new Date("2026-10-20T16:00").getTime();
  const ok = parseCustomRange("2026-10-20T15:00", "2026-10-20T15:05", now);
  assert.ok("range" in ok);
  if ("range" in ok) assert.equal(ok.range.to - ok.range.from, 5 * 60000);
  assert.ok("error" in parseCustomRange("", "2026-10-20T15:05", now));
  assert.ok("error" in parseCustomRange("2026-10-20T15:05", "2026-10-20T15:00", now));
  assert.ok("error" in parseCustomRange("2026-10-01T00:00", "2026-10-20T00:00", now));
  assert.ok("error" in parseCustomRange("2026-10-20T17:00", "2026-10-20T17:05", now));
  const clipped = parseCustomRange("2026-10-20T15:00", "2026-10-20T18:00", now);
  if ("range" in clipped) assert.equal(clipped.range.to, now);
  assert.equal(presetRange("1h", now), undefined);
  assert.equal(presetRange("6h", now)?.from, now - 6 * 3600000);
});

test("error share ignores empty points and no traffic", () => {
  assert.ok(Math.abs((errorShare([0, 1, null], [10, 10, null]) ?? 0) - 0.05) < 1e-9);
  assert.equal(errorShare(undefined, [0, 0]), null);
  assert.equal(errorShare(undefined, [4, 6]), 0);
});

test("burn rate and budget levels", () => {
  assert.equal(burnLevel(0.4), "ok");
  assert.equal(burnLevel(1), "warn");
  assert.equal(burnLevel(14.4), "bad");
  assert.equal(burnLevel(null), "none");
  assert.equal(budgetLevel(0.8), "ok");
  assert.equal(budgetLevel(0.2), "warn");
  assert.equal(budgetLevel(-0.1), "bad");
});

test("percentages and durations read well", () => {
  assert.equal(percentText(0.99952), "99.952%");
  assert.equal(percentText(0.5), "50.0%");
  assert.equal(percentText(null), "no traffic");
  assert.equal(hoursText(0.5), "30 min");
  assert.equal(hoursText(8.4), "8.4 h");
  assert.equal(hoursText(72), "3.0 days");
});
