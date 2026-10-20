// Run with: npm test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { qrMatrix, qrPath } from "./qr.ts";

const uri = "otpauth://totp/Kubemap:admin?algorithm=SHA1&digits=6&issuer=Kubemap&period=30&secret=BWVJTRHNCEVWAG3GLU53O5L26KCTCGZ7";

test("the matrix is square and has the three finder squares", () => {
  const m = qrMatrix(uri);
  const n = m.length;
  assert.ok(n >= 21 && n <= 177);
  assert.ok(m.every((row) => row.length === n));
  // The top-left finder square: a dark corner, a dark 7-module top edge.
  assert.equal(m[0][0], true);
  assert.deepEqual(m[0].slice(0, 7), Array(7).fill(true));
  // Top-right and bottom-left finders start with a dark corner too.
  assert.equal(m[0][n - 1], true);
  assert.equal(m[n - 1][0], true);
});

test("the same text gives the same code, other text another", () => {
  assert.deepEqual(qrMatrix(uri), qrMatrix(uri));
  assert.notDeepEqual(qrMatrix(uri), qrMatrix(uri + "x"));
});

test("the path has one square per dark module", () => {
  const m = qrMatrix("hi");
  const dark = m.flat().filter(Boolean).length;
  assert.equal(qrPath(m).split("z").length - 1, dark);
  assert.match(qrPath([[true, false], [false, true]]), /^M0 0h1v1h-1zM1 1h1v1h-1z$/);
});
