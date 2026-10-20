// Run with: npm test (node --test on src/*.test.ts). Node strips the types itself.
import { test } from "node:test";
import assert from "node:assert/strict";
import { edgePath, lanePadding, laneY, pathData, pointAt, routeLabel, skipsColumns, type Box, type Edge } from "./arrows.ts";

// flask-vue as the page lays it out: three columns, boxes at the same height,
// with a lane padding above them.
const top = lanePadding(1) + 20;
const entry: Box = { left: 0, right: 200, top, bottom: top + 70, col: 0 };
const frontend: Box = { left: 240, right: 440, top, bottom: top + 70, col: 1 };
const backend: Box = { left: 480, right: 680, top, bottom: top + 70, col: 2 };
const occupied = new Set([0, 1, 2]);

function samples(e: Edge) {
  return e.segments.flatMap((s) => Array.from({ length: 41 }, (_, i) => pointAt(s, i / 40)));
}

function inside(p: { x: number; y: number }, b: Box) {
  return p.x > b.left + 0.5 && p.x < b.right - 0.5 && p.y > b.top + 0.5 && p.y < b.bottom - 0.5;
}

test("a link that skips a column goes around the boxes in it (review-km-53-page B1)", () => {
  const e = edgePath(entry, backend, occupied, 0);
  const pts = samples(e);
  for (const p of pts) {
    assert.ok(!inside(p, frontend), `passes through the frontend box at ${p.x},${p.y}`);
    assert.ok(!inside(p, entry) && !inside(p, backend), `passes through an end box at ${p.x},${p.y}`);
  }
  // Starts at the entry's right edge, ends at the backend's left edge.
  assert.deepEqual(pts[0], { x: 200, y: top + 35 });
  assert.deepEqual(pts.at(-1), { x: 478, y: top + 35 });
  // Its highest point is the lane, above every box.
  assert.ok(Math.abs(Math.min(...pts.map((p) => p.y)) - laneY(0)) < 1e-9);
  assert.ok(laneY(0) < top);
  // The label sits on the lane, above the frontend column.
  assert.ok(e.label.y < laneY(0) && e.label.x > frontend.left && e.label.x < frontend.right);
});

test("lanes of several skipping links are separate and all above the boxes", () => {
  const a = edgePath(entry, backend, occupied, 0);
  const b = edgePath(entry, backend, occupied, 1);
  const ya = Math.min(...samples(a).map((p) => p.y));
  const yb = Math.min(...samples(b).map((p) => p.y));
  assert.notEqual(ya, yb);
  assert.ok(Math.max(ya, yb) < top, "a lane runs into the boxes");
  assert.ok(lanePadding(2) > laneY(1));
  assert.equal(lanePadding(0), 0);
});

test("neighbouring columns: a curve through the gap only", () => {
  const e = edgePath(entry, frontend, occupied, 0);
  for (const p of samples(e)) {
    assert.ok(p.x >= 200 && p.x <= 240, `leaves the gap at x=${p.x}`);
  }
  assert.equal(e.label.anchor, "end");
});

test("a link past an empty column is not re-routed", () => {
  // Backend (col 2) to cache (col 4) with no database column in between.
  assert.equal(skipsColumns(2, 4, new Set([0, 1, 2, 4])), false);
  assert.equal(skipsColumns(0, 2, occupied), true);
  assert.equal(skipsColumns(2, 0, occupied), true);
});

test("backwards and same-column links stay out of other boxes", () => {
  const back = edgePath(backend, entry, occupied, 0);
  for (const p of samples(back)) assert.ok(!inside(p, frontend), `backwards link crosses the frontend at ${p.x},${p.y}`);
  assert.deepEqual(samples(back)[0], { x: 480, y: top + 35 });

  const below: Box = { ...frontend, top: frontend.bottom + 10, bottom: frontend.bottom + 80 };
  const loop = edgePath(frontend, below, occupied, 0);
  for (const p of samples(loop)) {
    assert.ok(p.x >= 440 && p.x < backend.left, `same-column loop leaves the gap at x=${p.x}`);
  }
});

test("path data starts with M and has one command per segment", () => {
  const d = pathData(edgePath(entry, backend, occupied, 0));
  assert.match(d, /^M200,\d/);
  assert.equal((d.match(/[LQC]/g) ?? []).length, 9);
});

test("route labels show the paths", () => {
  assert.equal(routeLabel(["flask-vue.albertycliu.xyz/api"]), "/api");
  assert.equal(routeLabel(["a.example.com/", "a.example.com/"]), "/");
  assert.equal(routeLabel(["a.example.com/v1", "b.example.com/v2"]), "/v1, /v2");
  assert.equal(routeLabel(["* (default backend)"]), "/");
});
