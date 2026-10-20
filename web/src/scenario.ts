// Helpers for the scenario pages (KM-82), free of React and the DOM so
// node --test can check them.

// The kinds of placeholders (as in scenarios/scenarios.go). A filled-in
// command is shown in a Copy box for a person to paste into a shell with
// cluster-admin rights, so only a plain name or number of the right kind is
// ever filled in: never ";", "|", "$(", quotes, spaces or newlines, even
// when a crafted link or a paste puts them in a field (review KM-82 B1).
const NAMES = ["ns", "svc", "pod", "container", "node", "from", "fromns", "deploy", "pvc", "hpa", "cert", "argoapp", "host"];
const DNS = /^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$/;
const APP = /^[a-z0-9]([-a-z0-9.]*[a-z0-9])?\/[a-z0-9]([-a-z0-9.]*[a-z0-9])?$/;

/** Why a value cannot be used, or null when it is fine. */
export function invalidValue(name: string, v: string): string | null {
  if (name === "port") {
    const n = Number(v);
    return /^[1-9][0-9]{0,4}$/.test(v) && n <= 65535 ? null : "not a port (1 to 65535)";
  }
  if (name === "app") return v.length <= 253 && APP.test(v) ? null : "not an app ID (namespace/name)";
  if (!NAMES.includes(name)) return "not a known placeholder";
  return v.length <= 253 && DNS.test(v) ? null : "not a valid name (lowercase letters, digits, '-' and '.')";
}

/** The values that are safe to use: empty and invalid ones are left out. */
export function usable(values: Record<string, string>): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(values)) if (v && invalidValue(k, v) === null) out[k] = v;
  return out;
}

/**
 * Replaces <name> with its value. A placeholder without a value, with an
 * invalid one, or that is not a known placeholder stays as it is.
 */
export function fill(command: string, values: Record<string, string>): string {
  const ok = usable(values);
  // Own properties only: "constructor" or "toString" must stay placeholders (review km-82-merge N-1).
  return command.replace(/<([a-z]+)>/g, (m, name: string) => (Object.hasOwn(ok, name) ? ok[name] : m));
}

/**
 * The Kubemap page a step points to, for a cluster and the values known so
 * far. undefined when the page needs something that is not known yet.
 */
export function pageHref(page: string | undefined, clusterId: string, v: Record<string, string>): string | undefined {
  if (!page || !clusterId) return undefined;
  const c = `#/clusters/${encodeURIComponent(clusterId)}`;
  const e = encodeURIComponent;
  switch (page) {
    case "app-map":
      return v.app ? `${c}/apps/${e(v.app)}` : `${c}/apps`;
    case "pod":
      return v.ns ? `${c}/pods/${e(v.ns)}` : `${c}/pods`;
    case "logs":
      return v.ns && v.pod ? `${c}/namespaces/${e(v.ns)}/pods/${e(v.pod)}/logs` : v.ns ? `${c}/pods/${e(v.ns)}` : `${c}/pods`;
    case "debug":
      return v.ns && v.pod ? `${c}/namespaces/${e(v.ns)}/pods/${e(v.pod)}/debug` : `${c}/debug`;
    case "netpol":
      return `${c}/netpol`;
    case "node":
      return v.node ? `${c}/nodes/${e(v.node)}` : "#/servers";
    case "findings":
      return `${c}/problems`;
  }
  return undefined;
}

/** Reads "a=1&b=2" (the part of the hash after "?"). */
export function parseQuery(q: string | undefined): Record<string, string> {
  const out: Record<string, string> = {};
  if (!q) return out;
  for (const [k, v] of new URLSearchParams(q)) out[k] = v;
  return out;
}

/** The link that starts a scenario from a real object. */
export function scenarioHref(id: string, clusterId: string, values: Record<string, string>): string {
  const q = new URLSearchParams({ cluster: clusterId });
  for (const [k, v] of Object.entries(values)) if (v) q.set(k, v);
  return `#/scenarios/${encodeURIComponent(id)}?${q.toString()}`;
}
