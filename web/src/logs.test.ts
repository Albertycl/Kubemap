// Run with: npm test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { AnsiStripper, base64Bytes, LogBuffer, NdjsonSplitter, stripAnsi } from "./logs.ts";

const enc = new TextEncoder();

test("NDJSON lines split across chunks are joined", () => {
  const s = new NdjsonSplitter();
  const whole = '{"data":"aGVsbG8K"}\n{"end":{"reason":"log ended"}}\n';
  const bytes = enc.encode(whole);
  const got = [...s.push(bytes.slice(0, 7)), ...s.push(bytes.slice(7, 30)), ...s.push(bytes.slice(30))];
  assert.equal(got.length, 2);
  assert.equal(got[0].data, "aGVsbG8K");
  assert.equal(got[1].end?.reason, "log ended");
  assert.deepEqual(s.push(enc.encode("\n\n")), []);
});

test("UTF-8 split across two data chunks decodes whole", () => {
  const all = enc.encode("naïve ✓\n");
  const cut = 3; // inside "ï"
  const a = btoa(String.fromCharCode(...all.slice(0, cut)));
  const b = btoa(String.fromCharCode(...all.slice(cut)));
  const dec = new TextDecoder();
  const text = dec.decode(base64Bytes(a), { stream: true }) + dec.decode(base64Bytes(b), { stream: true });
  assert.equal(text, "naïve ✓\n");
});

test("ANSI colour and OSC codes are removed, text kept", () => {
  assert.equal(stripAnsi("\x1b[90m2026-09-27\x1b[0m \x1b[32mINF\x1b[0m ok"), "2026-09-27 INF ok");
  assert.equal(stripAnsi("\x1b]0;title\x07after"), "after");
  assert.equal(stripAnsi("plain <b>text</b>"), "plain <b>text</b>");
});

test("the log buffer drops the oldest whole lines past its cap", () => {
  const b = new LogBuffer(20);
  b.append("line-1\nline-2\n");
  b.append("line-3\nline-4\n");
  assert.ok(b.text.length <= 20);
  assert.ok(b.text.startsWith("line-"), b.text);
  assert.ok(b.text.endsWith("line-4\n"));
  assert.ok(b.dropped > 0);
  b.clear();
  assert.equal(b.text, "");
  assert.equal(b.dropped, 0);
});

test("an escape code split across chunks is still removed", () => {
  const a = new AnsiStripper();
  assert.equal(a.push("start \x1b[3"), "start ");
  assert.equal(a.push("2mgreen\x1b[0m end\n"), "green end\n");
  assert.equal(a.push("no codes\n"), "no codes\n");
  // An ESC that never becomes a code is not held forever.
  assert.equal(a.push("x\x1b"), "x");
  assert.equal(a.push("Zy"), "\x1bZy");
});
