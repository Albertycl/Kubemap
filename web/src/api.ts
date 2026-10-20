// Typed client for the portal API (portal/auth, portal/clusters).

import { hintsPath, type Hint, type HintSignals } from "./hints";

export type ClusterStatus = "active" | "disconnected";

export interface Cluster {
  id: string;
  name: string;
  status: ClusterStatus;
  createdAt: string;
  lastSeen?: string;
  agentVersion?: string;
  kubernetesVersion?: string;
  nodeCount: number;
}

export interface Me {
  username: string;
  mustChangePassword: boolean;
  totpEnabled?: boolean;
}

export interface TotpStatus {
  enabled: boolean;
  recoveryCodesLeft: number;
}

export interface TotpSetup {
  secret: string;
  uri: string;
}

/** An error response from the portal: {"error": code, "message": text}. */
export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
  ) {
    super(message);
  }
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    credentials: "same-origin",
    // X-Kubemap-Request: a cross-site page cannot send it, so even a write
    // without a body is known to come from this app (review KM-44 L15).
    headers: body === undefined ? { "X-Kubemap-Request": "1" } : { "Content-Type": "application/json", "X-Kubemap-Request": "1" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (res.status === 204) return undefined as T;
  let data: unknown = null;
  try {
    data = await res.json();
  } catch {
    // Non-JSON body; handled below.
  }
  if (!res.ok) {
    if (res.status === 401 && path !== "/api/login" && path !== "/api/me") {
      // The session ended (logout elsewhere, expiry, password change).
      window.dispatchEvent(new Event("kubemap:unauthorized"));
    }
    const d = (data ?? {}) as { error?: string; message?: string };
    throw new ApiError(res.status, d.error ?? "http_" + res.status, d.message ?? res.statusText);
  }
  return data as T;
}

export const api = {
  me: () => call<Me>("GET", "/api/me"),
  login: (username: string, password: string, code?: string) =>
    call<Me>("POST", "/api/login", code ? { username, password, code } : { username, password }),
  totpStatus: () => call<TotpStatus>("GET", "/api/totp"),
  totpSetup: (password: string) => call<TotpSetup>("POST", "/api/totp/setup", { password }),
  totpEnable: (code: string) => call<{ recoveryCodes: string[] }>("POST", "/api/totp/enable", { code }),
  totpDisable: (password: string, code: string) => call<void>("POST", "/api/totp/disable", { password, code }),
  logout: () => call<void>("POST", "/api/logout"),
  changePassword: (currentPassword: string, newPassword: string) =>
    call<Me>("POST", "/api/password", { currentPassword, newPassword }),

  clusters: () => call<{ clusters: Cluster[] }>("GET", "/api/clusters").then((r) => r.clusters),
  cluster: (id: string) => call<{ cluster: Cluster }>("GET", `/api/clusters/${encodeURIComponent(id)}`).then((r) => r.cluster),
};

/** "3 min ago", "just now", "in 58 min". */
export function relativeTime(iso: string | undefined, now = Date.now()): string {
  if (!iso) return "never";
  const secs = Math.round((new Date(iso).getTime() - now) / 1000);
  const abs = Math.abs(secs);
  if (abs < 10) return "just now";
  const [n, unit] =
    abs < 60 ? [abs, "s"] : abs < 3600 ? [Math.round(abs / 60), "min"] : abs < 86400 ? [Math.round(abs / 3600), "h"] : [Math.round(abs / 86400), "d"];
  return secs < 0 ? `${n} ${unit} ago` : `in ${n} ${unit}`;
}

/** What a cluster needs from the admin, in plain words. */
export function statusText(c: Cluster): string {
  if (c.status === "active") return "Connected";
  return "Disconnected";
}

// ---- Node health (portal/metrics, KM-30) ----

/** Percentages 0-100; psi* are the share of time (0-100) tasks waited. */
export interface HealthPoint {
  cpu: number;
  memory: number;
  swap: number;
  disk: number;
  inodes: number;
  psiCpu: number;
  psiMemory: number;
  psiIo: number;
}

/**
 * KM-65: the node agent's heartbeat crossed with Kubernetes' Ready
 * condition, so "node down" and "node agent down" read differently.
 */
export interface NodeState {
  code: "ok" | "reconnecting" | "unreachable" | "node-down" | "agent-down" | "not-ready" | "no-agent" | "removed";
  severity: "ok" | "warn" | "fail";
  title: string;
  detail: string;
  silentForSeconds?: number;
  kubernetesReady?: boolean;
}

export interface NodeSummary {
  name: string;
  /** The node agent's status; "none" when only Kubernetes knows the node. */
  status: string;
  connected: boolean;
  lastSeen: string;
  state?: NodeState;
  current?: { at: string; point: HealthPoint; hasPsi: boolean; load1: number; cores: number };
}

export interface FilesystemStats {
  mountPoint: string;
  device: string;
  fsType: string;
  totalBytes: number;
  availableBytes: number;
  inodesTotal: number;
  inodesFree: number;
}

export interface ProcessStats {
  pid: number;
  command: string;
  cpuPercent: number;
  rssBytes: number;
  podNamespace?: string;
  podName?: string;
  container?: string;
}

export interface NodeMetrics {
  cpu: { cores: number; usagePercent: number };
  memory: { totalBytes: number; availableBytes: number };
  swap: { totalBytes: number; usedBytes: number };
  load: { load1: number; load5: number; load15: number };
  filesystems?: FilesystemStats[];
  topProcesses?: ProcessStats[];
}

export type Range = "1h" | "6h" | "24h";

export type SeriesPoint = { t: string } & Omit<HealthPoint, "psiCpu" | "psiMemory" | "psiIo"> & {
  psiCpu: number | null;
  psiMemory: number | null;
  psiIo: number | null;
};

export interface NodeDetail {
  node: NodeSummary;
  range: Range;
  from: string;
  to: string;
  step: number; // seconds
  /** PSI values are null in steps where the node's kernel reported no PSI. */
  series: SeriesPoint[];
  latest?: { at: string; point: HealthPoint; hasPsi: boolean; diskMountPoint?: string; metrics: NodeMetrics };
}

export const nodeApi = {
  nodes: (clusterId: string) =>
    call<{ nodes: NodeSummary[] }>("GET", `/api/clusters/${encodeURIComponent(clusterId)}/nodes`).then((r) => r.nodes),
  node: (clusterId: string, node: string, range: Range) =>
    call<NodeDetail>(
      "GET",
      `/api/clusters/${encodeURIComponent(clusterId)}/nodes/${encodeURIComponent(node)}?range=${range}`,
    ),
};

/** 1.5 GiB, 820 MiB, ... */
export function bytes(n: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n >= 10 || i === 0 ? Math.round(n) : n.toFixed(1)} ${units[i]}`;
}

export function percent(n: number): string {
  return `${n >= 10 || Number.isInteger(n) ? Math.round(n) : n.toFixed(1)}%`;
}

// ---- Application map (portal/appmap, KM-53; types from pkg/protocol/appmap.go) ----

export type Tier = "entry" | "frontend" | "backend" | "database" | "cache";

/** The columns of the map, left to right. */
export const TIERS: { id: Tier; label: string }[] = [
  { id: "entry", label: "Entry" },
  { id: "frontend", label: "Frontend" },
  { id: "backend", label: "Backend" },
  { id: "database", label: "Database" },
  { id: "cache", label: "Cache" },
];

export interface MapPod {
  name: string;
  node?: string;
  /** What "kubectl get pods" shows: Running, CrashLoopBackOff (OOMKilled), ... */
  status: string;
  ready: boolean;
  restarts: number;
  primary?: boolean;
}

export interface MapRoute {
  host: string;
  path: string;
  tls: boolean;
  entryPoints?: string[];
  service: string;
  port?: string;
  /** -1 when unknown. */
  readyEndpoints: number;
  problems?: string[];
}

export interface MapComponent {
  id: string;
  kind: string;
  namespace: string;
  name: string;
  app?: string;
  tier: Tier;
  tierReason: string;
  status?: string;
  healthy: boolean;
  missing?: boolean;
  engine?: string;
  version?: string;
  routes?: MapRoute[];
  services?: string[];
  pods?: MapPod[];
}

export interface MapApp {
  id: string;
  name: string;
  namespace: string;
  source: string;
  /** Display text: "Healthy", "Degraded", "Healthy, OutOfSync", ... */
  health: string;
  /** Health for colour. */
  state: "ok" | "warn" | "fail";
  /** Each problem names its object: {id: "Pod/shop/orders-1", status: "CrashLoopBackOff"}. */
  problems?: { id: string; status: string }[];
  system?: boolean;
  components: MapComponent[];
}

export type EvidenceType = "route" | "env" | "configmap" | "file" | "operator";

export interface MapEvidence {
  type: EvidenceType;
  object: string;
  name?: string;
  key?: string;
  port?: string;
  detail: string;
}

export interface MapLink {
  from: string;
  to: string;
  fromApp?: string;
  toApp?: string;
  via?: string[];
  missing?: boolean;
  evidence: MapEvidence[];
  /** Set when traces show this call really happens (KM-54). */
  traces?: { spans: number; lastSeen: string; detail: string };
}

/** What the agent learned from traces for the map (KM-54). */
export interface AppMapTraces {
  source: string;
  window: string;
  spans: number;
  probes?: number;
  confirmed: number;
  unmatched?: string[];
  error?: string;
  at: string;
}

export interface AppMap {
  apps: MapApp[];
  links: MapLink[];
  peers?: MapComponent[];
  unassigned?: MapComponent[];
  incomplete?: string[];
  traces?: AppMapTraces;
  /** When the portal received the map. */
  at: string;
}

export const appMapApi = {
  get: (clusterId: string, opts: { namespace?: string; app?: string } = {}) => {
    const q = new URLSearchParams();
    if (opts.namespace) q.set("namespace", opts.namespace);
    if (opts.app) q.set("app", opts.app);
    const qs = q.toString();
    return call<AppMap>("GET", `/api/clusters/${encodeURIComponent(clusterId)}/appmap${qs ? "?" + qs : ""}`);
  },
};

// ---- Namespaces, pods and pod logs (portal/resources, KM-80) ----

export interface NamespaceInfo {
  name: string;
  phase: string;
  created: string;
  podSecurity?: string;
}

export interface ContainerInfo {
  name: string;
  init?: boolean;
  ready: boolean;
  restarts: number;
  /** running, waiting: <reason>, terminated: <reason> */
  state: string;
  /** A terminated earlier run exists, so "previous" logs can be read. */
  previous?: boolean;
}

export interface PodInfo {
  namespace: string;
  name: string;
  /** What "kubectl get pods" shows: Running, CrashLoopBackOff, Init:0/1 ... */
  status: string;
  phase: string;
  ready: number;
  total: number;
  restarts: number;
  node?: string;
  created: string;
  owner?: string;
  containers: ContainerInfo[];
}

export interface LogQuery {
  container?: string;
  previous?: boolean;
  timestamps?: boolean;
  tailLines?: number;
}

function logsPath(clusterId: string, ns: string, pod: string, q: LogQuery, follow: boolean): string {
  const p = new URLSearchParams();
  if (q.container) p.set("container", q.container);
  if (q.previous) p.set("previous", "true");
  if (q.timestamps) p.set("timestamps", "true");
  if (q.tailLines) p.set("tailLines", String(q.tailLines));
  if (follow) p.set("follow", "true");
  const qs = p.toString();
  return (
    `/api/clusters/${encodeURIComponent(clusterId)}/namespaces/${encodeURIComponent(ns)}` +
    `/pods/${encodeURIComponent(pod)}/logs${qs ? "?" + qs : ""}`
  );
}

/** Turns a non-2xx response into an ApiError, like call() does. */
async function failed(res: Response): Promise<ApiError> {
  let d: { error?: string; message?: string } = {};
  try {
    d = await res.json();
  } catch {
    // not JSON
  }
  if (res.status === 401) window.dispatchEvent(new Event("kubemap:unauthorized"));
  return new ApiError(res.status, d.error ?? "http_" + res.status, d.message ?? res.statusText);
}

export const resourceApi = {
  namespaces: (clusterId: string) =>
    call<{ namespaces: NamespaceInfo[]; truncated: boolean }>(
      "GET",
      `/api/clusters/${encodeURIComponent(clusterId)}/namespaces`,
    ),
  pods: (clusterId: string, namespace?: string) =>
    call<{ pods: PodInfo[]; truncated: boolean }>(
      "GET",
      `/api/clusters/${encodeURIComponent(clusterId)}/pods${namespace ? "?namespace=" + encodeURIComponent(namespace) : ""}`,
    ),
  /** The last lines of a log (not followed). */
  logs: async (clusterId: string, ns: string, pod: string, q: LogQuery) => {
    const res = await fetch(logsPath(clusterId, ns, pod, q, false), { credentials: "same-origin" });
    if (!res.ok) throw await failed(res);
    return { text: await res.text(), truncated: res.headers.get("X-Kubemap-Truncated") === "true" };
  },
  /**
   * Follows a log: the response body is NDJSON (see logs.ts). The caller
   * reads it and aborts the signal to stop.
   */
  follow: async (clusterId: string, ns: string, pod: string, q: LogQuery, signal: AbortSignal) => {
    const res = await fetch(logsPath(clusterId, ns, pod, q, true), { credentials: "same-origin", signal });
    if (!res.ok) throw await failed(res);
    if (!res.body) throw new ApiError(0, "no_body", "The browser cannot read a streamed response.");
    return res.body.getReader();
  },
};

/** "3 min", "2 h", "5 d": the age of an object. */
export function age(iso: string, now = Date.now()): string {
  const s = Math.max(0, Math.round((now - new Date(iso).getTime()) / 1000));
  if (s < 60) return `${s} s`;
  if (s < 3600) return `${Math.round(s / 60)} min`;
  if (s < 86400) return `${Math.round(s / 3600)} h`;
  return `${Math.round(s / 86400)} d`;
}

// ---- Problem finder (portal/health, KM-61/KM-62; types from pkg/protocol/health.go) ----

export type Severity = "critical" | "warning" | "info";

export interface HealthFinding {
  rule: string;
  severity: Severity;
  /** "Node/k8server", "Pod/ns/name", "Namespace/ns" or "Cluster". */
  object: string;
  title: string;
  evidence: string;
  meaning: string;
  why: string;
  fix: string;
  /** The command that proves the fix, for a person to type. */
  verify: string;
  items?: string[];
  count?: number;
}

export interface HealthReport {
  time: string;
  findings: HealthFinding[];
  rules: string[];
  thresholds: {
    evictionHard: Record<string, string>;
    evictionMinimumReclaim?: Record<string, string>;
    imageGCHighPercent: number;
    source: string;
  };
  notes?: string[];
}

export interface ClusterHealth {
  id: string;
  name: string;
  status: ClusterStatus;
  report?: HealthReport;
  error?: { error: string; message: string };
}

export const healthApi = {
  all: () => call<{ clusters: ClusterHealth[]; at: string }>("GET", "/api/health"),
  cluster: (id: string) =>
    call<{ report: HealthReport; at: string }>("GET", `/api/clusters/${encodeURIComponent(id)}/health`),
};

// ---- Diagnosis hints (portal/hints; types and path in hints.ts) ----

export const hintsApi = {
  get: (s: HintSignals) => call<{ hints: Hint[] }>("GET", hintsPath(s)),
};

// ---- Guided debugging (portal/resources, pod.debug, KM-76; types from pkg/protocol/debug.go) ----

export type Verdict = "ok" | "problem" | "unknown";

export interface DebugStep {
  /** status, events, logs, exit-code, resources, node, service, netpol */
  key: string;
  title: string;
  verdict: Verdict;
  finding: string;
  evidence?: string[];
  /** What a person types to check this step by hand. */
  commands: string[];
}

export interface PodDebug {
  namespace: string;
  name: string;
  owner?: string;
  status: string;
  /** The most specific problem found; empty when every step is ok. */
  cause?: string;
  causeStep?: string;
  steps: DebugStep[];
  notes?: string[];
}

export const debugApi = {
  pod: (clusterId: string, ns: string, pod: string) =>
    call<{ debug: PodDebug; at: string }>(
      "GET",
      `/api/clusters/${encodeURIComponent(clusterId)}/namespaces/${encodeURIComponent(ns)}/pods/${encodeURIComponent(pod)}/debug`,
    ),
};

export function debugHref(clusterId: string, ns: string, pod: string): string {
  return `#/clusters/${encodeURIComponent(clusterId)}/namespaces/${encodeURIComponent(ns)}/pods/${encodeURIComponent(pod)}/debug`;
}

export function debugAppHref(clusterId: string, appId: string): string {
  return `#/clusters/${encodeURIComponent(clusterId)}/debug/app/${encodeURIComponent(appId)}`;
}

// ---- NetworkPolicy checker (portal/netpol, KM-36; types from pkg/protocol/netpol.go) ----

export interface ServiceInfo {
  namespace: string;
  name: string;
  type: string;
  clusterIP?: string;
  selector?: Record<string, string>;
  ports: { name?: string; port: number; protocol: string; targetPort?: string }[];
}

export interface NetpolRule {
  policy: string;
  rule: number;
  detail: string;
}

export interface NetpolSide {
  direction: "egress" | "ingress";
  isolated: boolean;
  allowed: boolean;
  decision: string;
  selecting?: string[];
  allowedBy?: NetpolRule[];
}

export interface NetpolPath {
  to: string;
  port: number;
  allowed: boolean;
  egress: NetpolSide;
  ingress: NetpolSide;
}

export interface NetpolResult {
  allowed: boolean;
  summary: string;
  from: string;
  port: number;
  protocol: string;
  paths: NetpolPath[];
  notes?: string[];
}

export interface NetpolMatrix {
  namespace: string;
  sources: { label: string; pod: string }[];
  targets: { service: string; port: number; protocol: string; portName?: string }[];
  cells: { allowed: boolean; noDestination?: boolean; summary?: string; error?: string }[][];
  truncated: boolean;
  at: string;
}

export const netpolApi = {
  check: (clusterId: string, params: URLSearchParams) =>
    call<{ result: NetpolResult; at: string }>(
      "GET",
      `/api/clusters/${encodeURIComponent(clusterId)}/netpol/check?${params.toString()}`,
    ),
  matrix: (clusterId: string, namespace: string) =>
    call<NetpolMatrix>(
      "GET",
      `/api/clusters/${encodeURIComponent(clusterId)}/netpol/matrix?namespace=${encodeURIComponent(namespace)}`,
    ),
  services: (clusterId: string, namespace?: string) =>
    call<{ services: ServiceInfo[]; truncated: boolean }>(
      "GET",
      `/api/clusters/${encodeURIComponent(clusterId)}/services${namespace ? "?namespace=" + encodeURIComponent(namespace) : ""}`,
    ),
};

// ---- All-servers overview (portal/metrics GET /api/servers, KM-66; node.pressure from pkg/protocol/pressure.go) ----

export type PressureVerdict = "ok" | "elevated" | "pressure" | "unknown";

export interface ResourcePressure {
  resource: "cpu" | "memory" | "io" | "disk" | "pids" | string;
  condition?: string;
  conditionStatus?: string;
  conditionSince?: string;
  verdict: PressureVerdict;
  detail: string;
}

export interface NodePressure {
  node: string;
  ready: boolean;
  psiAvailable: boolean;
  verdict: PressureVerdict;
  summary: string;
  resources: ResourcePressure[];
}

export interface ServerInfo {
  clusterId: string;
  clusterName: string;
  clusterStatus: ClusterStatus;
  name: string;
  /** The node agent's status; "none" when no node agent reports for this node. */
  status: string;
  connected: boolean;
  lastSeen?: string;
  current?: { at: string; point: HealthPoint; hasPsi: boolean; load1: number; cores: number };
  /** The numbers are the last known, not current. */
  stale?: boolean;
  pressure?: NodePressure;
  state?: NodeState;
}

export const serverApi = {
  all: () =>
    call<{ servers: ServerInfo[]; pressureErrors?: Record<string, { error: string; message: string }>; at: string }>(
      "GET",
      "/api/servers",
    ),
};

// RBAC and Argo CD views (portal/access, KM-37; agent op rbac.whocan, KM-34).

export type SubjectKind = "ServiceAccount" | "User" | "Group";

export interface RBACSubject {
  kind: SubjectKind;
  name: string;
  namespace?: string;
  groups?: string[];
}

export interface RBACRule {
  verbs: string[];
  apiGroups?: string[];
  resources?: string[];
  resourceNames?: string[];
  nonResourceURLs?: string[];
}

export interface RBACGrant {
  binding: string;
  role: string;
  scope: string;
  via: string;
  roleMissing?: boolean;
  rules?: RBACRule[];
  risky?: string[];
}

export interface RBACWho {
  kind: SubjectKind;
  name: string;
  namespace?: string;
  everyone?: boolean;
  binding: string;
  role: string;
  scope: string;
  resourceNames?: string[];
}

export interface RBACResult {
  mode: "chain" | "check" | "whocan";
  subject?: RBACSubject;
  groups?: string[];
  allowed?: boolean;
  grants?: RBACGrant[];
  subjects?: RBACWho[];
  risky?: string[];
  notes?: string[];
}

export interface RBACQueryForm {
  subjectKind?: SubjectKind | "";
  subject?: string;
  subjectNamespace?: string;
  groups?: string;
  verb?: string;
  resource?: string;
  apiGroup?: string;
  namespace?: string;
  resourceName?: string;
}

export interface RBACAction {
  key: string;
  label: string;
  verb: string;
  resource: string;
  apiGroup?: string;
  why: string;
}

export interface NamespaceGrant {
  binding: string;
  role: string;
  scope: string;
  resourceNames?: string[];
}

export interface NamespaceView {
  namespace: string;
  actions: RBACAction[];
  subjects: { kind: SubjectKind; name: string; namespace?: string; everyone?: boolean; grants: Record<string, NamespaceGrant> }[];
  errors?: Record<string, string>;
  accounts: { name: string; risky: string[]; everyone?: string[]; platform?: string[]; grants: number; error?: string }[];
  /** Risky rights every account in the cluster has (not counting known platform grants). */
  everyoneRisky?: string[];
  notes?: string[];
}

export interface ArgoResource {
  group?: string;
  kind: string;
  namespace?: string;
  name: string;
  status?: string;
  health?: string;
  healthMessage?: string;
  hook?: boolean;
  requiresPruning?: boolean;
}

export interface ArgoApp {
  name: string;
  namespace: string;
  mapId: string;
  project: string;
  sources: { repoURL: string; path?: string; chart?: string; targetRevision?: string }[];
  destServer?: string;
  destNamespace?: string;
  autoSync: boolean;
  prune: boolean;
  selfHeal: boolean;
  sync: string;
  revision?: string;
  health: string;
  healthMessage?: string;
  operationPhase?: string;
  operationMessage?: string;
  operationFinished?: string;
  /** KM-41: when it started, who asked ("kubemap:<user>" from Kubemap), which commit it synced. */
  operationStarted?: string;
  operationBy?: string;
  operationRevision?: string;
  /** An operation is running; a new sync is refused until it finishes. */
  operationRunning: boolean;
  reconciledAt?: string;
  conditions?: { type: string; message: string }[];
  resources: ArgoResource[];
}

const cpath = (id: string) => `/api/clusters/${encodeURIComponent(id)}`;

export const accessApi = {
  query: (clusterId: string, form: RBACQueryForm) => {
    const q = new URLSearchParams();
    for (const [k, v] of Object.entries(form)) if (v) q.set(k, String(v).trim());
    return call<{ result: RBACResult; at: string }>("GET", `${cpath(clusterId)}/rbac?${q.toString()}`);
  },
  accounts: (clusterId: string) =>
    call<{ accounts: { namespace: string; name: string }[]; truncated: boolean }>("GET", `${cpath(clusterId)}/rbac/accounts`),
  namespace: (clusterId: string, ns: string) =>
    call<{ view: NamespaceView; at: string }>("GET", `${cpath(clusterId)}/rbac/namespaces/${encodeURIComponent(ns)}`),
  argocd: (clusterId: string) =>
    call<{ installed: boolean; apps: ArgoApp[]; truncated?: boolean; unreadable?: number; at: string }>("GET", `${cpath(clusterId)}/argocd`),
};

// ---- Scenarios (portal/scenarios, KM-82; types from scenarios/scenarios.go) ----

export interface ScenarioCheck {
  op: string;
  args?: Record<string, string>;
}

export interface ScenarioStep {
  title: string;
  why: string;
  /** May contain placeholders like <ns>. */
  command: string;
  /** app-map | pod | node | netpol | logs | findings | debug */
  kubemap?: string;
  check?: ScenarioCheck;
  good?: string;
  bad?: string;
}

export interface Scenario {
  id: string;
  area: string;
  title: string;
  question: string;
  symptoms?: string[];
  steps: ScenarioStep[];
  causes?: { cause: string; fix: string; prove: string }[];
  prevention?: string[];
  senior?: string[];
  lab?: string;
  /** Placeholders in order of first use ("ns", "svc"). */
  params: string[];
}

export interface ScenarioSummary {
  id: string;
  area: string;
  title: string;
  question: string;
  symptoms?: string[];
  steps: number;
  checks: number;
  params: string[];
  lab?: string;
}

export type CheckVerdict = "good" | "bad" | "warn" | "unknown" | "needs-input";

export interface CheckResult {
  verdict: CheckVerdict;
  summary: string;
  details?: string[];
  missing?: string[];
  op: string;
  at: string;
}

export const scenarioApi = {
  list: () =>
    call<{ scenarios: ScenarioSummary[]; areas: string[]; placeholders: Record<string, string> }>("GET", "/api/scenarios"),
  get: (id: string) =>
    call<{ scenario: Scenario; placeholders: Record<string, string> }>("GET", `/api/scenarios/${encodeURIComponent(id)}`),
  check: (clusterId: string, id: string, step: number, values: Record<string, string>) => {
    const q = new URLSearchParams();
    for (const [k, v] of Object.entries(values)) if (v) q.set(k, v);
    return call<CheckResult>(
      "GET",
      `/api/clusters/${encodeURIComponent(clusterId)}/scenarios/${encodeURIComponent(id)}/steps/${step}/check?${q.toString()}`,
    );
  },
};

// Incident journal (portal/journal, KM-89).

export type JournalKind = "incident" | "changes";
export type SectionKind =
  | "intro"
  | "summary"
  | "detection"
  | "rootcause"
  | "actions"
  | "result"
  | "lessons"
  | "text"
  | "timeline"
  | "decisions"
  | "followups"
  | "star"
  | "changes";

export interface TimelineRow {
  time: string;
  event: string;
}
export interface Decision {
  decision: string;
  why: string;
  tradeOff: string;
}
export type FollowUpStatus = "todo" | "doing" | "done";
export interface FollowUp {
  status: FollowUpStatus;
  owner: string;
  text: string;
}
export interface Star {
  situation: string;
  task: string;
  action: string;
  result: string;
}
export interface ChangeRow {
  time: string;
  change: string;
  why: string;
  checked: string;
  undo: string;
}

export interface JournalSection {
  kind: SectionKind;
  /** Empty only for the text (or change table) under the title. */
  heading: string;
  text?: string;
  timeline?: TimelineRow[];
  decisions?: Decision[];
  followUps?: FollowUp[];
  star?: Star;
  changes?: ChangeRow[];
}

export interface JournalRecord {
  kind: JournalKind;
  title: string;
  /** YYYY-MM-DD or empty. */
  date: string;
  cluster: string;
  severity: string;
  sections: JournalSection[];
}

export interface JournalEntry extends JournalRecord {
  id: string;
  source: string;
  version: number;
  createdAt: string;
  createdBy: string;
  updatedAt: string;
  updatedBy: string;
}

export interface JournalSummary {
  id: string;
  kind: JournalKind;
  title: string;
  date: string;
  cluster: string;
  severity: string;
  source: string;
  updatedAt: string;
  updatedBy: string;
}

export type EvidenceKind = "findings" | "events" | "metrics" | "audit";

export interface JournalEvidence {
  id: string;
  kind: EvidenceKind;
  clusterId: string;
  summary: string;
  /** The Kubemap page it came from (a #/ route). */
  link: string;
  note: string;
  data: unknown;
  capturedAt: string;
  capturedBy: string;
}

export interface AuditEntry {
  time: string;
  user: string;
  clusterId: string;
  action: string;
  target: string;
  result: string;
}

const jpath = (id: string) => `/api/journal/${encodeURIComponent(id)}`;

export const journalApi = {
  list: () => call<{ entries: JournalSummary[] }>("GET", "/api/journal").then((r) => r.entries),
  get: (id: string) => call<{ entry: JournalEntry; evidence: JournalEvidence[] }>("GET", jpath(id)),
  create: (r: JournalRecord) => call<{ entry: JournalEntry }>("POST", "/api/journal", r).then((x) => x.entry),
  update: (id: string, version: number, r: JournalRecord) =>
    call<{ entry: JournalEntry }>("PUT", jpath(id), { ...r, version }).then((x) => x.entry),
  remove: (id: string) => call<void>("DELETE", jpath(id)),
  importMarkdown: (markdown: string, source: string, replace = false) =>
    call<{ entry: JournalEntry; exact: boolean }>("POST", "/api/journal/import", { markdown, source, replace }),
  /** The record as Markdown, and the file name the portal gives it. */
  exportMarkdown: async (id: string, evidence = false): Promise<{ text: string; filename: string }> => {
    const res = await fetch(`${jpath(id)}/markdown${evidence ? "?evidence=1" : ""}`, { credentials: "same-origin" });
    if (!res.ok) {
      if (res.status === 401) window.dispatchEvent(new Event("kubemap:unauthorized"));
      const d = (await res.json().catch(() => ({}))) as { error?: string; message?: string };
      throw new ApiError(res.status, d.error ?? "http_" + res.status, d.message ?? res.statusText);
    }
    const m = /filename="([^"]+)"/.exec(res.headers.get("Content-Disposition") ?? "");
    return { text: await res.text(), filename: m ? m[1] : "journal.md" };
  },
  addEvidence: (id: string, body: { kind: EvidenceKind; clusterId?: string; namespace?: string; node?: string; note?: string }) =>
    call<{ evidence: JournalEvidence }>("POST", `${jpath(id)}/evidence`, body).then((x) => x.evidence),
  removeEvidence: (id: string, eid: string) => call<void>("DELETE", `${jpath(id)}/evidence/${encodeURIComponent(eid)}`),
  audit: (limit = 200) => call<{ entries: AuditEntry[] }>("GET", `/api/audit?limit=${limit}`).then((r) => r.entries),
};

// Draft incidents (portal/incidents, KM-90), shown and written up in the journal.

export interface IncidentDraft {
  id: string;
  clusterId: string;
  clusterName: string;
  status: "open" | "resolved";
  title: string;
  severity: string;
  started: string;
  updated: string;
  resolved?: string;
  detection: string;
  presentAtStart?: boolean;
  timeline: { time: string; event: string; source: string; rule?: string; object?: string }[];
}

export const draftApi = {
  list: () => call<{ drafts: IncidentDraft[] | null }>("GET", "/api/incident-drafts").then((r) => r.drafts ?? []),
  /** Makes an incident from the draft, with times in the browser's time zone. */
  writeUp: (id: string) =>
    call<{ entry: JournalEntry }>("POST", `/api/journal-drafts/${encodeURIComponent(id)}/write-up`, {
      timeZone: Intl.DateTimeFormat().resolvedOptions().timeZone ?? "",
    }).then((r) => r.entry),
};

// YAML view and edit (KM-39). Kubemap shows the diff and the kubectl
// commands; it never applies an edit itself.

export interface YamlView {
  yaml: string;
  apiVersion: string;
  kind: string;
  resourceVersion: string;
  /** Paths whose values Kubemap replaced with "<redacted by Kubemap>". */
  redacted: string[];
  argoApp?: string;
}

export interface YamlDiffLine {
  op: " " | "-" | "+";
  text: string;
}

export interface YamlDiff {
  changed: boolean;
  diff: YamlDiffLine[];
  patchType?: "strategic" | "merge";
  patch?: string;
  commands: { title: string; command: string }[];
  notes: string[];
}

export interface YamlTargetParams {
  group: string;
  version: string;
  resource: string;
  namespace: string;
  name: string;
}

export const yamlApi = {
  get: (clusterId: string, t: YamlTargetParams) =>
    call<YamlView>("GET", `/api/clusters/${encodeURIComponent(clusterId)}/yaml?${new URLSearchParams({ ...t }).toString()}`),
  names: (clusterId: string, t: Omit<YamlTargetParams, "name">) =>
    call<{ names: string[]; truncated: boolean }>("GET", `/api/clusters/${encodeURIComponent(clusterId)}/yaml/names?${new URLSearchParams({ ...t }).toString()}`),
  diff: (clusterId: string, t: YamlTargetParams, edited: string) =>
    call<YamlDiff>("POST", `/api/clusters/${encodeURIComponent(clusterId)}/yaml/diff`, { ...t, edited }),
};

// ---- Log search across an app's pods (portal/resources, logs.search, KM-57; types from pkg/protocol/logsearch.go) ----

export type LogLevel = "error" | "warn" | "info" | "debug" | "unknown";
export const LOG_LEVELS: LogLevel[] = ["error", "warn", "info", "debug", "unknown"];

export interface LogEntry {
  time: string;
  namespace: string;
  pod: string;
  container: string;
  previous?: boolean;
  level: LogLevel;
  text: string;
  /** Lines that continue this one: a stack trace or an indented message. */
  stack?: string[];
}

export interface LogSource {
  namespace: string;
  pod: string;
  container: string;
  previous?: boolean;
  lines: number;
  matched: number;
  truncated?: boolean;
  error?: string;
}

export interface LogSearchResult {
  app: string;
  since: string;
  until: string;
  entries: LogEntry[];
  /** Matches of the text in the window, by level, before the level filter. */
  levels: Record<LogLevel, number>;
  matched: number;
  truncated?: boolean;
  sources: LogSource[];
  notes?: string[];
}

export interface LogSearchQuery {
  app: string;
  component?: string;
  q?: string;
  levels?: LogLevel[];
  /** A duration back from now ("1h"). */
  since?: string;
  /** Read from this time instead (RFC 3339): "follow" asks for what is newer than the last line shown. */
  after?: string;
  previous?: boolean;
  max?: number;
}

export function logSearchPath(clusterId: string, s: LogSearchQuery): string {
  const p = new URLSearchParams({ app: s.app });
  if (s.component) p.set("component", s.component);
  if (s.q) p.set("q", s.q);
  if (s.levels && s.levels.length) p.set("levels", s.levels.join(","));
  if (s.after) p.set("since", s.after);
  else if (s.since) p.set("since", s.since);
  if (s.previous) p.set("previous", "1");
  if (s.max) p.set("max", String(s.max));
  return `/api/clusters/${encodeURIComponent(clusterId)}/log-search?${p.toString()}`;
}

export const logSearchApi = {
  search: (clusterId: string, s: LogSearchQuery) =>
    call<{ search: LogSearchResult; at: string }>("GET", logSearchPath(clusterId, s)),
};

export function logSearchHref(clusterId: string, appId: string): string {
  return `#/clusters/${encodeURIComponent(clusterId)}/log-search/${encodeURIComponent(appId)}`;
}

/** The app's logs in one view (KM-55): every level of every pod, newest hour. */
export function appLogsHref(clusterId: string, appId: string): string {
  return `${logSearchHref(clusterId, appId)}?levels=&since=1h`;
}

// Alerts (portal/alerts, KM-31): clusters with a critical finding now.

export interface Alert {
  clusterId: string;
  clusterName: string;
  since: string;
  draftId: string;
  findings: { rule: string; object: string; title: string }[];
  /** Changes when the set of critical findings changes. */
  key: string;
}

export const alertApi = {
  list: () => call<{ alerts: Alert[] }>("GET", "/api/alerts").then((r) => r.alerts),
};

// ---- Object chain (portal/appmap, object.chain, KM-35; types from pkg/protocol/chain.go) ----

export interface ChainItem {
  id: string;
  kind: string;
  namespace?: string;
  name: string;
  status?: string;
  missing?: boolean;
  detail?: string;
}

export interface ChainSection {
  key: string;
  items: ChainItem[];
  more?: number;
}

export interface ObjectChain {
  object?: ChainItem;
  sections?: ChainSection[];
  apps?: string[];
  matches?: ChainItem[];
  notes?: string[];
}

export const chainApi = {
  get: (clusterId: string, id: string) =>
    call<{ chain: ObjectChain; at: string }>("GET", `/api/clusters/${encodeURIComponent(clusterId)}/chain?id=${encodeURIComponent(id)}`),
  find: (clusterId: string, q: string) =>
    call<{ chain: ObjectChain; at: string }>("GET", `/api/clusters/${encodeURIComponent(clusterId)}/chain?q=${encodeURIComponent(q)}`),
};

// ---- Metrics and traces (portal/observe: Prometheus and Tempo, read-only) ----

export interface ObserveConfig {
  metrics: boolean;
  traces: boolean;
  grafanaUrl: string;
}
export interface ObserveSeries {
  name: string;
  values: (number | null)[];
}
export interface ObserveSlo {
  target: number;
  windowDays: number;
  requests: number | null;
  errors: number | null;
  burn1h: number | null;
  burn6h: number | null;
  remaining: number | null;
  exhaustHours: number | null;
}
export interface WorkloadMetrics {
  from: number;
  to: number;
  stepMs: number;
  times: number[];
  cpu: ObserveSeries[] | null;
  memory: ObserveSeries[] | null;
  restarts: ObserveSeries[] | null;
  requests: ObserveSeries[] | null;
  errors: ObserveSeries[] | null;
  latency: ObserveSeries[] | null;
  dbRequests: ObserveSeries[] | null;
  dbErrors: ObserveSeries[] | null;
  dbLatency: ObserveSeries[] | null;
  dbNames: string[] | null;
  slo: ObserveSlo | null;
  service: string;
  notes: string[];
}
export interface ObserveTrace {
  id: string;
  rootName: string;
  rootService: string;
  startMs: number;
  durationMs: number;
  error: boolean;
}

const rangeQuery = (r?: { from: number; to: number }) => (r ? `&from=${r.from}&to=${r.to}` : "");

export const observeApi = {
  trace: (id: string) => call<{ spans: import("./observe").TraceSpan[] }>("GET", `/api/observe/trace?id=${encodeURIComponent(id)}`).then((r) => r.spans),
  config: () => call<ObserveConfig>("GET", "/api/observe/config"),
  workload: (namespace: string, name: string, range?: { from: number; to: number }, slo?: number) =>
    call<WorkloadMetrics>("GET", `/api/observe/workload?namespace=${encodeURIComponent(namespace)}&name=${encodeURIComponent(name)}${rangeQuery(range)}${slo ? `&slo=${slo}` : ""}`),
  traces: (namespace: string, name: string, range?: { from: number; to: number }) =>
    call<{ traces: ObserveTrace[]; service: string }>("GET", `/api/observe/traces?namespace=${encodeURIComponent(namespace)}&name=${encodeURIComponent(name)}${rangeQuery(range)}`),
};
