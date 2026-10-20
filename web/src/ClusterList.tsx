import { CriticalBadge, useAlerts } from "./AlertBanner";
import { api, relativeTime, type Cluster, type Alert } from "./api";
import { errorMessage, StatusBadge, usePoll } from "./common";

export function ClusterList() {
  const { data: clusters, error } = usePoll(api.clusters);
  const alerts = useAlerts();

  return (
    <section>
      <div className="pagehead">
        <h1>Clusters</h1>
        {clusters !== undefined && clusters.length > 0 && (
          <p className="muted small pagehead-sub">
            {clusters.length} {clusters.length === 1 ? "cluster" : "clusters"}
            {alerts && alerts.length > 0 ? ` · ${alerts.length} with critical findings` : ""}
          </p>
        )}
      </div>

      {error != null && <div className="notice error">{errorMessage(error)}</div>}

      {clusters === undefined ? (
        <p className="muted">Loading…</p>
      ) : clusters.length === 0 ? (
        <div className="card empty">
          <h2>No clusters yet</h2>
          <p className="muted">The server has not created its cluster record yet. Check the pod logs: kubectl -n kubemap-system logs deploy/kubemap.</p>
        </div>
      ) : (
        <div className="card flush">
          <table className="clusters">
            <thead>
              <tr>
                <th>Name</th>
                <th>Status</th>
                <th>Kubernetes</th>
                <th className="num">Nodes</th>
                <th>Agent</th>
                <th>Last seen</th>
              </tr>
            </thead>
            <tbody>
              {clusters.map((c) => (
                <Row key={c.id} c={c} alerts={alerts} />
              ))}
            </tbody>
          </table>
        </div>
      )}

    </section>
  );
}

function Row({ c, alerts }: { c: Cluster; alerts: Alert[] | undefined }) {
  const href = `#/clusters/${encodeURIComponent(c.id)}`;
  return (
    <tr onClick={() => (window.location.hash = href)}>
      <td>
        <a href={href} className="strong">
          {c.name}
        </a>
      </td>
      <td>
        <StatusBadge cluster={c} /> <CriticalBadge alerts={alerts} clusterId={c.id} />
      </td>
      <td>{c.kubernetesVersion || <span className="muted">–</span>}</td>
      <td className="num">{c.nodeCount}</td>
      <td>{c.agentVersion || <span className="muted">–</span>}</td>
      <td title={c.lastSeen ? new Date(c.lastSeen).toLocaleString() : undefined}>{relativeTime(c.lastSeen)}</td>
    </tr>
  );
}
