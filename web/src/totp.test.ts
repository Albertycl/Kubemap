// Run with: npm test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { groupSecret, isTotpCode, looksLikeCode, normalizeCode, recoveryText, totpLoginError, totpStatusText } from "./totp.ts";

test("codes are normalized", () => {
  assert.equal(normalizeCode(" 123 456 "), "123456");
  assert.equal(normalizeCode("abcd-efgh-ijkl"), "ABCDEFGHIJKL");
  assert.equal(isTotpCode("123 456"), true);
  assert.equal(isTotpCode("12345"), false);
  assert.equal(isTotpCode("12345a"), false);
});

test("what can be submitted as a code", () => {
  assert.equal(looksLikeCode("123456"), true);
  assert.equal(looksLikeCode("abcd-efgh-ijkl"), true);
  assert.equal(looksLikeCode("abcd-efgh-ij18"), false); // 1 and 8 are not base32
  assert.equal(looksLikeCode(""), false);
  assert.equal(looksLikeCode("1234567"), false);
});

test("the secret is grouped by four", () => {
  assert.equal(groupSecret("GEZDGNBVGY3TQOJQ"), "GEZD GNBV GY3T QOJQ");
  assert.equal(groupSecret("GEZDG"), "GEZD G");
  assert.equal(groupSecret("GEZD GNBV"), "GEZD GNBV");
});

test("login error texts", () => {
  assert.match(totpLoginError("totp_required") ?? "", /6-digit code/);
  assert.match(totpLoginError("bad_totp") ?? "", /Wrong or already used/);
  assert.equal(totpLoginError("bad_credentials"), undefined);
  assert.equal(totpLoginError(undefined), undefined);
});

test("status text", () => {
  assert.equal(totpStatusText(false, 0), "Two-factor login is off.");
  assert.equal(totpStatusText(true, 8), "Two-factor login is on (8 recovery codes left).");
  assert.match(totpStatusText(true, 1), /Only 1 recovery code left/);
  assert.equal(recoveryText(["A", "B"]), "A\nB");
});
