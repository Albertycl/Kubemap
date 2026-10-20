import { useCallback, useId, useLayoutEffect, useMemo, useRef, useState, type RefObject } from "react";
import {
  ApiError,
  appMapApi,
  relativeTime,
  TIERS,
  type AppMap,
  type MapApp,
  type MapComponent,
  type MapLink,
  type MapPod,
} from "./api";
import { traceSummary } from "./traces";
import { errorMessage, usePoll } from "./common";
import { logsHref } from "./PodsPage";
import { appLogsHref, debugAppHref, debugHref } from "./api";
import { chainHref } from "./chain";
import { HintPanel } from "./HintPanel";
import { ObservePanel } from "./ObservePanel";
import { isWorkloadKind } from "./observe";
import { objectSignals } from "./hints";
import { mapPodLevel } from "./debug";
import { scenarioHref } from "./scenario";
import { edgePath, lanePadding, pathData, routeLabel, skipsColumns, type Box, type Edge } from "./arrows";

type Level = "ok" | "warn" | "fail";

/** Restarts (all pods of a component) from which a box shows a badge. */
const RESTART_BADGE = 5;

const LEVEL_TEXT: Record<Level, string> = {
  ok: "Healthy",
  warn: "Needs attention",
  fail: "Failing",
};

/** An app's colour, from the agent's state (older agents: read the health text). */
export function appLevel(a: MapApp): Level {
  if (a.state === "ok" || a.state === "warn" || a.state === "fail") return a.state;
  const h = a.health.toLowerCase();
  if (h.includes("degraded")) return "fail";
  if (/missing|outofsync|progressing|unknown|suspended/.test(h)) return "warn";
  return (a.problems?.length ?? 0) > 0 ? "warn" : "ok";
}

/** A pod: ready is fine; starting or not ready yet is a warning; anything else (CrashLoopBackOff, Error, ...) fails. */
export const podLevel: (p: MapPod) => Level = mapPodLevel;

export function componentLevel(c: MapComponent): Level {
  if (c.missing) return "fail";
  const pods = c.pods ?? [];
  if (c.healthy) return pods.some((p) => podLevel(p) !== "ok") ? "warn" : "ok";
  if (pods.length === 0 || pods.some((p) => podLevel(p) === "fail") || !pods.some((p) => p.ready)) return "fail";
  return "warn";
}

/** "Cluster.postgresql.cnpg.io" → "Cluster"; data stores add their engine: "Cluster · postgres 16". */
function kindLabel(c: MapComponent): string {
  const kind = c.kind.split(".")[0];
  if (!c.engine) return kind;
  return `${kind} · ${c.engine}${c.version ? " " + c.version : ""}`;
}

/** "Service/ns/name" → "name" (or "ns/name" when it is in another namespace). */
function shortId(id: string, ns?: string): string {
  const [, n, name] = id.split("/");
  if (name === undefined) return id;
  return n && n !== ns ? `${n}/${name}` : name;
}

/** "Pod/ns/name" → "Pod". */
function kindOf(id: string): string {
  return id.split("/")[0].split(".")[0];
}

/** The box a problem belongs to: the component itself, or the one that runs the pod. */
function problemTarget(app: MapApp, id: string): string | undefined {
  for (const c of app.components) {
    if (c.id === id || c.pods?.some((p) => `Pod/${c.namespace}/${p.name}` === id)) return c.id;
  }
  return undefined;
}

function routeText(r: { host: string; path: string }): string {
  return `${r.host || "*"}${r.path || "/"}`;
}

const CLUSTER_WIDE = "cluster-wide";

type Zone = "mine" | "other" | "system";
const ZONES: { id: Zone; title: string; hint: string }[] = [
  { id: "mine", title: "My applications", hint: "Drag your own apps here." },
  { id: "other", title: "Other applications", hint: "Nothing here." },
  { id: "system", title: "System", hint: "No system apps." },
];
const zoneKey = (clusterId: string) => `kubemap.appzones.${clusterId}`;
const loadZones = (clusterId: string): Record<string, Zone> => {
  try {
    const v = JSON.parse(localStorage.getItem(zoneKey(clusterId)) ?? "{}");
    const out: Record<string, Zone> = {};
    if (v && typeof v === "object" && !Array.isArray(v))
      for (const [k, z] of Object.entries(v)) if (z === "mine" || z === "other" || z === "system") out[k] = z;
    return out;
  } catch {
    return {};
  }
};

export function AppMapPage({ clusterId, focusApp }: { clusterId: string; focusApp?: string }) {
  const load = useCallback(() => appMapApi.get(clusterId, { app: focusApp }), [clusterId, focusApp]);
  const { data, error } = usePoll<AppMap>(load, 15000, focusApp ?? "");
  const [saved, setSaved] = useState(() => ({ clusterId, map: loadZones(clusterId) }));
  const [systemOpen, setSystemOpen] = useState(false);
  const [dragId, setDragId] = useState<string | null>(null);
  const [overZone, setOverZone] = useState<Zone | null>(null);
  const zones = saved.clusterId === clusterId ? saved.map : loadZones(clusterId);
  const [namespace, setNamespace] = useState("");
  const [search, setSearch] = useState("");
  const [selected, setSelected] = useState<string | null>(null);
  const [openId, setOpenId] = useState<string | null>(null);
  const [allOpen, setAllOpen] = useState(false);

  const moveTo = (id: string, z: Zone) => {
    const app = data?.apps.find((x) => x.id === id);
    const next = { ...zones };
    if (!app || z === (app.system ? "system" : "other")) delete next[id];
    else next[id] = z;
    setSaved({ clusterId, map: next });
    try {
      localStorage.setItem(zoneKey(clusterId), JSON.stringify(next));
    } catch {
      /* storage blocked: the move still works until reload */
    }
  };

  const back = `#/clusters/${encodeURIComponent(clusterId)}`;
  const mapHref = `${back}/apps`;

  const index = useMemo(() => {
    const comps = new Map<string, MapComponent>();
    const apps = new Map<string, MapApp>();
    for (const a of data?.apps ?? []) {
      apps.set(a.id, a);
      for (const c of a.components) comps.set(c.id, c);
    }
    for (const c of [...(data?.peers ?? []), ...(data?.unassigned ?? [])]) if (!comps.has(c.id)) comps.set(c.id, c);
    return { comps, apps };
  }, [data]);

  if (!data) {
    return (
      <section>
        <p>
          <a href={back}>← Cluster</a>
        </p>
        <h1>Application map</h1>
        {error ? <MapError error={error} /> : <p className="muted">Loading…</p>}
      </section>
    );
  }

  const q = search.trim().toLowerCase();
  const matches = (a: MapApp) =>
    !q || a.name.toLowerCase().includes(q) || a.components.some((c) => c.name.toLowerCase().includes(q));
  const zoneOf = (a: MapApp): Zone => zones[a.id] ?? (a.system ? "system" : "other");
  const visibleBase = data.apps;
  const nsOf = (a: MapApp) => a.namespace || CLUSTER_WIDE;
  const namespaces = [...new Set(visibleBase.map(nsOf))].sort();
  const visible = visibleBase.filter((a) => (!namespace || nsOf(a) === namespace) && matches(a));
  // Worst first, then by name: what is broken is what an SRE opens the map for.
  const rank: Record<Level, number> = { fail: 0, warn: 1, ok: 2 };
  visible.sort((a, b) => rank[appLevel(a)] - rank[appLevel(b)] || a.name.localeCompare(b.name));

  const nonSystem = visibleBase.filter((a) => focusApp || zoneOf(a) !== "system");
  const failing = nonSystem.filter((a) => appLevel(a) === "fail").length;
  const broken = data.links.filter((l) => l.missing).length;

  return (
    <section>
      <p>{focusApp ? <a href={mapHref}>← Application map</a> : <a href={back}>← Cluster</a>}</p>
      <div className="pagehead">
        <h1>{focusApp ? (index.apps.get(focusApp)?.name ?? focusApp) : "Application map"}</h1>
        <span className="muted small">updated {relativeTime(data.at)}</span>
      </div>
      {error != null && (
        <div className="notice error">Could not refresh: {errorMessage(error)} Showing the last map.</div>
      )}
      {(() => {
        const t = traceSummary(data.traces);
        if (!t) return null;
        return (
          <div className={t.level === "warn" ? "notice attention" : "muted small traces-summary"}>
            {t.text}
            {(data.traces?.unmatched?.length ?? 0) > 0 && (
              <ul className="small">
                {data.traces!.unmatched!.map((u) => (
                  <li key={u}>{u}</li>
                ))}
              </ul>
            )}
          </div>
        );
      })()}
      {(data.incomplete?.length ?? 0) > 0 && (
        <div className="notice attention">
          The map may be incomplete: the agent could not read {data.incomplete!.join(", ")}.
        </div>
      )}

      {!focusApp && (
        <>
          <div className="grid kpis">
            <Kpi label="Applications" value={nonSystem.length} />
            <Kpi label="Failing" value={failing} level={failing > 0 ? "fail" : "ok"} />
            <Kpi label="Broken links" value={broken} level={broken > 0 ? "fail" : "ok"} />
            <Kpi label="Unassigned objects" value={data.unassigned?.length ?? 0} />
          </div>
          <div className="filters">
            <button className={`chip ${namespace === "" ? "selected" : ""}`} onClick={() => setNamespace("")}>
              All namespaces
            </button>
            {namespaces.map((ns) => (
              <button
                key={ns}
                className={`chip ${namespace === ns ? "selected" : ""}`}
                onClick={() => setNamespace(ns)}
              >
                {ns}
              </button>
            ))}
            <span className="spacer" />
            <button
              type="button"
              className="chip"
              onClick={() => {
                setOpenId(null);
                setAllOpen(!allOpen);
              }}
            >
              {allOpen ? "Collapse all" : "Expand all"}
            </button>
            <input
              type="search"
              className="search"
              placeholder="Find an app or workload"
              aria-label="Find an app or workload"
              value={search}
              onChange={(e) => setSearch(e.target.value)}
            />
          </div>
        </>
      )}

      <Legend />

      {visible.length === 0 ? (
        <div className="card empty">
          <h2>No applications</h2>
          <p className="muted">
            {data.apps.length === 0
              ? "The agent found no workloads it could group into applications."
              : "Nothing matches the filters."}
          </p>
        </div>
      ) : (
        (() => {
          const shown = visible.filter(
            (a) => allOpen || !openId || !visible.some((v) => v.id === openId) || a.id === openId,
          );
          const card = (a: MapApp) => (
            <AppCard
              key={a.id}
              app={a}
              links={data.links}
              index={index}
              clusterId={clusterId}
              selected={selected}
              onSelect={setSelected}
              expanded={!!focusApp || allOpen || openId === a.id}
              onToggle={
                focusApp
                  ? undefined
                  : () => {
                      setAllOpen(false);
                      setOpenId(!allOpen && openId !== a.id ? a.id : null);
                    }
              }
              zone={focusApp ? undefined : zoneOf(a)}
              onMove={(z) => moveTo(a.id, z)}
              onDragStart={() => setTimeout(() => setDragId(a.id), 0)}
              onDragEnd={() => {
                setDragId(null);
                setOverZone(null);
              }}
            />
          );
          if (focusApp) return <div>{shown.map(card)}</div>;
          return (
            <div className="zones">
              {ZONES.map((z) => {
                const apps = shown.filter((a) => zoneOf(a) === z.id);
                const total = visible.filter((a) => zoneOf(a) === z.id).length;
                const forced = q !== "" || dragId !== null;
                const open = z.id !== "system" || systemOpen || forced;
                return (
                  <section
                    key={z.id}
                    className={`zone zone-${z.id}${overZone === z.id ? " over" : ""}`}
                    aria-label={z.title}
                    onDragOver={(e) => {
                      if (!dragId) return;
                      e.preventDefault();
                      setOverZone(z.id);
                    }}
                    onDragLeave={(e) => {
                      if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setOverZone(null);
                    }}
                    onDrop={(e) => {
                      e.preventDefault();
                      const id = dragId ?? e.dataTransfer.getData("text/plain");
                      const app = data.apps.find((x) => x.id === id);
                      if (app) moveTo(id, z.id);
                      setDragId(null);
                      setOverZone(null);
                    }}
                  >
                    <header className="zonehead">
                      <h2>{z.title}</h2>
                      <span className="muted small">{total}</span>
                      <span className="spacer" />
                      {z.id === "system" && !forced && (
                        <button
                          type="button"
                          className="link inline small"
                          aria-expanded={open}
                          onClick={() => setSystemOpen(!systemOpen)}
                        >
                          {open ? "Hide ▾" : "Show ▸"}
                        </button>
                      )}
                    </header>
                    {open &&
                      (apps.length === 0 ? (
                        total === 0 && <p className="muted small zoneempty">{z.hint}</p>
                      ) : (
                        <div className="appcards">{apps.map(card)}</div>
                      ))}
                  </section>
                );
              })}
            </div>
          );
        })()
      )}

      {!focusApp && (data.unassigned?.length ?? 0) > 0 && <Unassigned comps={data.unassigned!} />}
    </section>
  );
}

function MapError({ error }: { error: unknown }) {
  if (error instanceof ApiError) {
    const text: Record<string, string> = {
      agent_offline:
        "The cluster agent is not connected, so there is no map to show. It appears when the in-cluster agent is ready.",
      unsupported: "This cluster's agent is too old to send an application map. Update Kubemap.",
    };
    return <div className="notice error">{text[error.code] ?? error.message}</div>;
  }
  return <div className="notice error">{errorMessage(error)}</div>;
}

function Kpi({ label, value, level }: { label: string; value: number; level?: Level }) {
  return (
    <div className="card stat">
      <div className="muted small">{label}</div>
      <div className={`value ${level === "fail" ? "error" : ""}`}>{value}</div>
    </div>
  );
}

function Legend() {
  return (
    <ul className="legend maplegend" aria-label="Legend">
      <li>
        <span className="pod ok" aria-hidden="true" /> pod ready
      </li>
      <li>
        <span className="pod warn" aria-hidden="true" /> starting or not ready
      </li>
      <li>
        <span className="pod fail" aria-hidden="true" /> failing
      </li>
      <li>
        <span className="edgekey" aria-hidden="true" /> link found in configuration
      </li>
      <li>
        <span className="edgekey broken" aria-hidden="true" /> broken link
      </li>
    </ul>
  );
}

function HealthBadge({ level, text }: { level: Level; text: string }) {
  return (
    <span className={`badge level-${level}`}>
      <span className="dot" aria-hidden="true" />
      {text}
    </span>
  );
}

type Index = { comps: Map<string, MapComponent>; apps: Map<string, MapApp> };

function AppCard({
  app,
  links,
  index,
  clusterId,
  selected,
  onSelect,
  expanded,
  onToggle,
  zone,
  onMove,
  onDragStart,
  onDragEnd,
}: {
  expanded: boolean;
  onToggle?: () => void;
  zone?: Zone;
  onMove: (z: Zone) => void;
  onDragStart: () => void;
  onDragEnd: () => void;
  app: MapApp;
  links: MapLink[];
  index: Index;
  clusterId: string;
  selected: string | null;
  onSelect: (id: string | null) => void;
}) {
  const inApp = useMemo(() => new Set(app.components.map((c) => c.id)), [app]);
  // Memoised: the arrows are measured again whenever the inner links change.
  const { inner, outgoing, incoming } = useMemo(
    () => ({
      inner: links.filter((l) => inApp.has(l.from) && inApp.has(l.to)),
      outgoing: links.filter((l) => inApp.has(l.from) && !inApp.has(l.to)),
      incoming: links.filter((l) => !inApp.has(l.from) && inApp.has(l.to)),
    }),
    [links, inApp],
  );
  const tiers = useMemo(() => TIERS.filter((t) => app.components.some((c) => c.tier === t.id)), [app]);
  const level = appLevel(app);
  const sel = selected && inApp.has(selected) ? index.comps.get(selected) : undefined;
  const appHref = `#/clusters/${encodeURIComponent(clusterId)}/apps/${encodeURIComponent(app.id)}`;

  // Column of each box, and a lane for each link that must go around a column.
  const { colOf, lanes } = useMemo(() => {
    const colOf = new Map<string, number>();
    for (const c of app.components)
      colOf.set(
        c.id,
        tiers.findIndex((t) => t.id === c.tier),
      );
    const occupied = new Set(colOf.values());
    const lanes = new Map<MapLink, number>();
    for (const l of inner) {
      const a = colOf.get(l.from);
      const b = colOf.get(l.to);
      if (a !== undefined && b !== undefined && skipsColumns(a, b, occupied)) lanes.set(l, lanes.size);
    }
    return { colOf, lanes };
  }, [app, tiers, inner]);
  const grid = useRef<HTMLDivElement>(null);
  const boxes = useRef(new Map<string, HTMLElement>());

  if (!expanded) {
    const pods = app.components.flatMap((c) => c.pods ?? []);
    const probs = app.problems ?? [];
    return (
      <article
        className={`card appcard compact level-${level}${zone ? " draggable" : ""}`}
        draggable={!!zone}
        onDragStart={(e) => {
          e.dataTransfer.setData("text/plain", app.id);
          e.dataTransfer.effectAllowed = "move";
          onDragStart();
        }}
        onDragEnd={onDragEnd}
      >
        <header className="apphead">
          <h2>
            <a href={appHref}>{app.name}</a>
          </h2>
          <span className="spacer" />
          <HealthBadge level={level} text={app.health || LEVEL_TEXT[level]} />
        </header>
        <div className="muted small">
          {app.namespace || CLUSTER_WIDE} · {sourceText(app.source)}
        </div>
        <div className="small compactsum">
          {app.components.length === 0
            ? "No components"
            : `${app.components.length} component${app.components.length === 1 ? "" : "s"}`}
          {pods.length > 0 && ` · ${pods.filter((p) => p.ready).length}/${pods.length} pods ready`}
        </div>
        {probs.length > 0 && (
          <div
            className="small error compactprob"
            title={probs.map((p) => `${kindOf(p.id)} ${shortId(p.id, app.namespace)}: ${p.status}`).join("\n")}
          >
            {probs.length} problem{probs.length === 1 ? "" : "s"}: {kindOf(probs[0].id)}{" "}
            {shortId(probs[0].id, app.namespace)} {probs[0].status}
          </div>
        )}
        <footer className="compactfoot">
          <a className="small" href={debugAppHref(clusterId, app.id)}>
            Debug
          </a>
          <a className="small" href={appLogsHref(clusterId, app.id)}>
            Logs
          </a>
          <span className="spacer" />
          {zone && (
            <select
              className="movesel"
              aria-label={`Move ${app.name} to`}
              value=""
              onChange={(e) => e.target.value && onMove(e.target.value as Zone)}
            >
              <option value="">Move to…</option>
              {ZONES.filter((z) => z.id !== zone).map((z) => (
                <option key={z.id} value={z.id}>
                  {z.title}
                </option>
              ))}
            </select>
          )}
          {onToggle && (
            <button type="button" className="link inline small" aria-expanded={false} onClick={onToggle}>
              Show details ▸
            </button>
          )}
        </footer>
      </article>
    );
  }

  return (
    <article className={`card appcard level-${level} expanded`}>
      <header className="apphead">
        <h2>
          <a href={appHref}>{app.name}</a>
        </h2>
        <span className="muted small">
          {app.namespace || CLUSTER_WIDE} · {sourceText(app.source)}
        </span>
        <span className="spacer" />
        <HealthBadge level={level} text={app.health || LEVEL_TEXT[level]} />
        <a className="small" href={debugAppHref(clusterId, app.id)}>
          Debug
        </a>
        <a
          className="small"
          href={appLogsHref(clusterId, app.id)}
          title="Every pod of the app, merged by time, one colour per pod"
        >
          Logs
        </a>
        {zone && (
          <select
            className="movesel"
            aria-label={`Move ${app.name} to`}
            value=""
            onChange={(e) => e.target.value && onMove(e.target.value as Zone)}
          >
            <option value="">Move to…</option>
            {ZONES.filter((z) => z.id !== zone).map((z) => (
              <option key={z.id} value={z.id}>
                {z.title}
              </option>
            ))}
          </select>
        )}
        {onToggle && (
          <button type="button" className="link inline small" aria-expanded={true} onClick={onToggle}>
            Hide details ▾
          </button>
        )}
      </header>
      {(app.problems?.length ?? 0) > 0 && (
        <ul className="problems">
          {app.problems!.slice(0, 6).map((p) => {
            const target = problemTarget(app, p.id);
            const label = `${kindOf(p.id)} ${shortId(p.id, app.namespace)}`;
            return (
              <li key={p.id + p.status}>
                {target ? (
                  <button className="link inline" onClick={() => onSelect(target)}>
                    {label}
                  </button>
                ) : (
                  label
                )}
                : {p.status}
              </li>
            );
          })}
          {app.problems!.length > 6 && <li className="muted">and {app.problems!.length - 6} more</li>}
        </ul>
      )}

      {tiers.length === 0 ? (
        <p className="muted">No components.</p>
      ) : (
        <div className="mapwrap">
          <div
            className={`tiers ${tiers.length === 1 ? "single" : ""}`}
            ref={grid}
            style={{
              gridTemplateColumns: tiers.length > 1 ? `repeat(${tiers.length}, minmax(150px, 1fr))` : undefined,
              paddingTop: lanes.size > 0 ? lanePadding(lanes.size) : undefined,
            }}
          >
            {tiers.map((t) => (
              <div key={t.id} className="tier">
                <div className="tierhead">{t.label}</div>
                {app.components
                  .filter((c) => c.tier === t.id)
                  .map((c) => (
                    <ComponentBox
                      key={c.id}
                      c={c}
                      selected={selected === c.id}
                      onClick={() => onSelect(selected === c.id ? null : c.id)}
                      boxRef={(el) => {
                        if (el) boxes.current.set(c.id, el);
                        else boxes.current.delete(c.id);
                      }}
                    />
                  ))}
              </div>
            ))}
          </div>
          <Arrows
            container={grid}
            boxes={boxes}
            links={inner}
            colOf={colOf}
            lanes={lanes}
            selected={selected}
            layoutKey={app}
          />
        </div>
      )}

      {(outgoing.length > 0 || incoming.length > 0) && (
        <div className="crosslinks">
          {outgoing.length > 0 && (
            <CrossLinks title="Calls" links={outgoing} end="to" index={index} clusterId={clusterId} />
          )}
          {incoming.length > 0 && (
            <CrossLinks title="Called by" links={incoming} end="from" index={index} clusterId={clusterId} />
          )}
        </div>
      )}

      {sel && (
        <ComponentDetail c={sel} links={links} index={index} clusterId={clusterId} onClose={() => onSelect(null)} />
      )}
    </article>
  );
}

function sourceText(s: string): string {
  return (
    (
      {
        argocd: "Argo CD app",
        helm: "Helm release",
        label: "grouped by labels",
        workload: "single workload",
      } as Record<string, string>
    )[s] ?? s
  );
}

function ComponentBox({
  c,
  selected,
  onClick,
  boxRef,
}: {
  c: MapComponent;
  selected: boolean;
  onClick: () => void;
  boxRef: (el: HTMLElement | null) => void;
}) {
  const level = componentLevel(c);
  const pods = c.pods ?? [];
  const ready = pods.filter((p) => p.ready).length;
  const restarts = pods.reduce((n, p) => n + p.restarts, 0);
  return (
    <button
      ref={boxRef}
      type="button"
      className={`comp level-${level} ${selected ? "selected" : ""} ${c.missing ? "missing" : ""}`}
      onClick={onClick}
      aria-pressed={selected}
      title={c.tierReason ? `${c.tier}: ${c.tierReason}` : undefined}
    >
      <span className="comp-kind">{kindLabel(c)}</span>
      <span className="comp-name">{c.name}</span>
      {c.missing ? (
        <span className="comp-status error">does not exist</span>
      ) : (
        c.status && <span className="comp-status">{c.status}</span>
      )}
      {c.routes?.slice(0, 3).map((r, i) => (
        <span key={i} className="comp-route">
          {r.tls ? "https://" : "http://"}
          {routeText(r)}
          {(r.problems?.length ?? 0) > 0 && <span className="error"> ⚠</span>}
        </span>
      ))}
      {(c.routes?.length ?? 0) > 3 && <span className="comp-route muted">+{c.routes!.length - 3} more rules</span>}
      {pods.length > 0 && (
        <span className="pods" aria-label={`${ready} of ${pods.length} pods ready`}>
          {pods.slice(0, 12).map((p) => (
            <span
              key={p.name}
              className={`pod ${podLevel(p)} ${p.primary ? "primary" : ""}`}
              title={`${p.name}: ${p.status}`}
            />
          ))}
          {pods.length > 12 && <span className="muted small">+{pods.length - 12}</span>}
          <span className="muted small">
            {ready}/{pods.length}
          </span>
          {restarts >= RESTART_BADGE && (
            <span className="restarts" title="Container restarts, all pods">
              ↻ {restarts} restarts
            </span>
          )}
        </span>
      )}
    </button>
  );
}

/**
 * Draws the links between an app's components over its tier grid. Positions
 * come from the boxes' layout, so they are measured after render and again
 * whenever the grid changes size. The geometry is in arrows.ts: a link that
 * skips a column goes around the boxes in it, along a lane above them.
 */
function Arrows({
  container,
  boxes,
  links,
  colOf,
  lanes,
  selected,
  layoutKey,
}: {
  container: RefObject<HTMLDivElement | null>;
  boxes: RefObject<Map<string, HTMLElement>>;
  links: MapLink[];
  colOf: Map<string, number>;
  lanes: Map<MapLink, number>;
  selected: string | null;
  layoutKey: unknown;
}) {
  const [size, setSize] = useState({ w: 0, h: 0 });
  const [edges, setEdges] = useState<{ d: string; link: MapLink; label: string; at: Edge["label"] }[]>([]);
  const markerBase = "arrow" + useId().replace(/[^a-zA-Z0-9]/g, "");

  useLayoutEffect(() => {
    const el = container.current;
    if (!el) return;
    const occupied = new Set(colOf.values());
    const measure = () => {
      const base = el.getBoundingClientRect();
      setSize({ w: el.scrollWidth, h: el.scrollHeight });
      const box = (id: string): Box | undefined => {
        const r = boxes.current.get(id)?.getBoundingClientRect();
        const col = colOf.get(id);
        if (!r || col === undefined) return undefined;
        return {
          left: r.left - base.left,
          right: r.right - base.left,
          top: r.top - base.top,
          bottom: r.bottom - base.top,
          col,
        };
      };
      const out: {
        d: string;
        link: MapLink;
        label: string;
        at: Edge["label"];
      }[] = [];
      for (const l of links) {
        const a = box(l.from);
        const b = box(l.to);
        if (!a || !b) continue;
        const e = edgePath(a, b, occupied, lanes.get(l) ?? 0);
        const label = routeLabel(l.evidence.filter((ev) => ev.type === "route").map((ev) => ev.name ?? ""));
        out.push({
          d: pathData(e),
          link: l,
          label: l.evidence.some((ev) => ev.type === "route") ? label : "",
          at: e.label,
        });
      }
      setEdges(out);
    };
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, [container, boxes, links, colOf, lanes, layoutKey]);

  if (edges.length === 0) return null;
  return (
    <svg className="arrows" width={size.w} height={size.h} aria-hidden="true">
      <defs>
        {(["ok", "broken"] as const).map((k) => (
          <marker
            key={k}
            id={`${markerBase}-${k}`}
            viewBox="0 0 10 10"
            refX="9"
            refY="5"
            markerWidth="7"
            markerHeight="7"
            orient="auto-start-reverse"
          >
            <path d="M0,0 L10,5 L0,10 z" className={`head ${k}`} />
          </marker>
        ))}
      </defs>
      {edges.map(({ d, link, label, at }) => {
        const touches = selected !== null && (link.from === selected || link.to === selected);
        const state = `${link.missing ? "broken" : ""} ${link.traces ? "traced" : ""} ${touches ? "active" : selected ? "dim" : ""}`;
        return (
          <g key={link.from + "→" + link.to}>
            <path
              d={d}
              className={`edge ${state}`}
              markerEnd={`url(#${markerBase}-${link.missing ? "broken" : "ok"})`}
            />
            {label && (
              <text x={at.x} y={at.y} textAnchor={at.anchor} className={`edgelabel ${state}`}>
                {label}
              </text>
            )}
          </g>
        );
      })}
    </svg>
  );
}

function CrossLinks({
  title,
  links,
  end,
  index,
  clusterId,
}: {
  title: string;
  links: MapLink[];
  end: "from" | "to";
  index: Index;
  clusterId: string;
}) {
  return (
    <div>
      <h3>{title}</h3>
      <ul className="linklist">
        {links.map((l) => {
          const otherId = l[end];
          const other = index.comps.get(otherId);
          const otherAppId = end === "to" ? l.toApp : l.fromApp;
          const otherApp = otherAppId ? index.apps.get(otherAppId) : undefined;
          const own = index.comps.get(end === "to" ? l.from : l.to);
          return (
            <li key={l.from + "→" + l.to} className={l.missing ? "broken" : ""}>
              <span className="strong">{own?.name ?? shortId(end === "to" ? l.from : l.to)}</span>
              {end === "to" ? " → " : " ← "}
              {otherAppId ? (
                <a href={`#/clusters/${encodeURIComponent(clusterId)}/apps/${encodeURIComponent(otherAppId)}`}>
                  {otherApp?.name ?? otherAppId.split("/").pop()}
                </a>
              ) : null}
              {otherAppId ? " / " : ""}
              {other?.name ?? shortId(otherId)}
              {l.missing && (
                <span className="error"> (broken: {other?.missing ? "does not exist" : "nothing runs behind it"})</span>
              )}
              <Evidence link={l} />
            </li>
          );
        })}
      </ul>
    </div>
  );
}

function Evidence({ link }: { link: MapLink }) {
  if (link.evidence.length === 0 && !link.via?.length && !link.traces) return null;
  return (
    <ul className="evidence">
      {link.via && link.via.length > 0 && (
        <li className="muted small">through {link.via.map((v) => shortId(v)).join(", ")}</li>
      )}
      {link.evidence.map((e, i) => (
        <li key={i} className="small">
          <span className={`evtype ${e.type}`}>{e.type}</span> {e.detail}
        </li>
      ))}
      {link.traces && (
        <li className="small">
          <span className="evtype traces">confirmed by traces</span> {link.traces.detail}
          <span className="muted"> · last {relativeTime(link.traces.lastSeen)}</span>
        </li>
      )}
    </ul>
  );
}

function ComponentDetail({
  c,
  links,
  index,
  clusterId,
  onClose,
}: {
  c: MapComponent;
  links: MapLink[];
  index: Index;
  clusterId: string;
  onClose: () => void;
}) {
  const out = links.filter((l) => l.from === c.id);
  const inc = links.filter((l) => l.to === c.id);
  const name = (id: string) => index.comps.get(id)?.name ?? shortId(id, c.namespace);
  return (
    <div className="detail">
      <div className="detailhead">
        <h3>
          {c.kind.split(".")[0]}{" "}
          <span className="mono">
            {c.namespace}/{c.name}
          </span>
        </h3>
        <span className="spacer" />
        {!c.missing && (
          <a className="small" href={chainHref(clusterId, c.id)}>
            Everything connected
          </a>
        )}
        <button className="link" onClick={onClose}>
          Close
        </button>
      </div>
      <dl className="facts">
        <div className="fact">
          <dt>Tier</dt>
          <dd>
            {c.tier}
            {c.tierReason && <span className="muted"> · {c.tierReason}</span>}
          </dd>
        </div>
        <div className="fact">
          <dt>Status</dt>
          <dd>
            {c.missing ? <span className="error">does not exist</span> : c.status || LEVEL_TEXT[componentLevel(c)]}
          </dd>
        </div>
        {objectSignals(c) && (
          <div className="fact">
            <dt>Help</dt>
            <dd>
              <HintPanel signals={objectSignals(c)!} />
            </dd>
          </div>
        )}
        {c.engine && (
          <div className="fact">
            <dt>Engine</dt>
            <dd>
              {c.engine} {c.version}
            </dd>
          </div>
        )}
        {(c.services?.length ?? 0) > 0 && (
          <div className="fact">
            <dt>Services</dt>
            <dd>{c.services!.map((s) => shortId(s, c.namespace)).join(", ")}</dd>
          </div>
        )}
      </dl>

      {isWorkloadKind(c.kind) && !c.missing && <ObservePanel c={c} />}

      {(c.routes?.length ?? 0) > 0 && (
        <table className="data compact">
          <thead>
            <tr>
              <th>Rule</th>
              <th>Service</th>
              <th className="num">Ready endpoints</th>
              <th>Problems</th>
            </tr>
          </thead>
          <tbody>
            {c.routes!.map((r, i) => (
              <tr key={i}>
                <td>
                  {r.tls ? "https://" : "http://"}
                  {routeText(r)}
                  {r.entryPoints?.length ? <span className="muted small"> ({r.entryPoints.join(", ")})</span> : null}
                </td>
                <td>
                  {shortId(r.service, c.namespace)}
                  {r.port ? `:${r.port}` : ""}
                </td>
                <td className="num">{r.readyEndpoints < 0 ? "–" : r.readyEndpoints}</td>
                <td className={r.problems?.length ? "error" : "muted"}>
                  {r.problems?.join("; ") || "none"}
                  <div className="small">
                    <a
                      href={scenarioHref("traffic-503", clusterId, {
                        ns: r.service.split("/")[1] || c.namespace,
                        svc: r.service.split("/")[2] || "",
                        port: r.port && /^\d+$/.test(r.port) ? r.port : "",
                      })}
                    >
                      503? Walk through it
                    </a>
                  </div>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {(c.pods?.length ?? 0) > 0 && (
        <table className="data compact">
          <thead>
            <tr>
              <th>Pod</th>
              <th>Status</th>
              <th>Ready</th>
              <th className="num">Restarts</th>
              <th>Node</th>
            </tr>
          </thead>
          <tbody>
            {c.pods!.map((p) => (
              <tr key={p.name}>
                <td className="mono">
                  <a href={logsHref(clusterId, c.namespace, p.name)} title="Show logs">
                    {p.name}
                  </a>{" "}
                  <a className="small" href={debugHref(clusterId, c.namespace, p.name)}>
                    debug
                  </a>{" "}
                  <a className="small" href={chainHref(clusterId, `Pod/${c.namespace}/${p.name}`)}>
                    chain
                  </a>
                  {p.primary && <span className="tag">primary</span>}
                </td>
                <td>
                  <span className={`pod ${podLevel(p)}`} aria-hidden="true" /> {p.status}
                </td>
                <td>{p.ready ? "yes" : "no"}</td>
                <td className={`num ${p.restarts > 0 ? "error" : ""}`}>{p.restarts}</td>
                <td>{p.node || "–"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {(out.length > 0 || inc.length > 0) && (
        <>
          <h4>Links and their evidence</h4>
          <ul className="linklist">
            {out.map((l) => (
              <li key={"o" + l.to} className={l.missing ? "broken" : ""}>
                → <span className="strong">{name(l.to)}</span>
                {l.missing && <span className="error"> (broken)</span>}
                <Evidence link={l} />
              </li>
            ))}
            {inc.map((l) => (
              <li key={"i" + l.from} className={l.missing ? "broken" : ""}>
                ← <span className="strong">{name(l.from)}</span>
                {l.missing && <span className="error"> (broken)</span>}
                <Evidence link={l} />
              </li>
            ))}
          </ul>
        </>
      )}
    </div>
  );
}

function Unassigned({ comps }: { comps: MapComponent[] }) {
  return (
    <div className="card flush">
      <h2 className="pad">Not part of any application</h2>
      <p className="pad muted small">
        Objects the agent could not place in an app. A Service here with no pods behind it is often left over, or has a
        selector that matches nothing.
      </p>
      <table className="data">
        <thead>
          <tr>
            <th>Object</th>
            <th>Namespace</th>
            <th>Status</th>
          </tr>
        </thead>
        <tbody>
          {comps.map((c) => (
            <tr key={c.id}>
              <td>
                {c.kind.split(".")[0]} <span className="strong">{c.name}</span>
              </td>
              <td>{c.namespace}</td>
              <td className={componentLevel(c) === "ok" ? "" : "error"}>
                {c.missing ? "does not exist" : c.status || LEVEL_TEXT[componentLevel(c)]}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
