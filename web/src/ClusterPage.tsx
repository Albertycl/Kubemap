import { CriticalBadge, useAlerts } from "./AlertBanner";
import { api, ApiError, nodeApi, percent, relativeTime, type Cluster } from "./api";
import { errorMessage, StatusBadge, usePoll } from "./common";
import { chainHref } from "./chain";
import { Meter, NodeStatus } from "./NodePage";

export function ClusterPage({ id }: { id: string }) {
  const { data: c, error } = usePoll(() => api.cluster(id));
  const alerts = useAlerts();

  if (error instanceof ApiError && error.status === 404) {
    return (
      <section>
        <p>
          <a href="#/">← Clusters</a>
        </p>
        <div className="card empty">
          <h2>Cluster not found</h2>
          <p className="muted">This ID is not a cluster Kubemap knows.</p>
        </div>
      </section>
    );
  }
  if (!c) return <p className="muted">{error ? errorMessage(error) : "Loading…"}</p>;

  return (
    <section>
      <p>
        <a href="#/">← Clusters</a>
      </p>
      <div className="pagehead">
        <h1>{c.name}</h1>
        <StatusBadge cluster={c} />
        <CriticalBadge alerts={alerts} clusterId={c.id} />
      </div>

      {(
        <>
          {c.status === "disconnected" && (
            <div className="notice error">
              The agent has not been heard from since {relativeTime(c.lastSeen)}. Check that its pod runs and can reach
              the Kubemap server on port 8080 (Service kubemap in kubemap-system).
            </div>
          )}
          <div className="grid">
            <Stat label="Nodes" value={String(c.nodeCount)} />
            <Stat label="Kubernetes" value={c.kubernetesVersion || "–"} />
            <Stat label="Agent" value={c.agentVersion || "–"} />
            <Stat label="Last seen" value={relativeTime(c.lastSeen)} />
          </div>
          <div className="tools">
          <div className="card maplink">
            <div>
              <h2>Who can do what</h2>
              <p className="muted">
                An account's roles through its bindings, who may read Secrets or exec into pods, and the monitoring
                namespace's permissions.
              </p>
            </div>
            <a className="button" href={`#/clusters/${encodeURIComponent(c.id)}/rbac`}>
              Check permissions
            </a>
          </div>
          <div className="card maplink">
            <div>
              <h2>Argo CD</h2>
              <p className="muted">Each Argo CD app with its sync and health, and every resource it manages.</p>
            </div>
            <a className="button" href={`#/clusters/${encodeURIComponent(c.id)}/argocd`}>
              Argo CD apps
            </a>
          </div>
          <div className="card maplink">
            <div>
              <h2>NetworkPolicy checker</h2>
              <p className="muted">Can A reach B on a port, and which policy decides; plus a matrix per namespace.</p>
            </div>
            <a className="button" href={`#/clusters/${encodeURIComponent(c.id)}/netpol`}>
              Check connections
            </a>
          </div>
          <div className="card maplink">
            <div>
              <h2>What is connected</h2>
              <p className="muted">Any resource with its whole chain: owner, pods, Services, routes, config, volumes, ServiceAccount, policies.</p>
            </div>
            <a className="button" href={chainHref(c.id)}>
              Follow a resource
            </a>
          </div>
          <div className="card maplink">
            <div>
              <h2>Guided debugging</h2>
              <p className="muted">Pick a pod or an app; Kubemap walks the SRE checklist and shows the evidence and commands.</p>
            </div>
            <a className="button" href={`#/clusters/${encodeURIComponent(c.id)}/debug`}>
              Debug a pod or app
            </a>
          </div>
          <div className="card maplink">
            <div>
              <h2>Problems</h2>
              <p className="muted">
                Health checks on this cluster's nodes and pods, worst first, each with the fix and the command that
                proves it.
              </p>
            </div>
            <a className="button" href={`#/clusters/${encodeURIComponent(c.id)}/problems`}>
              Find problems
            </a>
          </div>
          <div className="card maplink">
            <div>
              <h2>YAML view</h2>
              <p className="muted">
                Any object's YAML with secret values redacted. Edit it to get a diff and the kubectl commands that
                dry-run and apply it; Kubemap never applies it for you.
              </p>
            </div>
            <a className="button" href={`#/clusters/${encodeURIComponent(c.id)}/yaml`}>
              Show YAML
            </a>
          </div>
          <div className="card maplink">
            <div>
              <h2>Pods and logs</h2>
              <p className="muted">Every pod with its status and restarts. Open one to read or follow its logs.</p>
            </div>
            <a className="button" href={`#/clusters/${encodeURIComponent(c.id)}/pods`}>
              Show pods
            </a>
          </div>
          <div className="card maplink">
            <div>
              <h2>Applications</h2>
              <p className="muted">
                Each app with its entry points, frontends, backends, databases and caches, the pods behind them, and how
                they connect.
              </p>
            </div>
            <a className="button primary" href={`#/clusters/${encodeURIComponent(c.id)}/apps`}>
              Open the application map
            </a>
          </div>
          </div>
          <NodesSection clusterId={c.id} />
                    <div className="card">
            <h2>Details</h2>
            <Facts c={c} />
                      </div>
        </>
      )}
    </section>
  );
}

function Facts({ c }: { c: Cluster }) {
  const rows: [string, string][] = [
    ["Kubernetes", c.kubernetesVersion || "–"],
    ["Nodes", String(c.nodeCount)],
    ["Agent version", c.agentVersion || "–"],
    ["Created", new Date(c.createdAt).toLocaleString()],
    ["Cluster ID", c.id],
  ];
  return (
    <dl className="facts">
      {rows.map(([k, v]) => (
        <div key={k} className="fact">
          <dt>{k}</dt>
          <dd>{v}</dd>
        </div>
      ))}
    </dl>
  );
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div className="card stat">
      <div className="muted small">{label}</div>
      <div className="value">{value}</div>
    </div>
  );
}

function NodesSection({ clusterId }: { clusterId: string }) {
  const { data: nodes, error } = usePoll(() => nodeApi.nodes(clusterId), 10000);
  return (
    <div className="card flush">
      <h2 className="pad">
        Nodes <a className="small" href="#/servers">all servers</a>
      </h2>
      {error != null && <p className="pad error">{errorMessage(error)}</p>}
      {nodes === undefined ? (
        <p className="pad muted">Loading…</p>
      ) : nodes.length === 0 ? (
        <p className="pad muted">No node agent has reported yet. Each node's agent connects by itself; check that the kubemap-node-agent DaemonSet runs on every node.</p>
      ) : (
        <table className="data clickable">
          <thead>
            <tr>
              <th>Node</th>
              <th>Agent</th>
              <th>CPU</th>
              <th>Memory</th>
              <th>Disk</th>
              <th className="num">Memory pressure</th>
              <th>Last seen</th>
            </tr>
          </thead>
          <tbody>
            {nodes.map((n) => {
              const href = `#/clusters/${encodeURIComponent(clusterId)}/nodes/${encodeURIComponent(n.name)}`;
              const p = n.current?.point;
              const agentless = n.status === "none";
              return (
                <tr key={n.name} onClick={agentless ? undefined : () => (window.location.hash = href)}>
                  <td>
                    {agentless ? (
                      <span className="strong">{n.name}</span>
                    ) : (
                      <a href={href} className="strong">
                        {n.name}
                      </a>
                    )}
                    {n.state && n.state.severity !== "ok" && <div className="small reason">{n.state.detail}</div>}
                  </td>
                  <td>
                    <NodeStatus n={n} />
                  </td>
                  <td>{p ? <Meter value={p.cpu} label={`CPU on ${n.name}`} /> : <span className="muted">–</span>}</td>
                  <td>{p ? <Meter value={p.memory} label={`Memory on ${n.name}`} /> : <span className="muted">–</span>}</td>
                  <td>{p ? <Meter value={p.disk} label={`Disk on ${n.name}`} /> : <span className="muted">–</span>}</td>
                  <td className="num">{p && n.current?.hasPsi ? percent(p.psiMemory) : <span className="muted">–</span>}</td>
                  <td>{agentless ? <span className="muted small">no node agent</span> : relativeTime(n.lastSeen)}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </div>
  );
}
