import { useEffect, useState } from "react";
import { accessApi, api, type ArgoApp, type Cluster } from "./api";
import { CopyBox, errorMessage } from "./common";
import { argoAttention, argoCommands, healthLevel, lastSyncText, resourceHref, syncLevel } from "./access";

/**
 * #/clusters/<id>/argocd: every Argo CD application with where it comes
 * from, its sync and health, and each managed resource (KM-37).
 */
export function ArgoPage({ clusterId }: { clusterId: string }) {
  const [cluster, setCluster] = useState<Cluster | undefined>();
  const [data, setData] = useState<{ installed: boolean; apps: ArgoApp[]; truncated?: boolean; unreadable?: number } | undefined>();
  const [error, setError] = useState<unknown>(null);
  const load = () => accessApi.argocd(clusterId).then(setData, setError);
  useEffect(() => {
    api.cluster(clusterId).then(setCluster, () => {});
    load();
  }, [clusterId]);

  return (
    <section className="argo">
      <p>
        <a href={`#/clusters/${encodeURIComponent(clusterId)}`}>← {cluster?.name ?? "Cluster"}</a>
      </p>
      <div className="pagehead">
        <h1>Argo CD applications</h1>
        <button onClick={load}>Refresh</button>
        <a className="button" href={`#/clusters/${encodeURIComponent(clusterId)}/rbac`}>
          Who can do what
        </a>
      </div>
      <p className="muted lead">
        What Argo CD deploys: each app's Git source, its sync and health, and every resource it manages with its own sync
        and health. Problems first. This page only reads: to sync or roll back, use Argo CD or Git.
      </p>
      {error != null && <div className="notice error">{errorMessage(error)}</div>}
      {data && !data.installed && (
        <div className="card empty">
          <p>Argo CD is not installed in this cluster (no Application resource).</p>
        </div>
      )}
      {data?.installed && data.apps.length === 0 && <div className="card empty">No Argo CD applications.</div>}
      {data?.truncated && <div className="notice attention">Not every application is shown: the list was cut.</div>}
      {(data?.unreadable ?? 0) > 0 && <div className="notice attention">{data!.unreadable} application(s) could not be read.</div>}
      {data?.apps.map((a) => <ArgoCard key={`${a.namespace}/${a.name}`} clusterId={clusterId} app={a} />)}
    </section>
  );
}

function ArgoCard({ clusterId, app: a }: { clusterId: string; app: ArgoApp }) {
  const attention = argoAttention(a);
  const [open, setOpen] = useState(attention !== null);
  const c = encodeURIComponent(clusterId);
  return (
    <div className={`card argo-app ${attention ? "attention" : ""}`}>
      <div className="argo-head">
        <h2>{a.name}</h2>
        <span className={`badge level-${syncLevel(a.sync)}`}>{a.sync}</span>
        <span className={`badge level-${healthLevel(a.health)}`}>{a.health}</span>
        {a.operationPhase && (
          <span className={`badge level-${a.operationPhase === "Succeeded" ? "ok" : a.operationPhase === "Running" ? "info" : "fail"}`}>
            last sync {a.operationPhase.toLowerCase()}
          </span>
        )}
        <span className="spacer" />
        <a className="small" href={`#/clusters/${c}/apps/${encodeURIComponent(a.mapId)}`}>
          On the app map
        </a>
      </div>
      {attention && <p className="warn">{attention}</p>}
      {a.healthMessage && <p className="muted small">{a.healthMessage}</p>}
      <dl className="argo-facts small">
        <dt>Source</dt>
        <dd>
          {a.sources.length === 0
            ? "—"
            : a.sources.map((s, i) => (
                <div key={i}>
                  <code>{s.repoURL}</code>
                  {s.path && <> path <code>{s.path}</code></>}
                  {s.chart && <> chart <code>{s.chart}</code></>}
                  {s.targetRevision && <> at <code>{s.targetRevision}</code></>}
                </div>
              ))}
        </dd>
        <dt>Deploys to</dt>
        <dd>
          {a.destNamespace ? <code>{a.destNamespace}</code> : "—"} on {a.destServer || "—"} · project {a.project || "default"}
        </dd>
        <dt>Sync policy</dt>
        <dd>
          {a.autoSync ? `automatic${a.prune ? ", prune" : ""}${a.selfHeal ? ", self-heal" : ""}` : "manual"}
          {!a.selfHeal && a.sync === "OutOfSync" && (
            <span className="muted"> · without self-heal, a change made in the cluster stays until the next sync</span>
          )}
        </dd>
        {a.revision && (
          <>
            <dt>Synced revision</dt>
            <dd>
              <code>{a.revision.slice(0, 12)}</code>
              {a.reconciledAt && <span className="muted"> · compared {new Date(a.reconciledAt).toLocaleString()}</span>}
            </dd>
          </>
        )}
        {(a.operationMessage || a.operationRunning) && (
          <>
            <dt>Last sync</dt>
            <dd>
              {lastSyncText(a)}
              {a.operationMessage && <div>{a.operationMessage}</div>}
              {a.operationFinished && !a.operationRunning && (
                <span className="muted">{new Date(a.operationFinished).toLocaleString()}</span>
              )}
            </dd>
          </>
        )}
      </dl>
      {(a.conditions?.length ?? 0) > 0 && (
        <ul className="small">
          {a.conditions!.map((x, i) => (
            <li key={i} className="warn">
              {x.type}: {x.message}
            </li>
          ))}
        </ul>
      )}
      <button className="link" onClick={() => setOpen((v) => !v)}>
        {open ? "Hide" : "Show"} the {a.resources.length} managed resource{a.resources.length === 1 ? "" : "s"}
      </button>
      {open && (
        <div className="tablewrap">
          <table className="data compact">
            <thead>
              <tr>
                <th>Resource</th>
                <th>Namespace</th>
                <th>Sync</th>
                <th>Health</th>
              </tr>
            </thead>
            <tbody>
              {a.resources.map((r) => (
                <tr key={`${r.group}/${r.kind}/${r.namespace}/${r.name}`}>
                  <td>
                    <a href={resourceHref(clusterId, a, r)}>
                      {r.kind}/{r.name}
                    </a>
                    {r.hook && <span className="muted small"> hook</span>}
                    {r.requiresPruning && <span className="badge level-warn"> to prune</span>}
                  </td>
                  <td className="small">{r.namespace || "—"}</td>
                  <td>{r.status ? <span className={`badge level-${syncLevel(r.status)}`}>{r.status}</span> : "—"}</td>
                  <td>
                    {r.health ? <span className={`badge level-${healthLevel(r.health)}`}>{r.health}</span> : <span className="muted">—</span>}
                    {r.healthMessage && <div className="muted small">{r.healthMessage}</div>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {attention && <CopyBox text={argoCommands(a).join("\n")} label="Commands that show the drift" />}
        </div>
      )}
    </div>
  );
}
