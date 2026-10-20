import { useEffect, useId, useMemo, useRef, useState, type KeyboardEvent, type PointerEvent } from "react";

export interface Series {
  name: string;
  /** A CSS custom property holding the series color, for example "--series-1". */
  color: string;
  values: (number | null)[];
}

interface Props {
  title: string;
  /** Shown after the title, for example "% of 4 cores". */
  subtitle?: string;
  times: number[]; // ms, ascending, one per value
  series: Series[];
  from: number; // ms
  to: number; // ms
  stepMs: number;
  /** Fixed top of the y axis (100 for percentages); otherwise a nice round max. */
  yMax?: number;
  format: (v: number) => string;
  dimmed?: boolean;
}

const PLOT_H = 150;
const AXIS_H = 22;
const TOP = 10;
const LEFT = 52;

function niceMax(v: number): number {
  for (const m of [1, 2, 5, 10, 20, 25, 50, 100]) if (v <= m) return m;
  return Math.ceil(v / 100) * 100;
}

function hhmm(ms: number): string {
  return new Date(ms).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

/**
 * A time-series line chart in plain SVG: 2px lines, hairline grid, a
 * crosshair that snaps to the nearest step with one tooltip for every
 * series, keyboard support, and a table view. One series gets an area wash
 * and its latest value at the end; two or more get a legend.
 */
export function LineChart({ title, subtitle, times, series, from, to, stepMs, yMax, format, dimmed }: Props) {
  const wrapRef = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(600);
  const [hover, setHover] = useState<number | null>(null);
  const tableId = useId();

  useEffect(() => {
    const el = wrapRef.current;
    if (!el) return;
    const ro = new ResizeObserver(([e]) => setWidth(Math.max(240, Math.floor(e.contentRect.width))));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  const single = series.length === 1;
  const right = single ? 52 : 12;
  const plotW = width - LEFT - right;
  const top = useMemo(() => {
    if (yMax !== undefined) return yMax;
    let m = 0;
    for (const s of series) for (const v of s.values) if (v !== null && v > m) m = v;
    return niceMax(m);
  }, [series, yMax]);

  const x = (t: number) => LEFT + ((t - from) / Math.max(1, to - from)) * plotW;
  const y = (v: number) => TOP + PLOT_H - (Math.min(v, top) / top) * PLOT_H;
  const ticks = [0, 0.25, 0.5, 0.75, 1].map((f) => f * top);
  const xTicks = useMemo(() => {
    const n = Math.max(2, Math.min(6, Math.floor(plotW / 110)));
    return Array.from({ length: n + 1 }, (_, i) => from + ((to - from) * i) / n);
  }, [from, to, plotW]);

  // Lines break where the node sent nothing for more than 1.5 steps.
  function path(values: (number | null)[], area: boolean): string {
    let d = "";
    let run: [number, number][] = [];
    const flush = () => {
      if (run.length === 0) return;
      const line = run.map(([px, py], i) => `${i ? "L" : "M"}${px.toFixed(1)},${py.toFixed(1)}`).join("");
      d += area
        ? `${line}L${run[run.length - 1][0].toFixed(1)},${TOP + PLOT_H}L${run[0][0].toFixed(1)},${TOP + PLOT_H}Z`
        : line;
      run = [];
    };
    values.forEach((v, i) => {
      if (v === null || (i > 0 && times[i] - times[i - 1] > stepMs * 1.5)) flush();
      if (v !== null) run.push([x(times[i]), y(v)]);
    });
    flush();
    return d;
  }

  function nearest(px: number): number | null {
    if (times.length === 0) return null;
    let best = 0;
    for (let i = 1; i < times.length; i++) if (Math.abs(x(times[i]) - px) < Math.abs(x(times[best]) - px)) best = i;
    return best;
  }

  const onMove = (e: PointerEvent<SVGSVGElement>) => {
    const r = e.currentTarget.getBoundingClientRect();
    setHover(nearest(e.clientX - r.left));
  };
  const onKey = (e: KeyboardEvent<SVGSVGElement>) => {
    if (times.length === 0) return;
    if (e.key === "ArrowLeft" || e.key === "ArrowRight") {
      e.preventDefault();
      const d = e.key === "ArrowLeft" ? -1 : 1;
      setHover((h) => Math.min(times.length - 1, Math.max(0, (h ?? times.length - 1) + d)));
    } else if (e.key === "Escape") setHover(null);
  };

  const last = single ? [...series[0].values].reverse().find((v) => v !== null) : undefined;
  const lastIdx = single ? series[0].values.length - 1 - [...series[0].values].reverse().findIndex((v) => v !== null) : -1;
  const empty = times.length === 0;
  const tipLeft = hover !== null ? x(times[hover]) : 0;

  return (
    <figure className={`chart card${dimmed ? " dimmed" : ""}`}>
      <figcaption>
        <span className="chart-title">{title}</span>
        {subtitle && <span className="muted small"> · {subtitle}</span>}
      </figcaption>
      {!single && (
        <ul className="legend" aria-label="Legend">
          {series.map((s) => (
            <li key={s.name}>
              <span className="key" style={{ background: `var(${s.color})` }} aria-hidden="true" />
              {s.name}
            </li>
          ))}
        </ul>
      )}
      <div className="plot" ref={wrapRef}>
        {empty ? (
          <div className="chart-empty muted small" style={{ height: PLOT_H + AXIS_H + TOP }}>
            No data in this range yet.
          </div>
        ) : (
          <svg
            width={width}
            height={TOP + PLOT_H + AXIS_H}
            role="img"
            aria-label={`${title}. Use the table below for the values.`}
            tabIndex={0}
            onPointerMove={onMove}
            onPointerLeave={() => setHover(null)}
            onKeyDown={onKey}
            onBlur={() => setHover(null)}
          >
            {ticks.map((t) => (
              <g key={t}>
                <line className="grid" x1={LEFT} x2={LEFT + plotW} y1={y(t)} y2={y(t)} />
                <text className="axis" x={LEFT - 6} y={y(t)} dy="0.32em" textAnchor="end">
                  {format(t)}
                </text>
              </g>
            ))}
            {xTicks.map((t, i) => (
              <text
                key={t}
                className="axis"
                x={x(t)}
                y={TOP + PLOT_H + 16}
                textAnchor={i === 0 ? "start" : i === xTicks.length - 1 ? "end" : "middle"}
              >
                {hhmm(t)}
              </text>
            ))}
            {single && <path d={path(series[0].values, true)} className="area" style={{ fill: `var(${series[0].color})` }} />}
            {series.map((s) => (
              <path key={s.name} d={path(s.values, false)} className="line" style={{ stroke: `var(${s.color})` }} />
            ))}
            {single && last !== undefined && lastIdx >= 0 && (
              <text className="endlabel" x={x(times[lastIdx]) + 8} y={y(last)} dy="0.32em">
                {format(last)}
              </text>
            )}
            {hover !== null && (
              <g>
                <line className="crosshair" x1={tipLeft} x2={tipLeft} y1={TOP} y2={TOP + PLOT_H} />
                {series.map((s) => {
                  const v = s.values[hover];
                  return v === null ? null : (
                    <circle key={s.name} cx={tipLeft} cy={y(v)} r={4} className="dot" style={{ fill: `var(${s.color})` }} />
                  );
                })}
              </g>
            )}
          </svg>
        )}
        {hover !== null && !empty && (
          <div
            className="tooltip"
            style={{ left: Math.min(Math.max(tipLeft, 80), width - 80), top: TOP }}
            role="status"
            aria-live="polite"
          >
            <div className="muted small">{new Date(times[hover]).toLocaleString()}</div>
            {series.map((s) => (
              <div key={s.name} className="tip-row">
                <span className="linekey" style={{ background: `var(${s.color})` }} aria-hidden="true" />
                <strong>{s.values[hover] === null ? "–" : format(s.values[hover]!)}</strong>
                {!single && <span className="muted small">{s.name}</span>}
              </div>
            ))}
          </div>
        )}
      </div>
      {!empty && (
        <details className="tableview">
          <summary aria-controls={tableId}>Table</summary>
          <div className="tablescroll" id={tableId}>
            <table>
              <thead>
                <tr>
                  <th>Time</th>
                  {series.map((s) => (
                    <th key={s.name} className="num">
                      {s.name}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {times.map((t, i) => (
                  <tr key={t}>
                    <td>{new Date(t).toLocaleString()}</td>
                    {series.map((s) => (
                      <td key={s.name} className="num">
                        {s.values[i] === null ? "–" : format(s.values[i]!)}
                      </td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </details>
      )}
    </figure>
  );
}
