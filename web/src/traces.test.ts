// Run with: npm test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { traceSummary } from "./traces.ts";

const base = { source: "tempo http://tempo.observability.svc:3200", window: "1h0m0s", spans: 201, at: "2026-09-28T14:00:00Z" };

test("trace summary", () => {
  assert.equal(traceSummary(undefined), null);
  assert.deepEqual(traceSummary({ ...base, probes: 200, confirmed: 1 }), {
    text: "Traces (tempo http://tempo.observability.svc:3200, last 1h): 1 link confirmed by real calls from 1 span (200 kubelet probe spans ignored).",
    level: "ok",
  });
  assert.equal(traceSummary({ ...base, spans: 200, probes: 200, confirmed: 0 })!.level, "muted");
  const u = traceSummary({ ...base, confirmed: 2, unmatched: ["a calls b (client spans): no link on the map"] })!;
  assert.equal(u.level, "warn");
  assert.match(u.text, /1 call in the traces have no link on the map/);
  assert.match(traceSummary({ ...base, confirmed: 0, error: "tempo: search answered 503" })!.text, /could not read .* 503/);
});

test("with nothing confirmed, the summary says only spans with a user agent count (review km-54 N-3)", () => {
  assert.match(traceSummary({ ...base, confirmed: 0 })!.text, /Only spans with a user agent are used/);
  assert.doesNotMatch(traceSummary({ ...base, confirmed: 2 })!.text, /user agent/);
});
