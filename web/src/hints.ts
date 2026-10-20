// Diagnosis hints ("What does this mean?"), free of React so node --test can
// check them. The portal matches (portal/hints); this builds the request.
// Hints are text: nothing here runs a command.

export interface Hint {
  slug: string;
  meaning: string;
  /** Read-only kubectl/helm commands with the real names filled in. */
  check: string[];
  causes: string[];
  /** Titles of the study notes, as plain text (the notes are private). */
  notes: string[];
}

export type HintKind = "Pod" | "Node" | "Service" | "Deployment" | "StatefulSet" | "PersistentVolumeClaim";

export interface HintSignals {
  kind: HintKind;
  namespace?: string;
  name: string;
  /** A pod's STATUS column, or the status the chain and app map show ("2/3 ready", "Pending"). */
  status?: string;
  /** A running pod with fewer ready containers than containers. */
  notReady?: boolean;
  /** Something refers to the object, but it does not exist. */
  missing?: boolean;
  /** Ids of health findings about the object. */
  rules?: string[];
  /** From the pod row, to fill the commands. */
  node?: string;
  container?: string;
  initContainer?: string;
}

export function hintsPath(s: HintSignals): string {
  const q = new URLSearchParams({ kind: s.kind, name: s.name });
  if (s.namespace) q.set("namespace", s.namespace);
  if (s.status) q.set("status", s.status);
  if (s.notReady) q.set("notReady", "1");
  if (s.missing) q.set("missing", "1");
  if (s.node) q.set("node", s.node);
  if (s.container) q.set("container", s.container);
  if (s.initContainer) q.set("initContainer", s.initContainer);
  for (const r of s.rules ?? []) q.append("rule", r);
  return "/api/hints?" + q.toString();
}

/** A pod row offers the panel only when it is not fine. */
export function podNeedsHint(p: { status: string; ready: number; total: number }): boolean {
  if (p.status === "Completed" || p.status === "Succeeded") return false;
  if (p.status === "Running") return p.ready !== p.total;
  return true;
}

/** The signals of a pod row; the first container (or init container) that is not ready fills the commands. */
export function podSignals(p: {
  namespace: string;
  name: string;
  status: string;
  ready: number;
  total: number;
  node?: string;
  containers?: { name: string; init?: boolean; ready: boolean }[];
}): HintSignals {
  const cs = p.containers ?? [];
  const pick = (init: boolean) => (cs.find((c) => !!c.init === init && !c.ready) ?? cs.find((c) => !!c.init === init))?.name;
  const s: HintSignals = { kind: "Pod", namespace: p.namespace, name: p.name, status: p.status, notReady: p.status === "Running" && p.ready !== p.total };
  if (p.node) s.node = p.node;
  const c = pick(false);
  if (c) s.container = c;
  const i = pick(true);
  if (i) s.initContainer = i;
  return s;
}

const READY = /^(\d+)\/(\d+) ready$/;

/**
 * The signals of an object in the chain or the app map (kind, namespace, name,
 * graph status). Only an object whose status the catalogue can read gets the
 * panel; undefined otherwise.
 */
export function objectSignals(o: { kind: string; namespace?: string; name: string; status?: string; missing?: boolean }): HintSignals | undefined {
  const kind = o.kind.split(".")[0];
  const status = o.status ?? "";
  const base = { namespace: o.namespace, name: o.name, status };
  const m = status.match(READY);
  const partlyReady = !!m && Number(m[1]) < Number(m[2]);
  switch (kind) {
    case "Pod":
      // Running is fine; "Running, not ready" is not. Finished pods and unknown status offer nothing.
      return status === "" || status === "Running" || status === "Completed" || status === "Succeeded" ? undefined : { kind: "Pod", ...base };
    case "Service":
      return o.missing || status === "no pods match selector" ? { kind: "Service", ...base, missing: !!o.missing } : undefined;
    case "Deployment":
    case "StatefulSet":
      return partlyReady ? { kind, ...base } : undefined;
    case "PersistentVolumeClaim":
      return status === "Pending" ? { kind, ...base } : undefined;
  }
  return undefined;
}

/** The signals of a health finding ("Pod/ns/name" or "Node/name"); undefined for other objects. */
export function findingSignals(f: { object: string; rule: string }): HintSignals | undefined {
  const [kind, a, b] = f.object.split("/");
  if (kind === "Node" && a) return { kind: "Node", name: a, rules: [f.rule] };
  if (kind === "Pod" && a && b) return { kind: "Pod", namespace: a, name: b, rules: [f.rule] };
  return undefined;
}
