import { useCallback, useState } from "react";
import { ApiError, bytes, nodeApi, percent, relativeTime, type NodeDetail, type NodeSummary, type Range } from "./api";
import { errorMessage, usePoll } from "./common";
import { LineChart } from "./LineChart";

const RANGES: { id: Range; label: string }[] = [
  { id: "1h", label: "Last hour" },
  { id: "6h", label: "Last 6 hours" },
  { id: "24h", label: "Last 24 hours" },
];

/** A small bar whose fill says how full something is: accent, then warning, then critical. */
export function Meter({ value, label }: { value: number; label: string }) {
  const level = value >= 90 ? "critical" : value >= 75 ? "warning" : "ok";
  return (
    <span className="meter-wrap">
      <span className="meter" role="meter" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(value)} aria-label={label}>
        <span className={`fill ${level}`} style={{ width: `${Math.max(2, Math.min(100, value))}%` }} />
      </span>
      <span className="meter-value">{percent(value)}</span>
    </span>
  );
}

export function NodeStatus({ n }: { n: NodeSummary }) {
  if (n.state) {
    return (
      <span className={`badge level-${n.state.severity}`} title={n.state.detail}>
        <span className="dot" aria-hidden="true" />
        {n.state.title}
      </span>
    );
  }
  return n.connected ? (
    <span className="badge active">
      <span className="dot" aria-hidden="true" />
      Reporting
    </span>
  ) : (
    <span className="badge disconnected">
      <span className="dot" aria-hidden="true" />
      Not reporting
    </span>
  );
}

export function NodePage({ clusterId, node }: { clusterId: string; node: string }) {
  const [range, setRange] = useState<Range>("1h");
  const load = useCallback(() => nodeApi.node(clusterId, node, range), [clusterId, node, range]);
  const { data, error } = usePoll<NodeDetail>(load, 10000, range);
  // Keep showing the previous range, dimmed, while the new one loads.
  const stale = data !== undefined && data.range !== range;
  const back = `#/clusters/${encodeURIComponent(clusterId)}`;

  if (error instanceof ApiError && error.status === 404) {
    return (
      <section>
        <p>
          <a href={back}>← Cluster</a>
        </p>
        <div className="card empty">
          <h2>Node not found</h2>
          <p className="muted">No node agent has reported a node with this name.</p>
        </div>
      </section>
    );
  }
  if (!data) return <p className="muted">{error ? errorMessage(error) : "Loading…"}</p>;

  const times = data.series.map((p) => new Date(p.t).getTime());
  const from = new Date(data.from).getTime();
  const to = new Date(data.to).getTime();
  const stepMs = data.step * 1000;
  const col = (k: Exclude<keyof NodeDetail["series"][number], "t">) => data.series.map((p) => p[k]);
  const latest = data.latest;
  // No PSI anywhere in the range and none now: the kernel does not report
  // it. Say so instead of drawing flat 0 % lines (review KM-30 B1).
  const hasPsi = latest?.hasPsi || data.series.some((p) => p.psiCpu !== null);
  const m = latest?.metrics;
  const pct = (v: number) => percent(v);

  return (
    <section>
      <p>
        <a href={back}>← Cluster</a>
      </p>
      <div className="pagehead">
        <h1>{data.node.name}</h1>
        <NodeStatus n={data.node} />
        <span className="muted small">last heard from {relativeTime(data.node.lastSeen)}</span>
      </div>
      {data.node.state && data.node.state.severity !== "ok" && (
        <p className={`notice ${data.node.state.severity === "fail" ? "error" : "attention"}`}>{data.node.state.detail}</p>
      )}

      <div className="filters" role="group" aria-label="Time range">
        {RANGES.map((r) => (
          <button key={r.id} className={r.id === range ? "chip selected" : "chip"} aria-pressed={r.id === range} onClick={() => setRange(r.id)}>
            {r.label}
          </button>
        ))}
      </div>

      {latest && m ? (
        <div className="grid kpis">
          <Tile label="CPU" value={pct(latest.point.cpu)} note={`${m.cpu.cores} cores`} />
          <Tile label="Memory used" value={pct(latest.point.memory)} note={`of ${bytes(m.memory.totalBytes)}`} />
          <Tile label="Swap used" value={m.swap.totalBytes ? pct(latest.point.swap) : "–"} note={m.swap.totalBytes ? `of ${bytes(m.swap.totalBytes)}` : "no swap"} />
          <Tile label="Disk used" value={pct(latest.point.disk)} note={latest.diskMountPoint ? `on ${latest.diskMountPoint}` : undefined} />
          <Tile label="Load (1 min)" value={m.load.load1.toFixed(2)} note={`${m.cpu.cores} cores`} />
          <Tile
            label="Memory pressure"
            value={latest.hasPsi ? pct(latest.point.psiMemory) : "–"}
            note={latest.hasPsi ? "of time tasks waited" : "not reported by this kernel"}
          />
        </div>
      ) : (
        <div className="notice attention">No live numbers yet: the node agent has not sent metrics since the portal started.</div>
      )}

      <div className="charts">
        <LineChart title="CPU used" subtitle="%" times={times} from={from} to={to} stepMs={stepMs} yMax={100} format={pct} dimmed={stale}
          series={[{ name: "CPU", color: "--series-1", values: col("cpu") }]} />
        <LineChart title="Memory used" subtitle="%" times={times} from={from} to={to} stepMs={stepMs} yMax={100} format={pct} dimmed={stale}
          series={[{ name: "Memory", color: "--series-1", values: col("memory") }]} />
        <LineChart title="Swap used" subtitle="%" times={times} from={from} to={to} stepMs={stepMs} yMax={100} format={pct} dimmed={stale}
          series={[{ name: "Swap", color: "--series-1", values: col("swap") }]} />
        <LineChart title="Disk used" subtitle={latest?.diskMountPoint ? `% of ${latest.diskMountPoint}` : "%"} times={times} from={from} to={to} stepMs={stepMs} yMax={100} format={pct} dimmed={stale}
          series={[{ name: "Disk", color: "--series-1", values: col("disk") }]} />
        {hasPsi ? (
          <LineChart
            title="Pressure stall"
            subtitle="% of time some tasks waited (10 s average)"
            times={times}
            from={from}
            to={to}
            stepMs={stepMs}
            format={pct}
            dimmed={stale}
            series={[
              { name: "CPU", color: "--series-1", values: col("psiCpu") },
              { name: "Memory", color: "--series-2", values: col("psiMemory") },
              { name: "I/O", color: "--series-3", values: col("psiIo") },
            ]}
          />
        ) : (
          <div className="card chart chart-empty">
            <div>
              <div className="chart-title">Pressure stall</div>
              <p className="muted">Not reported by this kernel (no PSI). Pressure cannot be shown for this node.</p>
            </div>
          </div>
        )}
      </div>

      {m?.filesystems && m.filesystems.length > 0 && (
        <div className="card flush">
          <h2 className="pad">Filesystems</h2>
          <table className="data">
            <thead>
              <tr>
                <th>Mount point</th>
                <th>Type</th>
                <th>Space used</th>
                <th className="num">Free</th>
                <th>Inodes used</th>
              </tr>
            </thead>
            <tbody>
              {m.filesystems.map((f) => {
                const used = f.totalBytes ? ((f.totalBytes - f.availableBytes) / f.totalBytes) * 100 : 0;
                const iused = f.inodesTotal ? ((f.inodesTotal - f.inodesFree) / f.inodesTotal) * 100 : 0;
                return (
                  <tr key={f.mountPoint}>
                    <td>
                      <code>{f.mountPoint}</code> <span className="muted small">{f.device}</span>
                    </td>
                    <td>{f.fsType}</td>
                    <td>
                      <Meter value={used} label={`Space used on ${f.mountPoint}`} />
                    </td>
                    <td className="num">{bytes(f.availableBytes)}</td>
                    <td>{f.inodesTotal ? <Meter value={iused} label={`Inodes used on ${f.mountPoint}`} /> : "–"}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      {m?.topProcesses && m.topProcesses.length > 0 && (
        <div className="card flush">
          <h2 className="pad">Busiest processes</h2>
          <table className="data">
            <thead>
              <tr>
                <th className="num">PID</th>
                <th>Command</th>
                <th>Pod</th>
                <th className="num">CPU</th>
                <th className="num">Memory</th>
              </tr>
            </thead>
            <tbody>
              {m.topProcesses.map((p) => (
                <tr key={p.pid}>
                  <td className="num">{p.pid}</td>
                  <td>
                    <code>{p.command}</code>
                  </td>
                  <td>
                    {p.podName ? (
                      <>
                        {p.podNamespace}/{p.podName}
                        {p.container && <span className="muted small"> · {p.container}</span>}
                      </>
                    ) : (
                      <span className="muted">host</span>
                    )}
                  </td>
                  <td className="num">{percent(p.cpuPercent)}</td>
                  <td className="num">{bytes(p.rssBytes)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

function Tile({ label, value, note }: { label: string; value: string; note?: string }) {
  return (
    <div className="card stat">
      <div className="muted small">{label}</div>
      <div className="value">{value}</div>
      {note && <div className="muted small">{note}</div>}
    </div>
  );
}
