import { useState } from "react";
import { observeApi, type MapComponent } from "./api";
import { errorMessage, usePoll } from "./common";
import { LineChart } from "./LineChart";
import { TraceTimeline } from "./TraceTimeline";
import {
  budgetLevel,
  burnLevel,
  debugSteps,
  DEFAULT_SLO,
  errorShare,
  grafanaTracesUrl,
  grafanaWorkloadUrl,
  hoursText,
  lastValue,
  parseCustomRange,
  percentText,
  presetRange,
  safeBase,
  SLO_TARGETS,
  toLocalInput,
  type Level,
  type RangeChoice,
} from "./observe";

const TRACE_ROWS = 20;
const num = (v: number) => (v < 10 ? v.toFixed(2) : v.toFixed(0));
const bytes = (v: number) => (v >= 1 << 30 ? `${(v / (1 << 30)).toFixed(1)}G` : `${Math.round(v / (1 << 20))}M`);
const rate = (v: number) => `${num(v)}/s`;
const ms = (v: number) =>
  v < 0.01 ? `${(v * 1000).toFixed(1)} ms` : v < 1 ? `${(v * 1000).toFixed(0)} ms` : `${v.toFixed(2)} s`;

const loadSlo = () => {
  try {
    const v = Number(localStorage.getItem("kubemap.slo"));
    return SLO_TARGETS.includes(v) ? v : DEFAULT_SLO;
  } catch {
    return DEFAULT_SLO;
  }
};

function Tile({ label, value, sub, level }: { label: string; value: string; sub?: string; level: Level }) {
  return (
    <div className={`slotile ${level}`}>
      <span className="small muted">{label}</span>
      <strong>{value}</strong>
      {sub && <span className="small muted">{sub}</span>}
    </div>
  );
}

/** Metrics, traces and the debug order for one workload (Prometheus and Tempo, read-only). */
export function ObservePanel({ c }: { c: MapComponent }) {
  const [open, setOpen] = useState<string | null>(null);
  const [choice, setChoice] = useState<RangeChoice>("1h");
  const [custom, setCustom] = useState<{ from: number; to: number } | undefined>(undefined);
  const [startText, setStartText] = useState(() => toLocalInput(Date.now() - 3600000));
  const [endText, setEndText] = useState(() => toLocalInput(Date.now()));
  const [rangeError, setRangeError] = useState("");
  const rangeNow = () => (choice === "custom" ? custom : presetRange(choice, Date.now()));
  const rangeKey = choice === "custom" ? `custom:${custom?.from}-${custom?.to}` : choice;
  const [slo, setSlo] = useState(loadSlo);
  const cfg = usePoll(() => observeApi.config(), 600000);
  const metricsOn = cfg.data?.metrics ?? false;
  const tracesOn = cfg.data?.traces ?? false;
  const m = usePoll(
    () =>
      metricsOn
        ? observeApi
            .workload(c.namespace, c.name, rangeNow(), slo)
            .then((d) => ({ k: `${c.id}|${rangeKey}|${slo}`, d }))
        : Promise.resolve(undefined),
    30000,
    `${c.id}|${metricsOn}|${rangeKey}|${slo}`,
  );
  const t = usePoll(
    () =>
      tracesOn
        ? observeApi.traces(c.namespace, c.name, rangeNow()).then((d) => ({ k: rangeKey, d }))
        : Promise.resolve(undefined),
    30000,
    `${c.id}|${tracesOn}|${rangeKey}`,
  );
  if (!cfg.data || (!metricsOn && !tracesOn)) return null;

  // Show an answer only for the range now selected, not the previous one.
  const d = m.data?.k === `${c.id}|${rangeKey}|${slo}` && m.error == null ? m.data.d : undefined;
  const td = t.data?.k === rangeKey && t.error == null ? t.data.d : undefined;
  const kind = c.kind.split(".")[0];
  const series = (s: { values: (number | null)[] }[] | null | undefined) => s?.[0]?.values;
  const restarts = series(d?.restarts)?.filter((v): v is number => v != null);
  const traces = td?.traces ?? [];
  const service = td?.service || d?.service || c.name;
  const steps = debugSteps({
    podsReady: (c.pods ?? []).filter((p) => p.ready).length,
    podsTotal: (c.pods ?? []).length,
    restartsDelta:
      restarts && restarts.length > 1 && restarts[restarts.length - 1] >= restarts[0]
        ? restarts[restarts.length - 1] - restarts[0]
        : undefined,
    errorRate: d?.requests ? (lastValue(series(d.errors)) ?? 0) : undefined,
    requestRate: lastValue(series(d?.requests)),
    failedTraces: traces.filter((x) => x.error).length,
    tracesKnown: td !== undefined,
  });
  const share = errorShare(series(d?.errors), series(d?.requests));
  const p95 = d?.latency?.find((x) => x.name === "p95")?.values;
  const s = d?.slo;
  const pickSlo = (v: number) => {
    setSlo(v);
    try {
      localStorage.setItem("kubemap.slo", String(v));
    } catch {
      /* the choice still applies until reload */
    }
  };
  const g = safeBase(cfg.data.grafanaUrl ?? "") ?? "";
  const fmt = (ms: number) =>
    new Date(ms).toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
  const rangeLabel =
    choice === "custom" && custom
      ? `${fmt(custom.from)} to ${fmt(custom.to)}`
      : choice === "6h"
        ? "last 6 hours"
        : choice === "24h"
          ? "last 24 hours"
          : "last hour";
  const apply = () => {
    const r = parseCustomRange(startText, endText, Date.now());
    if ("error" in r) {
      setRangeError(r.error);
      return;
    }
    setRangeError("");
    setCustom(r.range);
    setChoice("custom");
    setOpen(null);
  };
  const pick = (k: RangeChoice) => {
    setRangeError("");
    setStartText(toLocalInput(Date.now() - 3600000));
    setEndText(toLocalInput(Date.now()));
    setChoice(k);
    setOpen(null);
  };

  return (
    <div className="observe">
      <div className="detailhead">
        <h4>Monitor: {rangeLabel}</h4>
        <span className="spacer" />
        {g && (
          <>
            <a
              className="small"
              href={grafanaWorkloadUrl(g, c.namespace, c.name, kind)}
              target="_blank"
              rel="noreferrer"
            >
              Open metrics in Grafana
            </a>
            <a className="small" href={grafanaTracesUrl(g, service)} target="_blank" rel="noreferrer">
              Open traces in Grafana
            </a>
          </>
        )}
      </div>

      <div className="rangebar">
        <span className="small muted">Time range</span>
        {(["1h", "6h", "24h"] as const).map((k) => (
          <button
            key={k}
            type="button"
            className={choice === k ? "rangebtn on" : "rangebtn"}
            aria-pressed={choice === k}
            onClick={() => pick(k)}
          >
            {k === "1h" ? "Last hour" : k === "6h" ? "Last 6 hours" : "Last 24 hours"}
          </button>
        ))}
        <span className="small muted">or from</span>
        <input
          type="datetime-local"
          aria-label="Start time"
          value={startText}
          onChange={(e) => setStartText(e.target.value)}
        />
        <span className="small muted">to</span>
        <input
          type="datetime-local"
          aria-label="End time"
          value={endText}
          onChange={(e) => setEndText(e.target.value)}
        />
        <button type="button" className={choice === "custom" ? "rangebtn on" : "rangebtn"} onClick={apply}>
          Show
        </button>
      </div>
      <p className="small muted rangehelp">
        Times are in your browser's time zone. In this cluster, traces are kept for 24 hours and metrics for 7 days, so
        an older range shows metrics only.
      </p>
      {rangeError && <p className="error small">{rangeError}</p>}

      <ol className="debugsteps">
        {steps.map((s) => (
          <li key={s.title} className={`step ${s.level}`}>
            <span className="strong">{s.title}</span>{" "}
            <span className={s.level === "bad" ? "error" : "muted"}>{s.hint}</span>
          </li>
        ))}
      </ol>

      {m.error != null && <p className="error">{errorMessage(m.error)}</p>}
      {d && (
        <>
          <section className="layer layer-app" aria-label="Application layer">
            <div className="layerhead">
              <h4>Application layer</h4>
              <span className="muted small">what users feel: latency, errors, error budget (from traces)</span>
              <span className="spacer" />
              <label className="small muted">
                SLO target{" "}
                <select value={slo} onChange={(e) => pickSlo(Number(e.target.value))} aria-label="SLO target">
                  {SLO_TARGETS.map((t) => (
                    <option key={t} value={t}>
                      {t}%
                    </option>
                  ))}
                </select>
              </label>
            </div>
            {d.requests ? (
              <div className="slotiles">
                <Tile
                  label="Availability"
                  value={percentText(share == null ? null : 1 - share)}
                  sub="this time range"
                  level={share == null ? "none" : 1 - share >= slo / 100 ? "ok" : "bad"}
                />
                <Tile
                  label="Error rate"
                  value={percentText(share)}
                  sub="this time range"
                  level={share == null ? "none" : share === 0 ? "ok" : "warn"}
                />
                <Tile
                  label="Latency p95"
                  value={lastValue(p95) == null ? "no data" : ms(lastValue(p95)!)}
                  sub="latest point"
                  level="none"
                />
                <Tile
                  label="Burn rate, 1 hour"
                  value={s?.burn1h == null ? "no traffic" : `${s.burn1h.toFixed(1)}×`}
                  sub="1× = on pace for the SLO"
                  level={burnLevel(s?.burn1h)}
                />
                <Tile
                  label="Burn rate, 6 hours"
                  value={s?.burn6h == null ? "no traffic" : `${s.burn6h.toFixed(1)}×`}
                  sub="1× = on pace for the SLO"
                  level={burnLevel(s?.burn6h)}
                />
                <Tile
                  label={`Error budget left (${s?.windowDays ?? 7} days)`}
                  value={s?.remaining == null ? "no traffic" : `${Math.max(-999, s.remaining * 100).toFixed(1)}%`}
                  sub={
                    s?.exhaustHours != null
                      ? `gone in ${hoursText(s.exhaustHours)} at the 1 h burn rate`
                      : s?.remaining != null && s.remaining < 0
                        ? "overspent"
                        : "not burning now"
                  }
                  level={budgetLevel(s?.remaining)}
                />
              </div>
            ) : (
              <p className="muted small">No request metrics for this app yet, so no SLO can be shown.</p>
            )}
            <p className="muted small layerhelp">
              SLO {slo}% means at most {(100 - slo).toFixed(2).replace(/0+$/, "").replace(/\.$/, "")}% of requests may
              fail. Burn rate is how fast that allowance is used: 1× uses it up exactly at the end of{" "}
              {s?.windowDays ?? 7} days, 14.4× empties it in about 12 hours.
              {s?.requests != null &&
                ` Counted: ${Math.round(s.requests)} requests and ${Math.round(s.errors ?? 0)} failures in the ${s.windowDays} days before ${fmt(custom?.to ?? Date.now())}.`}
            </p>
            <div className="charts">
              {d.latency && d.latency.length > 0 && (
                <LineChart
                  title="Latency"
                  subtitle="p50, p95, p99 of requests served"
                  times={d.times}
                  from={d.from}
                  to={d.to}
                  stepMs={d.stepMs}
                  format={ms}
                  series={d.latency.map((x, i) => ({ name: x.name, color: `--series-${i + 1}`, values: x.values }))}
                />
              )}
              {d.requests && (
                <LineChart
                  title="Requests and errors"
                  subtitle="per second, from traces"
                  times={d.times}
                  from={d.from}
                  to={d.to}
                  stepMs={d.stepMs}
                  format={rate}
                  series={[
                    { name: "Requests", color: "--series-1", values: d.requests[0].values },
                    ...(d.errors ? [{ name: "Errors", color: "--series-2", values: d.errors[0].values }] : []),
                  ]}
                />
              )}
            </div>
          </section>

          <section className="layer layer-infra" aria-label="Infrastructure layer">
            <div className="layerhead">
              <h4>Infrastructure layer</h4>
              <span className="muted small">
                the pods that run it: {(c.pods ?? []).filter((p) => p.ready).length}/{(c.pods ?? []).length} ready
              </span>
            </div>
            <div className="charts">
              {d.cpu && (
                <LineChart
                  title="CPU"
                  subtitle="cores, all pods"
                  times={d.times}
                  from={d.from}
                  to={d.to}
                  stepMs={d.stepMs}
                  format={num}
                  series={[{ name: "CPU", color: "--series-1", values: d.cpu[0].values }]}
                />
              )}
              {d.memory && (
                <LineChart
                  title="Memory"
                  subtitle="working set, all pods, MiB (M) / GiB (G)"
                  times={d.times}
                  from={d.from}
                  to={d.to}
                  stepMs={d.stepMs}
                  format={bytes}
                  series={[{ name: "Memory", color: "--series-1", values: d.memory[0].values }]}
                />
              )}
              {d.restarts && (
                <LineChart
                  title="Restarts"
                  subtitle="total count"
                  times={d.times}
                  from={d.from}
                  to={d.to}
                  stepMs={d.stepMs}
                  format={num}
                  series={[{ name: "Restarts", color: "--series-2", values: d.restarts[0].values }]}
                />
              )}
            </div>
          </section>

          <section className="layer layer-db" aria-label="Database layer">
            <div className="layerhead">
              <h4>Database layer</h4>
              <span className="muted small">
                calls this app makes to {d.dbNames && d.dbNames.length > 0 ? d.dbNames.join(", ") : "a database"} (from
                traces)
              </span>
            </div>
            {d.dbRequests ? (
              <div className="charts">
                <LineChart
                  title="Database calls and errors"
                  subtitle="per second"
                  times={d.times}
                  from={d.from}
                  to={d.to}
                  stepMs={d.stepMs}
                  format={rate}
                  series={[
                    { name: "Calls", color: "--series-1", values: d.dbRequests[0].values },
                    ...(d.dbErrors ? [{ name: "Errors", color: "--series-2", values: d.dbErrors[0].values }] : []),
                  ]}
                />
                {d.dbLatency && (
                  <LineChart
                    title="Database latency"
                    subtitle="p95 of the calls"
                    times={d.times}
                    from={d.from}
                    to={d.to}
                    stepMs={d.stepMs}
                    format={ms}
                    series={[{ name: "p95", color: "--series-2", values: d.dbLatency[0].values }]}
                  />
                )}
              </div>
            ) : (
              <p className="muted small">No database calls seen in traces for this app in this time range.</p>
            )}
          </section>
        </>
      )}
      {d && d.notes.length > 0 && <p className="muted small">Not shown: {d.notes.join("; ")}.</p>}

      {tracesOn && (
        <section className="layer layer-traces" aria-label="Traces">
          <h4>
            Traces for {service}, {rangeLabel} (failed first, {Math.min(TRACE_ROWS, traces.length)} of {traces.length})
          </h4>
          {t.error != null ? (
            <p className="error">{errorMessage(t.error)}</p>
          ) : td === undefined ? (
            <p className="muted small">Loading traces…</p>
          ) : traces.length === 0 ? (
            <p className="muted small">
              No traces for {c.namespace}/{c.name} in this time range (traces are kept for 24 hours). The app must send
              OpenTelemetry traces whose resource has k8s.namespace.name and k8s.deployment.name (the OpenTelemetry
              Operator adds them).
            </p>
          ) : (
            <table className="data compact">
              <thead>
                <tr>
                  <th>Time</th>
                  <th>Request</th>
                  <th className="num">Duration</th>
                  <th>Result</th>
                </tr>
              </thead>
              <tbody>
                {traces.slice(0, TRACE_ROWS).map((x) => (
                  <tr
                    key={x.id}
                    className="clickrow"
                    tabIndex={0}
                    onKeyDown={(e) => e.key === "Enter" && setOpen(open === x.id ? null : x.id)}
                    onClick={() => setOpen(open === x.id ? null : x.id)}
                    aria-expanded={open === x.id}
                  >
                    <td>
                      {new Date(x.startMs).toLocaleString([], {
                        month: "short",
                        day: "numeric",
                        hour: "2-digit",
                        minute: "2-digit",
                        second: "2-digit",
                      })}
                    </td>
                    <td className="mono">
                      {x.rootService} {x.rootName}
                    </td>
                    <td className="num">{x.durationMs} ms</td>
                    <td className={x.error ? "error" : "muted"}>{x.error ? "failed" : "ok"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {open && (
            <>
              <h4>Where the time went (click the row again to close)</h4>
              <TraceTimeline id={open} />
            </>
          )}
        </section>
      )}
    </div>
  );
}
