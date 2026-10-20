import { observeApi } from "./api";
import { errorMessage, usePoll } from "./common";
import { formatDuration, timelineRows } from "./observe";

const SERIES = ["series-1", "series-2", "series-3"];

/** The spans of one trace as bars on a shared time axis: where the time went. */
export function TraceTimeline({ id }: { id: string }) {
  const t = usePoll(() => observeApi.trace(id), 600000, id);
  if (t.error != null) return <p className="error">{errorMessage(t.error)}</p>;
  if (!t.data) return <p className="muted small">Loading the spans…</p>;
  const rows = timelineRows(t.data);
  if (rows.length === 0) return <p className="muted small">Tempo has no spans for this trace yet.</p>;

  const total = Math.max(1, ...rows.map((r) => r.startUs + r.durationUs));
  const services = [...new Set(rows.map((r) => r.service))];
  const colour = (s: string) => `var(--${SERIES[services.indexOf(s) % SERIES.length]})`;
  const ticks = [0, 25, 50, 75, 100];
  const skew = Math.max(0, ...rows.map((r) => Math.abs(r.skewUs)));

  return (
    <div className="timeline" role="table" aria-label="Spans of the trace">
      <p className="small muted tlhelp">
        One trace is one request on its way through the app. Each block below is one step (a span). An indented step was
        called by the step above it. The bar shows <strong>when</strong> the step started (position) and{" "}
        <strong>how long</strong> it took (width), on one shared time axis.
      </p>
      {skew >= 1000 && (
        <p className="small tlskew">
          The clocks of two services differ by about <strong>{formatDuration(skew)}</strong> (usually the visitor's browser against the
          server). A step from the other clock was moved to the middle of the step that called it so the picture is readable. Each
          length is real; only the start position is an estimate.
        </p>
      )}
      <div className="tlsummary small">
        <span>
          Total <strong>{formatDuration(total)}</strong> · {rows.length} {rows.length === 1 ? "span" : "spans"}
        </span>
        {services.map((s) => (
          <span key={s} className="tlchip">
            <i style={{ background: colour(s) }} />
            {s}
          </span>
        ))}
        <span className="tlchip">
          <i className="bad" />
          failed
        </span>
      </div>
      <div className="tlaxis small muted" aria-hidden="true">
        {ticks.map((p) => (
          <span key={p} style={{ left: `${p}%` }} className={p === 100 ? "last" : p === 0 ? "first" : ""}>
            {formatDuration((total * p) / 100)}
          </span>
        ))}
      </div>
      {rows.map((r, i) => (
        <div className="tlspan" role="row" key={r.id}>
          <div className="tlhead" style={{ paddingLeft: `${r.depth * 1.25}rem` }}>
            {r.depth > 0 && <span className="tlarrow muted" aria-hidden="true">↳</span>}
            <span className="tlno">{i + 1}</span>
            <span className="tldot" style={{ background: r.error ? "var(--bad)" : colour(r.service) }} />
            <span className="tlsvc">{r.service}</span>
            <span className="tlname mono">{r.name}</span>
            <span className="tldur num">{formatDuration(r.durationUs)}</span>
          </div>
          <div className="tlbar" style={{ ["--grid-pos" as string]: "25%" }}>
            <span
              className={`tlfill${r.error ? " bad" : ""}`}
              style={{ left: `${r.leftPct}%`, width: `${r.widthPct}%`, background: r.error ? undefined : colour(r.service) }}
            />
          </div>
          <div className="tlmeta small muted">
            {r.depth === 0 ? "Starts the trace" : `Starts at +${formatDuration(r.startUs)}`}{r.skewUs !== 0 && " (estimated)"} · lasts {formatDuration(r.durationUs)}
            {r.depth > 0 && ` · ${Math.round((r.durationUs / total) * 100)}% of the trace`} · by {r.scope}
          </div>
          {r.detail && <div className="tldetail mono small">{r.detail}</div>}
        </div>
      ))}
    </div>
  );
}
