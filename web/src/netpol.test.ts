// Run with: npm test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { checkQuery, emptyForm, matrixCounts } from "./netpol.ts";

test("pod to Service query", () => {
  const q = checkQuery({ ...emptyForm, srcNamespace: "shop", srcPod: "web-1", dstNamespace: "shop", dstName: "api", port: "80" });
  assert.equal(q.missing, undefined);
  assert.equal(q.params!.toString(), "fromNamespace=shop&fromPod=web-1&toNamespace=shop&toService=api&port=80&protocol=TCP");
});

test("IP ends and pod destination", () => {
  const q = checkQuery({ ...emptyForm, srcKind: "ip", srcIp: " 10.0.0.9 ", dstKind: "pod", dstNamespace: "a", dstName: "b", port: "53", protocol: "UDP" });
  assert.equal(q.params!.toString(), "fromIp=10.0.0.9&toNamespace=a&toPod=b&port=53&protocol=UDP");
  const r = checkQuery({ ...emptyForm, srcNamespace: "a", srcPod: "b", dstKind: "ip", dstIp: "1.1.1.1", port: "443" });
  assert.equal(r.params!.get("toIp"), "1.1.1.1");
  assert.equal(r.params!.has("toNamespace"), false);
});

test("what is missing, in order", () => {
  assert.equal(checkQuery(emptyForm).missing, "Choose the source pod.");
  assert.equal(checkQuery({ ...emptyForm, srcNamespace: "a", srcPod: "b" }).missing, "Choose the destination Service.");
  assert.equal(checkQuery({ ...emptyForm, srcNamespace: "a", srcPod: "b", dstNamespace: "a", dstName: "c", port: "70000" }).missing, "Enter a port from 1 to 65535.");
  assert.equal(checkQuery({ ...emptyForm, srcKind: "ip" }).missing, "Enter the source IP address.");
});

test("matrix counts", () => {
  assert.deepEqual(
    matrixCounts([[{ allowed: true }, { allowed: false }], [{ allowed: false, error: "x" }, { allowed: false, noDestination: true }]]),
    { allowed: 1, blocked: 1, empty: 1, errors: 1 },
  );
});
