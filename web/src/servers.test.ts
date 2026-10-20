// Run with: npm test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { serverLevel, sortServers } from "./servers.ts";
import type { ServerInfo } from "./api.ts";

const base: ServerInfo = { clusterId: "c", clusterName: "lab", clusterStatus: "active", name: "n", status: "connected", connected: true };
const press = (verdict: "ok" | "elevated" | "pressure" | "unknown", ready = true) => ({
  node: "n", ready, psiAvailable: true, verdict, summary: verdict + " summary", resources: [],
});

test("levels and reasons", () => {
  assert.deepEqual(serverLevel({ ...base, pressure: press("ok") }), { level: "ok", reason: "ok summary" });
  assert.equal(serverLevel({ ...base, pressure: press("ok", false) }).reason, "NotReady");
  assert.equal(serverLevel({ ...base, connected: false }).level, "fail");
  assert.equal(serverLevel({ ...base, pressure: press("pressure") }).level, "fail");
  assert.equal(serverLevel({ ...base, pressure: press("elevated") }).level, "warn");
  assert.equal(serverLevel({ ...base, status: "none", connected: false }).reason, "no node agent on this node");
  assert.equal(serverLevel({ ...base, stale: true }).reason, "numbers are not current");
  assert.equal(serverLevel({ ...base, pressure: press("unknown") }).level, "warn");
});

test("worst first, then cluster and name", () => {
  const got = sortServers([
    { ...base, name: "b" },
    { ...base, name: "a", pressure: press("elevated") },
    { ...base, clusterName: "alpha", name: "z", connected: false },
    { ...base, name: "c", pressure: press("pressure") },
  ]);
  assert.deepEqual(got.map((s) => s.clusterName + "/" + s.name), ["alpha/z", "lab/c", "lab/a", "lab/b"]);
});

test("KM-65 state: node down and node agent down read differently", () => {
  const st = (code: string, severity: "ok" | "warn" | "fail", title: string) =>
    ({ code, severity, title, detail: title + " detail" }) as ServerInfo["state"];
  assert.deepEqual(serverLevel({ ...base, connected: false, state: st("node-down", "fail", "Node down") }), { level: "fail", reason: "Node down" });
  assert.deepEqual(serverLevel({ ...base, connected: false, state: st("agent-down", "warn", "Node agent down") }), {
    level: "warn",
    reason: "Node agent down",
  });
  assert.equal(serverLevel({ ...base, connected: false, state: st("unreachable", "fail", "Unreachable") }).reason, "Unreachable");
  assert.equal(serverLevel({ ...base, state: st("not-ready", "fail", "Up, but NotReady") }).level, "fail");
  // A reporting node falls through to the pressure verdict as before.
  assert.equal(serverLevel({ ...base, state: st("ok", "ok", "Up"), pressure: press("elevated") }).level, "warn");
});
