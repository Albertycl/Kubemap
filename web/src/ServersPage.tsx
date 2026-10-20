import { Fragment, useState } from "react";
import { percent, relativeTime, serverApi, type ResourcePressure } from "./api";
import { errorMessage, usePoll } from "./common";
import { Meter } from "./NodePage";
import { serverLevel, sortServers } from "./servers";

const PRESSURE_ERROR: Record<string, string> = {
  agent_offline: "cluster agent not connected: no pressure view",
  unsupported: "cluster agent too old for the pressure view",
};

/**
 * All-servers overview (KM-66): every node of every cluster, worst first,
 * with its numbers from the node agent and the cluster agent's pressure
 * view (kubelet conditions plus kernel PSI). #/servers
 */
export function ServersPage() {
  const { data, error } = usePoll(() => serverApi.all(), 10000);
  const [problems, setProblems] = useState(false);
  const [cluster, setCluster] = useState("");
  const [open, setOpen] = useState<string | null>(null);

  const all = data ? sortServers(data.servers) : [];
  const clusters = [...new Map(all.map((s) => [s.clusterId, s.clusterName])).entries()].sort((a, b) => a[1].localeCompare(b[1]));
  const list = all.filter((s) => (!cluster || s.clusterId === cluster) && (!problems || serverLevel(s).level !== "ok"));
  const count = (l: string) => all.filter((s) => serverLevel(s).level === l).length;

  return (
    <section>
      <p>
        <a href="#/">← Clusters</a>
      </p>
      <div className="pagehead">
        <h1>Servers</h1>
        {data && <span className="muted small">updated {relativeTime(data.at)}</span>}
      </div>
      <p className="muted lead">
        Every node of every cluster. The numbers come from each node's own agent; the pressure view comes from the cluster
        agent, which combines the kubelet's conditions with the kernel's pressure stall information (PSI). Kubernetes
        alone reports no CPU or I/O pressure, and its memory and disk conditions only flip at the eviction threshold.
      </p>
      {error != null && <div className="notice error">{errorMessage(error)}{data ? " Showing the last list." : ""}</div>}
      {!data ? (
        !error && <p className="muted">Loading…</p>
      ) : (
        <>
          <div className="grid kpis">
            <Kpi label="Servers" value={all.length} />
            <Kpi label="Failing" value={count("fail")} bad />
            <Kpi label="Need attention" value={count("warn")} warn />
            <Kpi label="Healthy" value={count("ok")} />
          </div>
          <div className="filters">
            {clusters.length > 1 && (
              <label className="inline">
                Cluster
                <select value={cluster} onChange={(e) => setCluster(e.target.value)}>
                  <option value="">All clusters</option>
                  {clusters.map(([id, name]) => (
                    <option key={id} value={id}>
                      {name}
                    </option>
                  ))}
                </select>
              </label>
            )}
            <label className="toggle">
              <input type="checkbox" checked={problems} onChange={(e) => setProblems(e.target.checked)} />
              Only servers with problems
            </label>
          </div>
          {Object.entries(data.pressureErrors ?? {}).map(([id, e]) => (
            <div key={id} className="notice attention small">
              {all.find((s) => s.clusterId === id)?.clusterName ?? id}: {PRESSURE_ERROR[e.error] ?? e.message}
            </div>
          ))}
          {list.length === 0 ? (
            <div className="card empty">
              <h2>No servers</h2>
              <p className="muted">
                {all.length === 0 ? "No node agent has reported yet, and no cluster agent answered." : "Nothing matches the filters."}
              </p>
            </div>
          ) : (
            <div className="card flush">
              <table className="data servers">
                <thead>
                  <tr>
                    <th>Server</th>
                    <th>State</th>
                    <th>CPU</th>
                    <th>Memory</th>
                    <th>Disk</th>
                    <th className="num">Swap</th>
                    <th className="num">Load</th>
                    <th className="num">
                      <abbr title="Share of time tasks waited (PSI some, 10 s)">Waiting CPU / mem / IO</abbr>
                    </th>
                    <th>Last seen</th>
                  </tr>
                </thead>
                <tbody>
                  {list.map((s) => {
                    const key = s.clusterId + "/" + s.name;
                    const { level, reason } = serverLevel(s);
                    const p = s.current?.point;
                    const href = `#/clusters/${encodeURIComponent(s.clusterId)}/nodes/${encodeURIComponent(s.name)}`;
                    const isOpen = open === key;
                    return (
                      <Fragment key={key}>
                        <tr className={`level-${level} ${s.stale ? "stale" : ""}`}>
                          <td>
                            {s.status === "none" ? (
                              <span className="strong">{s.name}</span>
                            ) : (
                              <a href={href} className="strong">
                                {s.name}
                              </a>
                            )}
                            <div className="muted small">{s.clusterName}</div>
                          </td>
                          <td>
                            <span className={`badge level-${level}`}>
                              <span className="dot" aria-hidden="true" />
                              {level === "ok" ? "Healthy" : level === "warn" ? "Attention" : "Failing"}
                            </span>
                            {reason && <div className="small reason">{reason}</div>}
                            {s.pressure && (
                              <button className="link small" onClick={() => setOpen(isOpen ? null : key)} aria-expanded={isOpen}>
                                {isOpen ? "Hide pressure" : "Pressure details"}
                              </button>
                            )}
                          </td>
                          <td>{p ? <Meter value={p.cpu} label={`CPU on ${s.name}`} /> : <span className="muted">–</span>}</td>
                          <td>{p ? <Meter value={p.memory} label={`Memory on ${s.name}`} /> : <span className="muted">–</span>}</td>
                          <td>{p ? <Meter value={p.disk} label={`Disk on ${s.name}`} /> : <span className="muted">–</span>}</td>
                          <td className="num">{p ? percent(p.swap) : "–"}</td>
                          <td className="num">
                            {s.current ? (
                              <span title={`load average 1 min on ${s.current.cores} cores`}>
                                {s.current.load1.toFixed(1)} / {s.current.cores}
                              </span>
                            ) : (
                              "–"
                            )}
                          </td>
                          <td className="num">
                            {p && s.current?.hasPsi ? `${percent(p.psiCpu)} / ${percent(p.psiMemory)} / ${percent(p.psiIo)}` : "–"}
                          </td>
                          <td>
                            {s.status === "none" ? (
                              <span className="muted small">no node agent</span>
                            ) : (
                              <>
                                {relativeTime(s.lastSeen)}
                                {s.stale && <div className="small warn-text">numbers as of {relativeTime(s.current?.at)}</div>}
                              </>
                            )}
                          </td>
                        </tr>
                        {isOpen && s.pressure && (
                          <tr className="pressure-row">
                            <td colSpan={9}>
                              <PressureDetail r={s.pressure.resources} psi={s.pressure.psiAvailable} />
                            </td>
                          </tr>
                        )}
                      </Fragment>
                    );
                  })}
                </tbody>
              </table>
            </div>
          )}
        </>
      )}
    </section>
  );
}

function Kpi({ label, value, bad, warn }: { label: string; value: number; bad?: boolean; warn?: boolean }) {
  return (
    <div className="card stat">
      <div className="muted small">{label}</div>
      <div className={`value ${bad && value > 0 ? "error" : warn && value > 0 ? "warn-text" : ""}`}>{value}</div>
    </div>
  );
}

const VERDICT_LEVEL: Record<string, string> = { ok: "ok", elevated: "warn", pressure: "fail", unknown: "info" };

// The PSI thresholds of the verdicts (pkg/protocol PSISomeElevated and
// PSISomePressure), shown so an SRE can judge a value near them (review km-60 N-4).
const PSI_ELEVATED = 10;
const PSI_PRESSURE = 25;

function PressureDetail({ r, psi }: { r: ResourcePressure[]; psi: boolean }) {
  return (
    <div className="pressure-detail">
      {!psi && (
        <p className="small muted">
          The node agent reports no PSI for this node (kernel without PSI, or no recent report), so CPU and I/O pressure
          are unknown.
        </p>
      )}
      <p className="small muted">
        PSI verdicts use the share of time some task waited (avg60): elevated from {PSI_ELEVATED} %, pressure from{" "}
        {PSI_PRESSURE} %.
      </p>
      <table className="data compact">
        <thead>
          <tr>
            <th>Resource</th>
            <th>Verdict</th>
            <th>Kubelet condition</th>
            <th>Evidence</th>
          </tr>
        </thead>
        <tbody>
          {r.map((x) => (
            <tr key={x.resource}>
              <td className="strong">{x.resource}</td>
              <td>
                <span className={`badge level-${VERDICT_LEVEL[x.verdict] ?? "info"}`}>{x.verdict}</span>
              </td>
              <td>{x.condition ? `${x.condition}=${x.conditionStatus || "–"}` : <span className="muted">none for {x.resource}</span>}</td>
              <td>{x.detail}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
