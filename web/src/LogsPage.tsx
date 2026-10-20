import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { ApiError, debugHref, resourceApi, type LogQuery } from "./api";
import { errorMessage, usePoll } from "./common";
import { AnsiStripper, base64Bytes, LogBuffer, NdjsonSplitter } from "./logs";
import { podLevel } from "./PodsPage";

const TAILS = [100, 500, 2000, 10000];

type State =
  | { kind: "idle" }
  | { kind: "loading" }
  | { kind: "following" }
  | { kind: "ended"; text: string; error: boolean }
  | { kind: "error"; text: string };

function describe(e: unknown): string {
  if (e instanceof ApiError && e.code === "agent_offline") return "The cluster agent is not connected.";
  return errorMessage(e);
}

/** #/clusters/<id>/namespaces/<ns>/pods/<pod>/logs */
export function LogsPage({ clusterId, namespace, pod }: { clusterId: string; namespace: string; pod: string }) {
  const loadPods = useCallback(() => resourceApi.pods(clusterId, namespace), [clusterId, namespace]);
  const pods = usePoll(loadPods, 15000);
  const info = pods.data?.pods.find((p) => p.name === pod);
  const containers = info?.containers ?? [];

  const [container, setContainer] = useState("");
  const [previous, setPrevious] = useState(false);
  const [timestamps, setTimestamps] = useState(false);
  const [tailLines, setTailLines] = useState(500);
  const [follow, setFollow] = useState(false);
  const [wrap, setWrap] = useState(true);
  const [state, setState] = useState<State>({ kind: "idle" });
  const [truncated, setTruncated] = useState(false);
  const [reload, setReload] = useState(0);

  // The log text lives outside React state (it can be megabytes); a version
  // number, bumped at most once per frame, re-renders it.
  const buf = useRef(new LogBuffer());
  const [, setVersion] = useState(0);
  const frame = useRef(0);
  const repaint = () => {
    if (frame.current) return;
    frame.current = requestAnimationFrame(() => {
      frame.current = 0;
      setVersion((v) => v + 1);
    });
  };
  useEffect(() => () => cancelAnimationFrame(frame.current), []);

  // Pick the first app container once the pod is known.
  const chosen = container || containers.find((c) => !c.init)?.name || containers[0]?.name || "";
  const current = containers.find((c) => c.name === chosen);
  const query: LogQuery = { container: chosen || undefined, previous, timestamps, tailLines };
  const queryKey = JSON.stringify(query);

  useEffect(() => {
    if (!chosen && pods.data === undefined) return; // wait for the pod's containers
    buf.current.clear();
    setTruncated(false);
    repaint();
    const ctl = new AbortController();
    const q = JSON.parse(queryKey) as LogQuery;
    const ansi = new AnsiStripper();

    if (!follow || q.previous) {
      setState({ kind: "loading" });
      resourceApi
        .logs(clusterId, namespace, pod, q)
        .then((r) => {
          if (ctl.signal.aborted) return;
          buf.current.append(ansi.push(r.text));
          setTruncated(r.truncated);
          setState({ kind: "idle" });
          repaint();
        })
        .catch((e) => !ctl.signal.aborted && setState({ kind: "error", text: describe(e) }));
      return () => ctl.abort();
    }

    setState({ kind: "loading" });
    (async () => {
      try {
        const reader = await resourceApi.follow(clusterId, namespace, pod, q, ctl.signal);
        setState({ kind: "following" });
        const lines = new NdjsonSplitter();
        const text = new TextDecoder();
        for (;;) {
          const { value, done } = await reader.read();
          if (done) break;
          for (const l of lines.push(value)) {
            if (l.data) buf.current.append(ansi.push(text.decode(base64Bytes(l.data), { stream: true })));
            if (l.end) {
              setState(
                l.end.error
                  ? { kind: "ended", text: l.end.error.message, error: true }
                  : { kind: "ended", text: l.end.reason || "The log ended.", error: false },
              );
            }
          }
          repaint();
        }
        setState((s) => (s.kind === "following" ? { kind: "ended", text: "The connection ended.", error: true } : s));
      } catch (e) {
        if (!ctl.signal.aborted) setState({ kind: "error", text: describe(e) });
      }
    })();
    return () => ctl.abort();
  }, [clusterId, namespace, pod, queryKey, follow, reload, chosen]);

  // Keep the view at the bottom while the user has not scrolled up.
  const pre = useRef<HTMLPreElement>(null);
  const atBottom = useRef(true);
  useLayoutEffect(() => {
    const el = pre.current;
    if (el && atBottom.current) el.scrollTop = el.scrollHeight;
  });

  const back = `#/clusters/${encodeURIComponent(clusterId)}/pods/${encodeURIComponent(namespace)}`;
  const level = info ? podLevel(info) : undefined;

  return (
    <section className="logs">
      <p>
        <a href={back}>← Pods in {namespace}</a>
      </p>
      <div className="pagehead">
        <h1 className="mono">{pod}</h1>
        {info && level && (
          <span className={`badge level-${level}`}>
            <span className="dot" aria-hidden="true" />
            {info.status}
          </span>
        )}
        <a className="button" href={debugHref(clusterId, namespace, pod)}>
          Debug this pod
        </a>
        {info && (
          <span className="muted small">
            {info.ready}/{info.total} ready · {info.restarts} restarts{info.node ? ` · ${info.node}` : ""}
          </span>
        )}
      </div>
      {pods.data && !info && <div className="notice attention">This pod is not in {namespace} any more. Its logs may be gone.</div>}

      <div className="filters logcontrols">
        <label className="inline">
          Container
          <select value={chosen} onChange={(e) => setContainer(e.target.value)} disabled={containers.length === 0}>
            {containers.map((c) => (
              <option key={c.name} value={c.name}>
                {c.name}
                {c.init ? " (init)" : ""} · {c.state}
                {c.restarts ? ` · ${c.restarts} restarts` : ""}
              </option>
            ))}
            {containers.length === 0 && <option value="">default</option>}
          </select>
        </label>
        <label className="inline">
          Lines
          <select value={tailLines} onChange={(e) => setTailLines(Number(e.target.value))}>
            {TAILS.map((n) => (
              <option key={n} value={n}>
                last {n}
              </option>
            ))}
          </select>
        </label>
        <label className="toggle" title={current && !current.previous ? "This container has not restarted" : undefined}>
          <input
            type="checkbox"
            checked={previous}
            disabled={current !== undefined && !current.previous && !previous}
            onChange={(e) => setPrevious(e.target.checked)}
          />
          Previous run (before the last restart)
        </label>
        <label className="toggle">
          <input type="checkbox" checked={timestamps} onChange={(e) => setTimestamps(e.target.checked)} />
          Timestamps
        </label>
        <label className="toggle">
          <input type="checkbox" checked={wrap} onChange={(e) => setWrap(e.target.checked)} />
          Wrap lines
        </label>
        <span className="spacer" />
        {!previous &&
          (follow ? (
            <button onClick={() => setFollow(false)}>Stop following</button>
          ) : (
            <button className="primary" onClick={() => setFollow(true)}>
              Follow live
            </button>
          ))}
        {!follow && (
          <button onClick={() => setReload((n) => n + 1)} disabled={state.kind === "loading"}>
            Reload
          </button>
        )}
      </div>

      <div className="logstatus small">
        {state.kind === "loading" && <span className="muted">Loading…</span>}
        {state.kind === "following" && (
          <span className="live">
            <span className="dot" aria-hidden="true" /> Following live
          </span>
        )}
        {state.kind === "ended" && <span className={state.error ? "error" : "muted"}>Stream ended: {state.text}</span>}
        {state.kind === "error" && <span className="error">{state.text}</span>}
        {truncated && <span className="muted"> · Older lines were cut; the view shows the newest 512 KiB.</span>}
        {buf.current.dropped > 0 && <span className="muted"> · Older lines were dropped from this view.</span>}
      </div>

      <pre
        ref={pre}
        className={`logview ${wrap ? "wrap" : ""}`}
        tabIndex={0}
        aria-label={`Log of ${pod}${chosen ? ", container " + chosen : ""}`}
        onScroll={(e) => {
          const el = e.currentTarget;
          atBottom.current = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
        }}
      >
        {buf.current.text || (state.kind === "idle" ? "(no log lines)" : "")}
      </pre>
    </section>
  );
}
