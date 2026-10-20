import { useCallback, useState } from "react";
import { debugHref, healthApi, relativeTime, type ClusterHealth, type HealthFinding, type HealthReport, type Severity } from "./api";
import { CopyBox, errorMessage, usePoll } from "./common";
import { HintPanel } from "./HintPanel";
import { findingSignals } from "./hints";
import { counts, groupFindings, SEVERITIES, runsOnNode } from "./problems";

const SEVERITY_TEXT: Record<Severity, string> = { critical: "Critical", warning: "Warning", info: "Info" };
const LEVEL: Record<Severity, string> = { critical: "fail", warning: "warn", info: "info" };

const CLUSTER_ERROR: Record<string, string> = {
  agent_offline: "The cluster agent is not connected, so this cluster could not be checked.",
  unsupported: "This cluster's agent is too old for health checks. Update Kubemap.",
};

/**
 * The problem finder: every finding of the agents' health rules, per
 * cluster and per server, worst first, each with its meaning, fix and the
 * command that proves the fix. #/problems shows all clusters,
 * #/clusters/<id>/problems one.
 */
export function ProblemsPage({ clusterId }: { clusterId?: string }) {
  const load = useCallback(async (): Promise<{ clusters: ClusterHealth[]; at: string }> => {
    if (!clusterId) return healthApi.all();
    const r = await healthApi.cluster(clusterId);
    return { clusters: [{ id: clusterId, name: "", status: "active", report: r.report }], at: r.at };
  }, [clusterId]);
  // The checks use the cluster's objects and the node agents' reports
  // (the cluster agent makes no kubelet calls); once a minute is plenty.
  const { data, error, refresh } = usePoll(load, 60000, clusterId ?? "");
  const [severity, setSeverity] = useState<Severity | "">("");
  const [busy, setBusy] = useState(false);

  const all = (data?.clusters ?? []).flatMap((c) => c.report?.findings ?? []);
  const n = counts(all);
  const back = clusterId ? `#/clusters/${encodeURIComponent(clusterId)}` : "#/";

  return (
    <section>
      <p>
        <a href={back}>{clusterId ? "← Cluster" : "← Clusters"}</a>
      </p>
      <div className="pagehead">
        <h1>Problems</h1>
        {data && <span className="muted small">checked {relativeTime(data.at)}</span>}
        <button
          onClick={async () => {
            setBusy(true);
            await refresh();
            setBusy(false);
          }}
          disabled={busy}
        >
          {busy ? "Checking…" : "Check again"}
        </button>
      </div>
      <p className="muted lead">
        The Kubemap agent in each cluster runs built-in health rules on its nodes and pods. Every finding says what it
        means, why it matters, how to fix it, and which command proves the fix.
      </p>

      {error != null && <div className="notice error">{errorMessage(error)}{data ? " Showing the last result." : ""}</div>}
      {!data ? (
        !error && <p className="muted">Checking…</p>
      ) : (
        <>
          <div className="grid kpis">
            {SEVERITIES.map((s) => (
              <button
                key={s}
                className={`card stat kpi-button ${severity === s ? "selected" : ""}`}
                onClick={() => setSeverity(severity === s ? "" : s)}
                aria-pressed={severity === s}
              >
                <div className="muted small">{SEVERITY_TEXT[s]}</div>
                <div className={`value sev-${s}`}>{n[s]}</div>
              </button>
            ))}
            <div className="card stat">
              <div className="muted small">Clusters checked</div>
              <div className="value">
                {data.clusters.filter((c) => c.report).length} / {data.clusters.length}
              </div>
            </div>
          </div>
          {severity && (
            <p className="small">
              Showing only {SEVERITY_TEXT[severity].toLowerCase()} findings.{" "}
              <button className="link" onClick={() => setSeverity("")}>
                Show all
              </button>
            </p>
          )}
          {data.clusters.length === 0 && (
            <div className="card empty">
              <h2>No clusters</h2>
              <p className="muted">The server has not created its cluster record yet; check its pod logs.</p>
            </div>
          )}
          {data.clusters.map((c) => (
            <ClusterProblems key={c.id} c={c} single={!!clusterId} severity={severity} />
          ))}
        </>
      )}
    </section>
  );
}

function ClusterProblems({ c, single, severity }: { c: ClusterHealth; single: boolean; severity: Severity | "" }) {
  const findings = (c.report?.findings ?? []).filter((f) => !severity || f.severity === severity);
  const groups = groupFindings(findings);
  return (
    <div className="cluster-problems">
      {!single && (
        <h2 className="clusterhead">
          <a href={`#/clusters/${encodeURIComponent(c.id)}`}>{c.name}</a>
          {c.report && <SeverityCounts f={c.report.findings} />}
        </h2>
      )}
      {c.error && <div className="notice attention">{CLUSTER_ERROR[c.error.error] ?? c.error.message}</div>}
      {c.report && findings.length === 0 && (
        <div className="card empty">
          <h3>No {severity ? SEVERITY_TEXT[severity].toLowerCase() + " " : ""}problems found</h3>
          <p className="muted">{c.report.rules.length} rules checked.</p>
        </div>
      )}
      {groups.map((g) => (
        <div key={g.key} className="problem-group">
          <h3>
            {g.kind === "server" ? "Server " : g.kind === "namespace" ? "Namespace " : ""}
            <span className={g.kind === "cluster" ? "" : "mono"}>{g.label}</span>
          </h3>
          {g.findings.map((f, i) => (
            <Finding key={f.rule + f.object + i} f={f} clusterId={c.id} />
          ))}
        </div>
      ))}
      {c.report && <Checked r={c.report} />}
    </div>
  );
}

function SeverityCounts({ f }: { f: HealthFinding[] }) {
  const n = counts(f);
  return (
    <span className="sevcounts small">
      {SEVERITIES.filter((s) => n[s] > 0).map((s) => (
        <span key={s} className={`badge level-${LEVEL[s]}`}>
          {n[s]} {SEVERITY_TEXT[s].toLowerCase()}
        </span>
      ))}
      {f.length === 0 && <span className="badge level-ok">no problems</span>}
    </span>
  );
}

function Finding({ f, clusterId }: { f: HealthFinding; clusterId: string }) {
  const [kind, ns, name] = f.object.split("/");
  // Critical findings start open: they are what the page is for.
  return (
    <details className={`card finding sev-${f.severity}`} open={f.severity === "critical"}>
      <summary>
        <span className={`badge level-${LEVEL[f.severity]}`}>{SEVERITY_TEXT[f.severity]}</span>
        <span className="finding-title">{f.title}</span>
        <span className="muted small mono">{f.object}</span>
      </summary>
      <dl className="finding-body">
        <dt>Evidence</dt>
        <dd className="evidence">{f.evidence}</dd>
        <dt>What it means</dt>
        <dd>{f.meaning}</dd>
        <dt>Why it matters</dt>
        <dd>{f.why}</dd>
        <dt>Fix</dt>
        <dd className="fix">{f.fix}</dd>
        {f.verify && (
          <>
            <dt>Prove it</dt>
            <dd>
              {runsOnNode(f.verify) && <p className="small muted">Run this on the node itself, not with kubectl.</p>}
              <CopyBox text={f.verify} label={`Command that proves the fix for: ${f.title}`} />
            </dd>
          </>
        )}
        {(f.items?.length ?? 0) > 0 && (
          <>
            <dt>Affected</dt>
            <dd>
              <ul className="items mono small">
                {f.items!.map((it) => (
                  <li key={it}>{it}</li>
                ))}
              </ul>
              {(f.count ?? 0) > f.items!.length && <span className="muted small">and {f.count! - f.items!.length} more</span>}
            </dd>
          </>
        )}
      </dl>
      {findingSignals(f) && (
        <div className="finding-hints">
          <HintPanel signals={findingSignals(f)!} />
        </div>
      )}
      <p className="muted small rule">
        Rule {f.rule}
        {kind === "Pod" && ns && name && (
          <>
            {" · "}
            <a href={debugHref(clusterId, ns, name)}>Debug this pod step by step</a>
          </>
        )}
      </p>
    </details>
  );
}

/** What was checked, with which thresholds: "no finding" is not "not checked". */
function Checked({ r }: { r: HealthReport }) {
  const t = r.thresholds;
  return (
    <details className="checked small">
      <summary>What was checked ({r.rules.length} rules)</summary>
      <p className="mono">{r.rules.join(", ")}</p>
      <p>
        Kubelet thresholds ({t.source}): evict at{" "}
        {Object.entries(t.evictionHard)
          .map(([k, v]) => `${k} < ${v}`)
          .join(", ")}
        {t.evictionMinimumReclaim && Object.keys(t.evictionMinimumReclaim).length > 0 && (
          <>
            ; then free a further{" "}
            {Object.entries(t.evictionMinimumReclaim)
              .map(([k, v]) => `${v} (${k})`)
              .join(", ")}
          </>
        )}
        ; image clean-up from {t.imageGCHighPercent}% disk use.
      </p>
      {(r.notes?.length ?? 0) > 0 && (
        <ul>
          {r.notes!.map((n) => (
            <li key={n}>{n}</li>
          ))}
        </ul>
      )}
      <p className="muted">Checked {new Date(r.time).toLocaleString()}.</p>
    </details>
  );
}
