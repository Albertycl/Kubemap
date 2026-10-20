import { useCallback, useEffect, useRef, useState } from "react";
import {
  ApiError,
  appMapApi,
  debugAppHref,
  LOG_LEVELS,
  logSearchApi,
  logSearchHref,
  type LogEntry,
  type LogLevel,
  type LogSearchQuery,
  type LogSearchResult,
} from "./api";
import { errorMessage, usePoll } from "./common";
import { logsHref } from "./PodsPage";
import { entryTime, highlight, readSearch, stackLineKind, WINDOW_TEXT, WINDOWS, writeSearch } from "./logsearch";
import { appPods, FOLLOW_EVERY_MS, followFrom, mergeEntries, nextFollowDelay, podColours, type AppPod } from "./applogs";

const LEVEL_TEXT: Record<LogLevel, string> = {
  error: "Errors",
  warn: "Warnings",
  info: "Info",
  debug: "Debug",
  unknown: "No level",
};

/** How many lines the view keeps while following. */
const FOLLOW_CAP = 2000;

/** The app's pods by component, each with its colour; a click hides or shows it. */
function PodLegend({
  pods,
  colours,
  hidden,
  counts,
  onToggle,
  onOnly,
  onAll,
}: {
  pods: AppPod[];
  colours: Map<string, number>;
  hidden: Set<string>;
  counts: Map<string, number>;
  onToggle: (key: string) => void;
  onOnly: (key: string) => void;
  onAll: () => void;
}) {
  const byComponent = new Map<string, AppPod[]>();
  for (const p of pods) byComponent.set(p.component, [...(byComponent.get(p.component) ?? []), p]);
  return (
    <div className="card podlegend" aria-label="Pods of the app">
      {[...byComponent.entries()].map(([comp, ps]) => (
        <div key={comp} className="legendgroup">
          <span className="muted small">{ps[0].tier} · {comp}</span>
          <div className="legendpods">
            {ps.map((p) => (
              <span key={p.key} className={`legendpod ${hidden.has(p.key) ? "off" : ""}`}>
                <button className={`chip podchip slot-${colours.get(p.key) ?? 0}`} aria-pressed={!hidden.has(p.key)}
                  title="Show or hide this pod's lines" onClick={() => onToggle(p.key)}>
                  <span className="swatch" aria-hidden="true" />
                  <span className="mono">{p.pod}</span>
                  <span className="count"> {counts.get(p.key) ?? 0}</span>
                </button>
                <button className="linklike small" onClick={() => onOnly(p.key)} title="Show only this pod">only</button>
              </span>
            ))}
          </div>
        </div>
      ))}
      {hidden.size > 0 && (
        <button className="linklike small" onClick={onAll}>
          Show all pods
        </button>
      )}
    </div>
  );
}

/** Stack lines shown before "show all". */
const STACK_PREVIEW = 8;

function describe(e: unknown): string {
  if (e instanceof ApiError && e.code === "agent_offline") return "The cluster agent is not connected.";
  return errorMessage(e);
}

function Marked({ text, q }: { text: string; q?: string }) {
  return (
    <>
      {highlight(text, q ?? "").map((p, i) => (p.match ? <mark key={i}>{p.text}</mark> : <span key={i}>{p.text}</span>))}
    </>
  );
}

function Entry({ e, q, clusterId, slot }: { e: LogEntry; q?: string; clusterId: string; slot: number }) {
  const [open, setOpen] = useState(false);
  const stack = e.stack ?? [];
  const shown = open ? stack : stack.slice(0, STACK_PREVIEW);
  return (
    <li className={`logentry lvl-${e.level}${stack.length ? " has-stack" : ""}`}>
      <div className="logline">
        <time dateTime={e.time} title={e.time}>
          {entryTime(e.time)}
        </time>
        <span className={`lvl lvl-${e.level}`}>{e.level === "unknown" ? "–" : e.level}</span>
        <a className={`podtag slot-${slot}`} href={logsHref(clusterId, e.namespace, e.pod)} title={`${e.namespace}/${e.pod}, container ${e.container}`}>
          {e.pod}
          {e.previous ? " (previous run)" : ""}
        </a>
        <span className="logtext">
          <Marked text={e.text} q={q} />
        </span>
      </div>
      {stack.length > 0 && (
        <pre className="stack" aria-label="Stack trace">
          {shown.map((l, i) => (
            <div key={i} className={`st-${stackLineKind(l)}`}>
              <Marked text={l} q={q} />
            </div>
          ))}
          {stack.length > STACK_PREVIEW && (
            <button className="linklike small" onClick={() => setOpen(!open)}>
              {open ? "Show less" : `Show all ${stack.length} lines`}
            </button>
          )}
        </pre>
      )}
    </li>
  );
}

/** #/clusters/<id>/log-search[/<app>][?q=…&levels=…&since=…&component=…&previous=0] */
export function LogSearchPage({ clusterId, app, query }: { clusterId: string; app?: string; query?: string }) {
  const loadMap = useCallback(() => appMapApi.get(clusterId), [clusterId]);
  const map = usePoll(loadMap, 60000);
  const apps = (map.data?.apps ?? []).filter((a) => !a.system || a.id === app);
  const current = map.data?.apps.find((a) => a.id === app);

  const search = readSearch(app ?? "", query);
  const [text, setText] = useState(search.q ?? "");
  const [result, setResult] = useState<LogSearchResult | undefined>(undefined);
  const [error, setError] = useState<string>("");
  const [loading, setLoading] = useState(false);
  const [again, setAgain] = useState(0);
  // The lines shown: the search's, plus what "follow" reads later (KM-55).
  const [shown, setShown] = useState<LogEntry[]>([]);
  const [follow, setFollow] = useState(false);
  const [hidden, setHidden] = useState<Set<string>>(new Set());
  const [fresh, setFresh] = useState(0);
  const [waiting, setWaiting] = useState(0); // > 0: the agent was busy; the next read is in this many ms
  // Why the last read was refused: the agent's own message (busy with other
  // searches, or not connected at all), review km-55-fixes N-1.
  const [waitReason, setWaitReason] = useState("");

  // The URL is the state: every change of the search writes it, and the
  // page (keyed on the route) searches again.
  const go = (s: Partial<LogSearchQuery>, appId = app) => {
    if (!appId) return;
    const next = { ...search, ...s, app: appId };
    const hash = `${logSearchHref(clusterId, appId)}?${writeSearch(next)}`;
    if (window.location.hash === hash) setAgain((n) => n + 1); // the same search: run it again (new lines)
    else window.location.hash = hash;
  };

  const key = JSON.stringify(search);
  useEffect(() => {
    if (!app) return;
    let gone = false;
    setLoading(true);
    setError("");
    logSearchApi
      .search(clusterId, JSON.parse(key) as LogSearchQuery)
      .then((r) => {
        if (gone) return;
        setResult(r.search);
        setShown(r.search.entries);
        setFresh(0);
      })
      .catch((e) => !gone && setError(describe(e)))
      .finally(() => !gone && setLoading(false));
    return () => {
      gone = true;
    };
  }, [clusterId, app, key, again]);

  // Follow: every 10 s, read what is newer than the last line shown, with the
  // same filters, and add it (KM-55). Lines already shown are not repeated.
  // While the agent is busy (its two search slots are taken), wait longer
  // instead of stopping (review-km-55 N-1).
  const shownRef = useRef<LogEntry[]>([]);
  shownRef.current = shown;
  const untilRef = useRef("");
  untilRef.current = result?.until ?? "";
  useEffect(() => {
    if (!follow || !app) return;
    let stopped = false;
    let delay = FOLLOW_EVERY_MS;
    let t = 0;
    const read = async () => {
      if (document.visibilityState === "visible") {
        try {
          const q = { ...(JSON.parse(key) as LogSearchQuery), after: followFrom(shownRef.current, untilRef.current) };
          const r = await logSearchApi.search(clusterId, q);
          const m = mergeEntries(shownRef.current, r.search.entries, FOLLOW_CAP);
          if (m.added) {
            setShown(m.entries);
            setFresh((n) => n + m.added);
          }
          untilRef.current = r.search.until;
          delay = nextFollowDelay(delay, false);
          setWaiting(0);
        } catch (e) {
          if (e instanceof ApiError && (e.code === "unavailable" || e.code === "timeout")) {
            delay = nextFollowDelay(delay, true);
            setWaiting(delay);
            setWaitReason(e.message);
          } else {
            setError(describe(e));
            setFollow(false);
            return;
          }
        }
      }
      if (!stopped) t = window.setTimeout(read, delay);
    };
    t = window.setTimeout(read, delay);
    return () => {
      stopped = true;
      window.clearTimeout(t);
    };
  }, [follow, app, clusterId, key]);

  // One colour per pod, in the app's order (entry, frontend, backend, data).
  const pods = appPods(current);
  const colours = podColours(
    pods.map((p) => p.key),
    shown.map((e) => `${e.namespace}/${e.pod}`),
  );
  const slot = (e: LogEntry) => colours.get(`${e.namespace}/${e.pod}`) ?? 0;
  const visible = hidden.size ? shown.filter((e) => !hidden.has(`${e.namespace}/${e.pod}`)) : shown;
  const lineCount = new Map<string, number>();
  for (const e of shown) lineCount.set(`${e.namespace}/${e.pod}`, (lineCount.get(`${e.namespace}/${e.pod}`) ?? 0) + 1);
  const allLevels = (search.levels ?? []).length === 0 && !search.q;

  const toggleLevel = (l: LogLevel) => {
    const have = search.levels ?? [];
    go({ levels: have.includes(l) ? have.filter((x) => x !== l) : [...have, l] });
  };
  const failed = (result?.sources ?? []).filter((s) => s.error);

  return (
    <section className="logsearch">
      <p>
        <a href={`#/clusters/${encodeURIComponent(clusterId)}/apps${app ? "/" + encodeURIComponent(app) : ""}`}>← Application map</a>
      </p>
      <div className="pagehead">
        <h1>{allLevels && current ? `Logs · ${current.name}` : "Log search"}</h1>
        {app && (
          <a className="button" href={debugAppHref(clusterId, app)}>
            Debug this app
          </a>
        )}
      </div>
      <p className="muted small">
        Every pod of an app at once, merged by time, one colour per pod. Stack traces stay with the line that started
        them. The cluster agent reads the logs with the same read-only right as the log view.
      </p>

      <form
        className="filters"
        onSubmit={(e) => {
          e.preventDefault();
          go({ q: text.trim() || undefined });
        }}
      >
        <label className="inline">
          App
          <select value={app ?? ""} onChange={(e) => go({ component: undefined }, e.target.value)}>
            {!app && <option value="">Choose an app…</option>}
            {apps.map((a) => (
              <option key={a.id} value={a.id}>
                {a.id}
              </option>
            ))}
          </select>
        </label>
        {current && current.components.some((c) => c.pods?.length) && (
          <label className="inline">
            Part
            <select value={search.component ?? ""} onChange={(e) => go({ component: e.target.value || undefined })}>
              <option value="">all pods</option>
              {current.components
                .filter((c) => c.pods?.length)
                .map((c) => (
                  <option key={c.id} value={c.id}>
                    {c.kind} {c.name} ({c.pods?.length})
                  </option>
                ))}
            </select>
          </label>
        )}
        <input
          className="search"
          type="search"
          placeholder="Text, e.g. timeout or pool"
          value={text}
          maxLength={200}
          onChange={(e) => setText(e.target.value)}
          aria-label="Search text"
        />
        <label className="inline">
          Time
          <select value={search.since} onChange={(e) => go({ since: e.target.value })}>
            {WINDOWS.map((w) => (
              <option key={w} value={w}>
                {WINDOW_TEXT[w]}
              </option>
            ))}
          </select>
        </label>
        <label className="toggle">
          <input type="checkbox" checked={search.previous} onChange={(e) => go({ previous: e.target.checked })} />
          Include runs before a restart
        </label>
        <button className="primary" type="submit" disabled={!app || loading}>
          {loading ? "Searching…" : "Search"}
        </button>
      </form>

      <div className="filters" role="group" aria-label="Levels">
        {LOG_LEVELS.map((l) => (
          <button
            key={l}
            className={`chip lvlchip lvl-${l} ${(search.levels ?? []).includes(l) ? "selected" : ""}`}
            aria-pressed={(search.levels ?? []).includes(l)}
            onClick={() => toggleLevel(l)}
            disabled={!app}
          >
            {LEVEL_TEXT[l]}
            {result && <span className="count"> {result.levels[l] ?? 0}</span>}
          </button>
        ))}
        {(search.levels ?? []).length > 0 && (
          <button className="chip" onClick={() => go({ levels: [] })} disabled={!app}>
            All levels
          </button>
        )}
      </div>

      {!app && <div className="card empty">Choose an app to search the logs of all its pods.</div>}
      {map.error !== null && !map.data && <div className="notice error">{describe(map.error)}</div>}
      {error && <div className="notice error">{error}</div>}

      {result && app && (
        <>
          <p className="small muted">
            {result.matched} {result.matched === 1 ? "entry" : "entries"} in {new Set(result.sources.map((s) => s.pod)).size} pods
            {result.truncated ? `, the newest ${result.entries.length} shown` : ""} · {entryTime(result.since)} to {entryTime(result.until)}{" "}
            (your time)
          </p>
          {failed.length > 0 && (
            <div className="notice attention">
              Not read: {failed.map((s) => `${s.pod}/${s.container}${s.previous ? " (previous)" : ""}: ${s.error}`).join("; ")}
            </div>
          )}
          {(result.notes ?? []).map((n) => (
            <div key={n} className="notice attention small">
              {n}
            </div>
          ))}
          {pods.length > 0 && (
            <PodLegend pods={pods} colours={colours} hidden={hidden} counts={lineCount}
              onToggle={(k) => setHidden((h) => { const n = new Set(h); if (n.has(k)) n.delete(k); else n.add(k); return n; })}
              onOnly={(k) => setHidden(new Set(pods.map((p) => p.key).filter((x) => x !== k)))}
              onAll={() => setHidden(new Set())} />
          )}
          <div className="filters followbar">
            <label className="toggle">
              <input type="checkbox" checked={follow} onChange={(e) => { setFollow(e.target.checked); setFresh(0); }} />
              Follow new lines (every {FOLLOW_EVERY_MS / 1000} s)
            </label>
            {follow && <span className="live small"><span className="dot" aria-hidden="true" /> following{fresh ? ` · ${fresh} new` : ""}</span>}
            {follow && waiting > 0 && (
              <span className="muted small">
                {waitReason || "the agent is busy with other searches"}; next read in {waiting / 1000} s
              </span>
            )}
            {shown.length >= FOLLOW_CAP && <span className="muted small">showing the newest {FOLLOW_CAP} lines</span>}
          </div>
          {shown.length === 0 ? (
            <div className="card empty">
              No log lines match. {(search.levels ?? []).length ? "Try all levels, " : ""}a longer time, or other text.
            </div>
          ) : (
            <ol className="logentries">
              {visible.map((e, i) => (
                <Entry key={`${e.pod}/${e.container}/${e.time}/${i}`} e={e} q={search.q} clusterId={clusterId} slot={slot(e)} />
              ))}
            </ol>
          )}
          <details className="tableview">
            <summary>Logs read ({result.sources.length})</summary>
            <div className="tablescroll">
            <table>
              <thead>
                <tr>
                  <th>Pod</th>
                  <th>Container</th>
                  <th>Lines in the window</th>
                  <th>Matched</th>
                  <th></th>
                </tr>
              </thead>
              <tbody>
                {result.sources.map((s) => (
                  <tr key={`${s.namespace}/${s.pod}/${s.container}/${s.previous}`}>
                    <td className="mono">{s.pod}</td>
                    <td>
                      {s.container}
                      {s.previous ? " (previous run)" : ""}
                    </td>
                    <td>{s.lines}</td>
                    <td>{s.matched}</td>
                    <td className={s.error ? "error" : "muted"}>{s.error ?? (s.truncated ? "older lines of the window not searched" : "")}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            </div>
          </details>
        </>
      )}
    </section>
  );
}
