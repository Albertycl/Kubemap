import { useEffect, useState } from "react";
import {
  api,
  ApiError,
  resourceApi,
  scenarioApi,
  type CheckResult,
  type Cluster,
  type Scenario,
  type ScenarioStep,
} from "./api";
import { CopyBox, errorMessage, usePoll } from "./common";
import { fill, invalidValue, pageHref, parseQuery, usable } from "./scenario";

const AREA_TEXT: Record<string, string> = { traffic: "Traffic", pods: "Pods", nodes: "Nodes", security: "Security", operations: "Operations" };

/** #/scenarios: every scenario by area. */
export function ScenarioList() {
  const { data, error } = usePoll(() => scenarioApi.list(), 300000);
  return (
    <section>
      <div className="pagehead">
        <h1>Scenarios</h1>
      </div>
      <p className="muted lead">
        Real questions an SRE gets, walked through in the order a senior engineer checks. Each step says why it matters,
        gives the command to type, opens the Kubemap page that shows it, and can run a read-only live check against a
        cluster.
      </p>
      {error != null && <div className="notice error">{errorMessage(error)}</div>}
      {data &&
        data.areas
          .filter((a) => data.scenarios.some((s) => s.area === a))
          .map((area) => (
            <div key={area}>
              <h2 className="areahead">{AREA_TEXT[area] ?? area}</h2>
              <div className="scenario-cards">
                {data.scenarios
                  .filter((s) => s.area === area)
                  .map((s) => (
                    <a key={s.id} className="card scenario-card" href={`#/scenarios/${encodeURIComponent(s.id)}`}>
                      <h3>{s.title}</h3>
                      <p className="question">{s.question}</p>
                      {(s.symptoms?.length ?? 0) > 0 && <p className="muted small">{s.symptoms!.join(" · ")}</p>}
                      <p className="muted small">
                        {s.steps} steps, {s.checks} with a live check{s.lab ? ` · practice lab ${s.lab}` : ""}
                      </p>
                    </a>
                  ))}
              </div>
            </div>
          ))}
    </section>
  );
}

type StepState = { loading?: boolean; result?: CheckResult; error?: unknown };

/**
 * #/scenarios/<id>[?cluster=<id>&ns=..&svc=..]: the walk-through. It works
 * without a cluster (steps and commands show); live checks need one.
 */
export function ScenarioPage({ id, query }: { id: string; query?: string }) {
  const initial = parseQuery(query);
  const [data, setData] = useState<{ scenario: Scenario; placeholders: Record<string, string> } | undefined>();
  const [loadError, setLoadError] = useState<unknown>(null);
  const [clusters, setClusters] = useState<Cluster[]>([]);
  const [clusterId, setClusterId] = useState(initial.cluster ?? "");
  const [values, setValues] = useState<Record<string, string>>(() => {
    const v = { ...initial };
    delete v.cluster;
    return v;
  });
  const [checks, setChecks] = useState<Record<number, StepState>>({});

  useEffect(() => {
    scenarioApi.get(id).then(setData, setLoadError);
    api.clusters().then(
      (list) => setClusters(list),
      () => setClusters([]),
    );
  }, [id]);

  if (loadError) {
    const nf = loadError instanceof ApiError && loadError.status === 404;
    return (
      <section>
        <p>
          <a href="#/scenarios">← Scenarios</a>
        </p>
        <div className="notice error">{nf ? "No such scenario." : errorMessage(loadError)}</div>
      </section>
    );
  }
  if (!data) return <p className="muted">Loading…</p>;
  const s = data.scenario;
  const cluster = clusters.find((c) => c.id === clusterId);

  const run = async (n: number) => {
    if (!clusterId) return;
    setChecks((c) => ({ ...c, [n]: { loading: true } }));
    try {
      // Only valid values are sent; a missing one comes back as "needs input".
      const result = await scenarioApi.check(clusterId, s.id, n, usable(values));
      setChecks((c) => ({ ...c, [n]: { result } }));
    } catch (error) {
      setChecks((c) => ({ ...c, [n]: { error } }));
    }
  };
  const runAll = () => s.steps.forEach((st, i) => st.check && run(i + 1));

  return (
    <section>
      <p>
        <a href="#/scenarios">← Scenarios</a>
      </p>
      <div className="pagehead">
        <h1>{s.title}</h1>
        <span className="badge">{AREA_TEXT[s.area] ?? s.area}</span>
      </div>
      <p className="question big">{s.question}</p>
      {(s.symptoms?.length ?? 0) > 0 && (
        <ul className="symptoms">
          {s.symptoms!.map((x) => (
            <li key={x}>{x}</li>
          ))}
        </ul>
      )}

      <div className="card context">
        <h2>Where</h2>
        <p className="muted small">
          Choose a cluster and fill in what you know. The commands and live checks use these values; anything left empty
          stays as a placeholder like <code>&lt;svc&gt;</code>.
        </p>
        <div className="context-fields">
          <label>
            Cluster
            <select
              value={clusterId}
              onChange={(e) => {
                setClusterId(e.target.value);
                setChecks({});
              }}
            >
              <option value="">No cluster (read only the steps)</option>
              {clusters.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name} ({c.status})
                </option>
              ))}
            </select>
          </label>
          {s.params.map((p) => {
            const bad = values[p] ? invalidValue(p, values[p]) : null;
            return (
              <label key={p}>
                {data.placeholders[p] ?? p} <code>&lt;{p}&gt;</code>
                <input
                  value={values[p] ?? ""}
                  aria-invalid={bad !== null}
                  className={bad ? "invalid" : ""}
                  onChange={(e) => setValues((v) => ({ ...v, [p]: e.target.value.trim() }))}
                />
                {bad && <span className="small error">Not used: {bad}.</span>}
              </label>
            );
          })}
        </div>
        {clusterId && s.params.includes("from") && !values.from && <IngressPodHint clusterId={clusterId} onPick={(ns, pod) => setValues((v) => ({ ...v, fromns: ns, from: pod }))} />}
        <div className="actions left">
          <button className="primary" onClick={runAll} disabled={!clusterId}>
            Run every live check
          </button>
          {!clusterId && <span className="muted small">Live checks need a cluster.</span>}
          {cluster && cluster.status !== "active" && <span className="error small">This cluster's agent is {cluster.status}.</span>}
        </div>
      </div>

      <ol className="scenario-steps">
        {s.steps.map((st, i) => (
          <StepCard key={i} n={i + 1} st={st} values={values} clusterId={clusterId} state={checks[i + 1]} onRun={() => run(i + 1)} />
        ))}
      </ol>

      {(s.causes?.length ?? 0) > 0 && (
        <div className="card">
          <h2>Usual causes</h2>
          {s.causes!.map((c, i) => (
            <div key={i} className="cause-item">
              <h3>{c.cause}</h3>
              <p>
                <span className="muted small">Fix: </span>
                {c.fix}
              </p>
              {c.prove && <CopyBox text={fill(c.prove, values)} label={`Command that proves the fix for: ${c.cause}`} />}
            </div>
          ))}
        </div>
      )}
      <div className="scenario-extra">
        {(s.prevention?.length ?? 0) > 0 && (
          <div className="card">
            <h2>Prevention</h2>
            <ul>
              {s.prevention!.map((x) => (
                <li key={x}>{x}</li>
              ))}
            </ul>
          </div>
        )}
        {(s.senior?.length ?? 0) > 0 && (
          <div className="card">
            <h2>What a senior SRE adds</h2>
            <ul>
              {s.senior!.map((x) => (
                <li key={x}>{x}</li>
              ))}
            </ul>
          </div>
        )}
      </div>
      {s.lab && (
        <p className="muted small">
          Practice lab: <code>{s.lab}</code>
          {clusterId && (
            <>
              {" · "}
              <a href={`#/clusters/${encodeURIComponent(clusterId)}/pods/${encodeURIComponent(s.lab.split("/")[0])}`}>its pods</a>
            </>
          )}
        </p>
      )}
    </section>
  );
}

const VERDICT_TEXT: Record<string, string> = {
  good: "Good",
  bad: "Problem",
  warn: "Attention",
  unknown: "Unknown",
  "needs-input": "Needs input",
};
const VERDICT_LEVEL: Record<string, string> = { good: "ok", bad: "fail", warn: "warn", unknown: "info", "needs-input": "info" };

function StepCard({
  n,
  st,
  values,
  clusterId,
  state,
  onRun,
}: {
  n: number;
  st: ScenarioStep;
  values: Record<string, string>;
  clusterId: string;
  state?: StepState;
  onRun: () => void;
}) {
  const href = pageHref(st.kubemap, clusterId, usable(values));
  const r = state?.result;
  return (
    <li className={`card scenario-step ${r ? "verdict-" + r.verdict : ""}`}>
      <h3>
        <span className="stepno">{n}</span> {st.title}
      </h3>
      <p>{st.why}</p>
      <CopyBox text={fill(st.command, values)} label={`Command for step ${n}`} />
      <div className="step-foot">
        {st.kubemap &&
          (href ? (
            <a className="button" href={href}>
              Open in Kubemap: {st.kubemap}
            </a>
          ) : (
            <span className="muted small">Kubemap page: {st.kubemap} (choose a cluster)</span>
          ))}
        {st.check && (
          <button onClick={onRun} disabled={!clusterId || state?.loading}>
            {state?.loading ? "Checking…" : `Live check (${st.check.op})`}
          </button>
        )}
      </div>
      {st.check && !clusterId && <p className="small muted">Live check: not connected. Choose a cluster above to run it.</p>}
      {state?.error != null && (
        <div className="notice error small">
          {state.error instanceof ApiError && state.error.code === "agent_offline"
            ? "Not connected: the cluster agent is offline."
            : errorMessage(state.error)}
        </div>
      )}
      {r && (
        <div className="check-result">
          <span className={`badge level-${VERDICT_LEVEL[r.verdict] ?? "info"}`}>{VERDICT_TEXT[r.verdict] ?? r.verdict}</span>{" "}
          <span>{r.summary}</span>
          {(r.details?.length ?? 0) > 0 && (
            <ul className="small">
              {r.details!.map((d, i) => (
                <li key={i}>{d}</li>
              ))}
            </ul>
          )}
        </div>
      )}
      {(st.good || st.bad) && (
        <dl className="goodbad small">
          {st.good && (
            <>
              <dt>Good</dt>
              <dd>{st.good}</dd>
            </>
          )}
          {st.bad && (
            <>
              <dt>Bad</dt>
              <dd>{st.bad}</dd>
            </>
          )}
        </dl>
      )}
    </li>
  );
}

/** Offers the ingress controller's pod (Traefik on K3s) as the source of the NetworkPolicy check. */
function IngressPodHint({ clusterId, onPick }: { clusterId: string; onPick: (ns: string, pod: string) => void }) {
  const [pod, setPod] = useState<{ ns: string; name: string } | null | undefined>(undefined);
  useEffect(() => {
    resourceApi.pods(clusterId, "kube-system").then(
      (r) => {
        const p = r.pods.find((x) => x.name.startsWith("traefik") && x.phase === "Running");
        setPod(p ? { ns: p.namespace, name: p.name } : null);
      },
      () => setPod(null),
    );
  }, [clusterId]);
  if (!pod) return null;
  return (
    <p className="small">
      The NetworkPolicy check needs the ingress controller's pod as the source.{" "}
      <button className="link" onClick={() => onPick(pod.ns, pod.name)}>
        Use {pod.ns}/{pod.name}
      </button>
    </p>
  );
}
