import { useCallback, useEffect, useState } from "react";
import {
  appMapApi,
  debugApi,
  debugAppHref,
  debugHref,
  relativeTime,
  resourceApi,
  type DebugStep,
  type PodDebug,
} from "./api";
import { CopyBox, errorMessage, usePoll } from "./common";
import { appPods, openByDefault, tally } from "./debug";
import { logsHref, podLevel } from "./PodsPage";

const VERDICT_TEXT: Record<DebugStep["verdict"], string> = { ok: "OK", problem: "Problem", unknown: "Not checked" };
const VERDICT_LEVEL: Record<DebugStep["verdict"], string> = { ok: "ok", problem: "fail", unknown: "info" };

/**
 * Guided debugging (KM-76 UI). Three routes:
 *   #/clusters/<id>/debug                            pick a pod or an app
 *   #/clusters/<id>/namespaces/<ns>/pods/<pod>/debug  the checklist for a pod
 *   #/clusters/<id>/debug/app/<app>                  an app's pods, worst one first
 */
export function DebugPage({ clusterId, namespace, pod, app }: { clusterId: string; namespace?: string; pod?: string; app?: string }) {
  if (namespace && pod) return <PodChecklist clusterId={clusterId} namespace={namespace} pod={pod} />;
  if (app) return <AppDebug clusterId={clusterId} appId={app} />;
  return <Picker clusterId={clusterId} />;
}

function Intro() {
  return (
    <p className="muted lead">
      Kubemap walks the checklist an SRE follows for a broken pod, in the same order: status and reason → events → logs
      → exit code → requests and limits against usage → node → Service endpoints → NetworkPolicy. Each step shows what
      was found, the evidence, and the <code>kubectl</code> command to check it yourself.
    </p>
  );
}

/** The checklist for one pod. */
function PodChecklist({ clusterId, namespace, pod, embedded }: { clusterId: string; namespace: string; pod: string; embedded?: boolean }) {
  const [data, setData] = useState<{ debug: PodDebug; at: string } | undefined>();
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const run = useCallback(async () => {
    setBusy(true);
    try {
      setData(await debugApi.pod(clusterId, namespace, pod));
      setError(null);
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  }, [clusterId, namespace, pod]);
  // Run once when the pod is chosen; "Run again" repeats it. No polling:
  // a walk reads logs, events and policies, and the result is a snapshot.
  useEffect(() => {
    setData(undefined);
    run();
  }, [run]);

  const d = data?.debug;
  const back = `#/clusters/${encodeURIComponent(clusterId)}/debug`;
  return (
    <section>
      {!embedded && (
        <>
          <p>
            <a href={back}>← Guided debugging</a>
          </p>
          <div className="pagehead">
            <h1 className="mono">{pod}</h1>
            <span className="muted small">
              {namespace}
              {d?.owner ? ` · ${d.owner}` : ""}
            </span>
          </div>
          <Intro />
        </>
      )}
      <div className="actions left">
        <button onClick={run} disabled={busy}>
          {busy ? "Walking the checklist…" : "Run again"}
        </button>
        <a className="button" href={logsHref(clusterId, namespace, pod)}>
          Logs
        </a>
        {embedded && (
          <a className="button" href={debugHref(clusterId, namespace, pod)}>
            Open this pod alone
          </a>
        )}
        {data && <span className="muted small">walked {relativeTime(data.at)}</span>}
      </div>
      {error != null && <div className="notice error">{errorMessage(error)}</div>}
      {!d ? (
        busy && <p className="muted">Walking the checklist…</p>
      ) : (
        <>
          {d.cause ? (
            <div className="cause problem">
              <div className="small strong">Most likely cause · step {stepNumber(d, d.causeStep)}</div>
              <div className="cause-text">{d.cause}</div>
            </div>
          ) : (
            <div className="cause ok">
              <div className="cause-text">No problem found in the {d.steps.length} steps.</div>
            </div>
          )}
          <p className="small muted">
            Status {d.status} · {tally(d.steps)}
          </p>
          <ol className="checklist">
            {d.steps.map((s) => (
              <Step key={s.key} s={s} cause={s.key === d.causeStep} open={openByDefault(s, d.causeStep)} clusterId={clusterId} />
            ))}
          </ol>
          {(d.notes?.length ?? 0) > 0 && (
            <ul className="small muted notes">
              {d.notes!.map((n) => (
                <li key={n}>{n}</li>
              ))}
            </ul>
          )}
        </>
      )}
    </section>
  );
}

function stepNumber(d: PodDebug, key?: string): string {
  const i = d.steps.findIndex((s) => s.key === key);
  return i < 0 ? "?" : `${i + 1}, ${d.steps[i].title.toLowerCase()}`;
}

function Step({ s, cause, open, clusterId }: { s: DebugStep; cause: boolean; open: boolean; clusterId: string }) {
  return (
    <li className={`step verdict-${s.verdict} ${cause ? "is-cause" : ""}`}>
      <details open={open}>
        <summary>
          <span className={`badge level-${VERDICT_LEVEL[s.verdict]}`}>{VERDICT_TEXT[s.verdict]}</span>
          <span className="step-title">{s.title}</span>
          <span className="step-finding">{s.finding}</span>
        </summary>
        {(s.evidence?.length ?? 0) > 0 && (
          <ul className="evidence-list small">
            {s.evidence!.map((e, i) => (
              <li key={i}>{e}</li>
            ))}
          </ul>
        )}
        {s.commands.length > 0 && (
          <div className="step-commands">
            <div className="small muted">Check it yourself</div>
            <CopyBox text={s.commands.join("\n")} label={`kubectl commands for ${s.title}`} />
          </div>
        )}
        {s.key === "netpol" && (
          <p className="small step-link">
            <a href={`#/clusters/${encodeURIComponent(clusterId)}/netpol`}>Open the NetworkPolicy checker</a> to test other
            connections or see the namespace matrix.
          </p>
        )}
      </details>
    </li>
  );
}

/** An app: its pods worst first, and the checklist for the chosen one. */
function AppDebug({ clusterId, appId }: { clusterId: string; appId: string }) {
  const load = useCallback(() => appMapApi.get(clusterId, { app: appId }), [clusterId, appId]);
  const { data, error } = usePoll(load, 30000, appId);
  const app = data?.apps.find((a) => a.id === appId);
  const pods = app ? appPods(app) : [];
  const [chosen, setChosen] = useState<string>("");
  const current = pods.find((p) => p.namespace + "/" + p.name === chosen) ?? pods[0];
  return (
    <section>
      <p>
        <a href={`#/clusters/${encodeURIComponent(clusterId)}/debug`}>← Guided debugging</a>
      </p>
      <div className="pagehead">
        <h1>{app?.name ?? appId}</h1>
        {app && <span className="muted small">{app.health}</span>}
      </div>
      <Intro />
      {error != null && <div className="notice error">{errorMessage(error)}</div>}
      {!data ? (
        !error && <p className="muted">Loading the app…</p>
      ) : pods.length === 0 ? (
        <div className="card empty">
          <h2>No pods</h2>
          <p className="muted">This app has no running pods to debug. Check its workloads on the application map.</p>
        </div>
      ) : (
        <>
          <div className="filters">
            <label className="inline">
              Pod
              <select value={current.namespace + "/" + current.name} onChange={(e) => setChosen(e.target.value)}>
                {pods.map((p) => (
                  <option key={p.namespace + "/" + p.name} value={p.namespace + "/" + p.name}>
                    {p.name} · {p.component} · {p.status}
                    {p.restarts ? ` · ${p.restarts} restarts` : ""}
                  </option>
                ))}
              </select>
            </label>
            <span className="muted small">The worst pod is chosen first.</span>
          </div>
          <PodChecklist
            key={current.namespace + "/" + current.name}
            clusterId={clusterId}
            namespace={current.namespace}
            pod={current.name}
            embedded
          />
        </>
      )}
    </section>
  );
}

/** Pick a pod (pods with problems first) or an app. */
function Picker({ clusterId }: { clusterId: string }) {
  const pods = usePoll(() => resourceApi.pods(clusterId), 15000);
  const apps = usePoll(() => appMapApi.get(clusterId), 30000);
  const [all, setAll] = useState(false);
  const list = (pods.data?.pods ?? [])
    .map((p) => ({ p, level: podLevel(p) }))
    .filter((x) => all || x.level !== "ok")
    .sort((a, b) => (a.level === b.level ? b.p.restarts - a.p.restarts : a.level === "fail" ? -1 : b.level === "fail" ? 1 : 0));
  const userApps = (apps.data?.apps ?? []).filter((a) => !a.system);
  return (
    <section>
      <p>
        <a href={`#/clusters/${encodeURIComponent(clusterId)}`}>← Cluster</a>
      </p>
      <div className="pagehead">
        <h1>Guided debugging</h1>
      </div>
      <Intro />
      <div className="debug-pick">
        <div className="card flush">
          <h2 className="pad">A pod</h2>
          {pods.error != null && <p className="pad error">{errorMessage(pods.error)}</p>}
          <p className="pad small">
            <label className="toggle">
              <input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} />
              Show all pods, not only those with problems
            </label>
          </p>
          {!pods.data ? (
            <p className="pad muted">Loading…</p>
          ) : list.length === 0 ? (
            <p className="pad muted">No pod has a problem right now.</p>
          ) : (
            <table className="data clickable pods">
              <tbody>
                {list.slice(0, 200).map(({ p, level }) => {
                  const href = debugHref(clusterId, p.namespace, p.name);
                  return (
                    <tr key={p.namespace + "/" + p.name} onClick={() => (window.location.hash = href)}>
                      <td>
                        <a href={href} className="strong mono">
                          {p.name}
                        </a>
                        <div className="muted small">{p.namespace}</div>
                      </td>
                      <td>
                        <span className={`badge level-${level}`}>
                          <span className="dot" aria-hidden="true" />
                          {p.status}
                        </span>
                      </td>
                      <td className={`num ${p.restarts >= 5 ? "error" : ""}`}>{p.restarts} restarts</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
        </div>
        <div className="card flush">
          <h2 className="pad">An app</h2>
          {apps.error != null && <p className="pad error">{errorMessage(apps.error)}</p>}
          {!apps.data ? (
            <p className="pad muted">Loading…</p>
          ) : (
            <table className="data clickable">
              <tbody>
                {userApps.map((a) => {
                  const href = debugAppHref(clusterId, a.id);
                  return (
                    <tr key={a.id} onClick={() => (window.location.hash = href)}>
                      <td>
                        <a href={href} className="strong">
                          {a.name}
                        </a>
                        <div className="muted small">{a.namespace}</div>
                      </td>
                      <td>
                        <span className={`badge level-${a.state === "fail" ? "fail" : a.state === "warn" ? "warn" : "ok"}`}>
                          <span className="dot" aria-hidden="true" />
                          {a.health}
                        </span>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
        </div>
      </div>
    </section>
  );
}
