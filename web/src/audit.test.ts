import { test } from "node:test";
import assert from "node:assert/strict";
import { auditGroup, resultLevel } from "./audit.ts";

test("audit groups and levels", () => {
  assert.equal(auditGroup("journal.update"), "journal");
  assert.equal(auditGroup("login"), "login");
  assert.equal(auditGroup("cluster.accept"), "other");
  assert.equal(resultLevel("ok"), "ok");
  assert.equal(resultLevel("refused (forbidden): x"), "fail");
  assert.equal(resultLevel("unknown: no answer in time"), "warn");
});
