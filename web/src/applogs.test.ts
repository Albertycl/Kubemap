// Run with: npm test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { appPods, compareTime, entryKey, FOLLOW_EVERY_MS, FOLLOW_MAX_MS, followFrom, mergeEntries, nextFollowDelay, podColours, PALETTE_SIZE } from "./applogs.ts";
import type { LogEntry, MapApp } from "./api.ts";

const comp = (id: string, tier: string, pods: string[]) => {
  const [kind, namespace, name] = id.split("/");
  return { id, kind, namespace, name, tier, tierReason: "", healthy: true, pods: pods.map((n) => ({ name: n, status: "Running", ready: true, restarts: 0 })) };
};
const app = {
  id: "argocd/url-shortener", name: "url-shortener", namespace: "sre-lab-app", source: "argocd", health: "Healthy", state: "ok",
  components: [
    comp("Cluster.postgresql.cnpg.io/sre-lab-app/url-shortener-db", "database", ["url-shortener-db-2", "url-shortener-db-1"]),
    comp("Deployment/sre-lab-app/url-shortener", "backend", ["url-shortener-b", "url-shortener-a"]),
    comp("IngressRoute.traefik.io/sre-lab-app/url-shortener-http", "entry", []),
    comp("Deployment/sre-lab-app/url-shortener-pooler", "database", ["pooler-x"]),
  ],
} as unknown as MapApp;

test("an app's pods go from the entry to the data, then by component and name", () => {
  const pods = appPods(app);
  assert.deepEqual(pods.map((p) => p.pod), ["url-shortener-a", "url-shortener-b", "url-shortener-db-1", "url-shortener-db-2", "pooler-x"]);
  assert.equal(pods[0].component, "Deployment url-shortener");
  assert.equal(pods[2].component, "Cluster url-shortener-db");
  assert.equal(pods[2].key, "sre-lab-app/url-shortener-db-1");
  assert.deepEqual(appPods(undefined), []);
});

test("every pod gets its own colour while the palette lasts", () => {
  const keys = Array.from({ length: PALETTE_SIZE }, (_, i) => `ns/p${i}`);
  const c = podColours(keys);
  assert.equal(new Set(c.values()).size, PALETTE_SIZE);
  // Pods the map does not know take the next slots, and known ones keep theirs.
  const c2 = podColours(["ns/a", "ns/b"], ["ns/old", "ns/a"]);
  assert.deepEqual([...c2.entries()], [["ns/a", 0], ["ns/b", 1], ["ns/old", 2]]);
});

const e = (time: string, pod: string, text: string): LogEntry => ({ time, namespace: "ns", pod, container: "c", level: "info", text });

test("follow adds only new lines, in time order, up to the cap", () => {
  const shown = [e("2026-09-28T10:00:00Z", "a", "one"), e("2026-09-28T10:00:02Z", "b", "two")];
  const fresh = [e("2026-09-28T10:00:02Z", "b", "two"), e("2026-09-28T10:00:01Z", "a", "late"), e("2026-09-28T10:00:03Z", "a", "three")];
  const m = mergeEntries(shown, fresh, 100);
  assert.equal(m.added, 2);
  assert.deepEqual(m.entries.map((x) => x.text), ["one", "late", "two", "three"]);
  assert.equal(mergeEntries(m.entries, fresh, 100).added, 0);
  assert.deepEqual(mergeEntries(shown, fresh, 3).entries.map((x) => x.text), ["late", "two", "three"]);
  // The same text at the same time from two pods is two lines.
  assert.notEqual(entryKey(e("t", "a", "x")), entryKey(e("t", "b", "x")));
});

test("follow reads from the newest line, or the end of the window", () => {
  assert.equal(followFrom([e("2026-09-28T10:00:03Z", "a", "x")], "2026-09-28T10:00:00Z"), "2026-09-28T10:00:03Z");
  assert.equal(followFrom([], "2026-09-28T10:05:00Z"), "2026-09-28T10:05:00Z");
  assert.equal(followFrom([e("2026-09-28T10:00:03Z", "a", "x")], "2026-09-28T10:05:00Z"), "2026-09-28T10:05:00Z");
});

test("times compare as times, not as text", () => {
  assert.ok(compareTime("2026-09-28T10:00:00Z", "2026-09-28T10:00:00.1Z") < 0);
  assert.ok(compareTime("2026-09-28T10:00:00.5Z", "2026-09-28T10:00:00.123456789Z") > 0);
  assert.ok(compareTime("2026-09-28T10:00:00.000000002Z", "2026-09-28T10:00:00.000000001Z") > 0);
  assert.equal(compareTime("2026-09-28T10:00:01Z", "2026-09-28T10:00:01Z"), 0);
  const m = mergeEntries([e("2026-09-28T10:00:00.1Z", "a", "later")], [e("2026-09-28T10:00:00Z", "a", "first")], 10);
  assert.deepEqual(m.entries.map((x) => x.text), ["first", "later"]);
});

test("follow backs off while the agent is busy, and comes back after a good read", () => {
  let d = FOLLOW_EVERY_MS;
  const seen: number[] = [];
  for (let i = 0; i < 4; i++) seen.push((d = nextFollowDelay(d, true)));
  assert.deepEqual(seen, [20000, 40000, FOLLOW_MAX_MS, FOLLOW_MAX_MS]);
  assert.equal(nextFollowDelay(d, false), FOLLOW_EVERY_MS);
  assert.equal(FOLLOW_EVERY_MS, 10000);
});
