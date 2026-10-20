// Geometry of the app map's arrows (KM-53), kept free of React and the DOM
// so it can be tested with node --test.
//
// Boxes sit in tier columns. A link between neighbouring columns is a curve
// through the gap between them. A link that skips a column with boxes in it
// (entry → backend past the frontend) must not be drawn through those boxes,
// or it reads as a link that does not exist; it goes up through the gap
// next to its source, along a lane above all boxes, and down through the gap
// next to its target.

export interface Box {
  left: number;
  right: number;
  top: number;
  bottom: number;
  /** Tier column, 0 = leftmost. */
  col: number;
}

export interface Point {
  x: number;
  y: number;
}

export type Segment =
  | { kind: "L"; from: Point; to: Point }
  | { kind: "Q"; from: Point; c: Point; to: Point }
  | { kind: "C"; from: Point; c1: Point; c2: Point; to: Point };

export interface Edge {
  segments: Segment[];
  /** Where a label (route path) goes, and how it is anchored. */
  label: Point & { anchor: "start" | "middle" | "end" };
}

/** Space between the top of the grid and the first lane, and between lanes. */
export const LANE_TOP = 12;
export const LANE_STEP = 16;

/** The y of lane i, relative to the grid's top. */
export function laneY(i: number): number {
  return LANE_TOP + i * LANE_STEP;
}

/** Padding the grid needs above its boxes for n lanes. */
export function lanePadding(n: number): number {
  return n > 0 ? laneY(n - 1) + LANE_STEP : 0;
}

/** True when a link from column a to column b passes a column holding boxes. */
export function skipsColumns(a: number, b: number, occupied: Set<number>): boolean {
  for (let c = Math.min(a, b) + 1; c < Math.max(a, b); c++) if (occupied.has(c)) return true;
  return false;
}

const INSET = 16; // how far into the gap a lane route turns
const RADIUS = 6; // rounded corners of lane routes
const LOOP = 28; // same-column loops go this far right

function mid(b: Box): number {
  return (b.top + b.bottom) / 2;
}

/**
 * The path of one link from box a to box b. lane is the lane index for a
 * link that skips columns (ignored otherwise).
 */
export function edgePath(a: Box, b: Box, occupied: Set<number>, lane: number): Edge {
  const ay = mid(a);
  const by = mid(b);
  if (a.col === b.col) {
    // Same column: loop out through the gap on the right.
    const x1 = a.right;
    const x2 = b.right + 2;
    const x = Math.max(x1, x2) + LOOP;
    return {
      segments: [{ kind: "C", from: { x: x1, y: ay }, c1: { x, y: ay }, c2: { x, y: by }, to: { x: x2, y: by } }],
      label: { x: x + 3, y: (ay + by) / 2, anchor: "start" },
    };
  }
  const dir = b.col > a.col ? 1 : -1;
  const x1 = dir > 0 ? a.right : a.left;
  const x2 = dir > 0 ? b.left - 2 : b.right + 2;
  if (!skipsColumns(a.col, b.col, occupied)) {
    const dx = Math.max(24, Math.abs(x2 - x1) / 2) * dir;
    return {
      segments: [{ kind: "C", from: { x: x1, y: ay }, c1: { x: x1 + dx, y: ay }, c2: { x: x2 - dx, y: by }, to: { x: x2, y: by } }],
      label: { x: x2 - 4 * dir, y: by - 6, anchor: dir > 0 ? "end" : "start" },
    };
  }
  // Around the boxes: up the gap after a, along the lane, down the gap before b.
  const y = laneY(lane);
  const g1 = x1 + INSET * dir;
  const g2 = x2 - INSET * dir;
  const r = RADIUS;
  const segments: Segment[] = [
    { kind: "L", from: { x: x1, y: ay }, to: { x: g1 - r * dir, y: ay } },
    { kind: "Q", from: { x: g1 - r * dir, y: ay }, c: { x: g1, y: ay }, to: { x: g1, y: ay - r } },
    { kind: "L", from: { x: g1, y: ay - r }, to: { x: g1, y: y + r } },
    { kind: "Q", from: { x: g1, y: y + r }, c: { x: g1, y }, to: { x: g1 + r * dir, y } },
    { kind: "L", from: { x: g1 + r * dir, y }, to: { x: g2 - r * dir, y } },
    { kind: "Q", from: { x: g2 - r * dir, y }, c: { x: g2, y }, to: { x: g2, y: y + r } },
    { kind: "L", from: { x: g2, y: y + r }, to: { x: g2, y: by - r } },
    { kind: "Q", from: { x: g2, y: by - r }, c: { x: g2, y: by }, to: { x: g2 + r * dir, y: by } },
    { kind: "L", from: { x: g2 + r * dir, y: by }, to: { x: x2, y: by } },
  ];
  return { segments, label: { x: (g1 + g2) / 2, y: y - 4, anchor: "middle" } };
}

/** SVG path data for an edge. */
export function pathData(e: Edge): string {
  const p = (q: Point) => `${round(q.x)},${round(q.y)}`;
  let d = e.segments.length ? `M${p(e.segments[0].from)}` : "";
  for (const s of e.segments) {
    if (s.kind === "L") d += ` L${p(s.to)}`;
    else if (s.kind === "Q") d += ` Q${p(s.c)} ${p(s.to)}`;
    else d += ` C${p(s.c1)} ${p(s.c2)} ${p(s.to)}`;
  }
  return d;
}

function round(n: number): number {
  return Math.round(n * 10) / 10;
}

/** A point on a segment, t in [0, 1]; used by the tests. */
export function pointAt(s: Segment, t: number): Point {
  const u = 1 - t;
  if (s.kind === "L") return { x: u * s.from.x + t * s.to.x, y: u * s.from.y + t * s.to.y };
  if (s.kind === "Q")
    return {
      x: u * u * s.from.x + 2 * u * t * s.c.x + t * t * s.to.x,
      y: u * u * s.from.y + 2 * u * t * s.c.y + t * t * s.to.y,
    };
  return {
    x: u * u * u * s.from.x + 3 * u * u * t * s.c1.x + 3 * u * t * t * s.c2.x + t * t * t * s.to.x,
    y: u * u * u * s.from.y + 3 * u * u * t * s.c1.y + 3 * u * t * t * s.c2.y + t * t * t * s.to.y,
  };
}

/**
 * Route labels: the paths a link's route evidence names, for example
 * "flask-vue.example.com/api" → "/api". Several paths are joined with ", ".
 */
export function routeLabel(names: string[]): string {
  const paths = names.map((n) => {
    const i = n.indexOf("/");
    return i < 0 ? "/" : n.slice(i);
  });
  return [...new Set(paths)].join(", ");
}
