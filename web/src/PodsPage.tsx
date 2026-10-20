import { useCallback, useState } from "react";
import { age, ApiError, debugHref, resourceApi, type PodInfo } from "./api";
import { yamlHref } from "./yamlview";
import { HintPanel } from "./HintPanel";
import { podNeedsHint, podSignals } from "./hints";
import { errorMessage, usePoll } from "./common";

type Level = "ok" | "warn" | "fail";

/** A pod's colour: fine, on its way (starting, stopping, not all ready), or failing. */
export function podLevel(p: PodInfo): Level {
  if (p.status === "Completed" || p.status === "Succeeded") return "ok";
  if (p.status === "Running") return p.ready === p.total ? "ok" : "warn";
  if (/^(Pending|ContainerCreating|PodInitializing|Terminating)$|^Init:\d+\/\d+$/.test(p.status)) return "warn";
  return "fail";
}

export function logsHref(clusterId: string, ns: string, pod: string): string {
  return `#/clusters/${encodeURIComponent(clusterId)}/namespaces/${encodeURIComponent(ns)}/pods/${encodeURIComponent(pod)}/logs`;
}

/** #/clusters/<id>/pods, or one namespace: #/clusters/<id>/pods/<ns>. */
export function PodsPage({ clusterId, namespace }: { clusterId: string; namespace?: string }) {
  const ns = namespace ?? "";
  const nsList = usePoll(() => resourceApi.namespaces(clusterId), 30000);
  const load = useCallback(() => resourceApi.pods(clusterId, ns || undefined), [clusterId, ns]);
  const { data, error } = usePoll(load, 10000, ns);
  const [search, setSearch] = useState("");
  const [problems, setProblems] = useState(false);
  const back = `#/clusters/${encodeURIComponent(clusterId)}`;

  const setNamespace = (v: string) => {
    window.location.hash = `${back}/pods${v ? "/" + encodeURIComponent(v) : ""}`;
  };

  const q = search.trim().toLowerCase();
  const pods = (data?.pods ?? [])
    .filter((p) => !ns || p.namespace === ns) // a late answer for another namespace
    .filter((p) => !q || p.name.toLowerCase().includes(q) || (p.node ?? "").toLowerCase().includes(q))
    .filter((p) => !problems || podLevel(p) !== "ok");
  const failing = (data?.pods ?? []).filter((p) => podLevel(p) === "fail").length;

  return (
    <section>
      <p>
        <a href={back}>← Cluster</a>
      </p>
      <div className="pagehead">
        <h1>Pods</h1>
        {data && (
          <span className="muted small">
            {data.pods.length} pods{failing > 0 ? `, ${failing} failing` : ""}
          </span>
        )}
      </div>

      <div className="filters">
        <label className="inline">
          Namespace
          <select value={ns} onChange={(e) => setNamespace(e.target.value)}>
            <option value="">All namespaces</option>
            {(nsList.data?.namespaces ?? []).map((n) => (
              <option key={n.name} value={n.name}>
                {n.name}
              </option>
            ))}
            {ns && !(nsList.data?.namespaces ?? []).some((n) => n.name === ns) && <option value={ns}>{ns}</option>}
          </select>
        </label>
        <input
          type="search"
          className="search"
          placeholder="Find a pod or node"
          aria-label="Find a pod or node"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
        />
        <label className="toggle">
          <input type="checkbox" checked={problems} onChange={(e) => setProblems(e.target.checked)} />
          Only pods with problems
        </label>
      </div>

      {error != null && (
        <div className="notice error">
          {error instanceof ApiError && error.code === "agent_offline"
            ? "The cluster agent is not connected, so pods cannot be listed now."
            : errorMessage(error)}
          {data ? " Showing the last list." : ""}
        </div>
      )}
      {data?.truncated && (
        <div className="notice attention">Only the first {data.pods.length} pods are shown. Choose a namespace to see all of its pods.</div>
      )}

      {!data ? (
        !error && <p className="muted">Loading…</p>
      ) : pods.length === 0 ? (
        <div className="card empty">
          <h2>No pods</h2>
          <p className="muted">{data.pods.length === 0 ? "This namespace has no pods." : "Nothing matches the filters."}</p>
        </div>
      ) : (
        <div className="card flush">
          <table className="data clickable pods">
            <thead>
              <tr>
                <th>Pod</th>
                {!ns && <th>Namespace</th>}
                <th>Status</th>
                <th className="num">Ready</th>
                <th className="num">Restarts</th>
                <th>Node</th>
                <th className="num">Age</th>
                <th>
                  <span className="sr-only">Actions</span>
                </th>
              </tr>
            </thead>
            <tbody>
              {pods.map((p) => {
                const href = logsHref(clusterId, p.namespace, p.name);
                const level = podLevel(p);
                return (
                  <tr key={p.namespace + "/" + p.name} onClick={() => (window.location.hash = href)}>
                    <td>
                      <a href={href} className="strong mono" title="Show logs">
                        {p.name}
                      </a>
                    </td>
                    {!ns && <td>{p.namespace}</td>}
                    <td>
                      <span className={`badge level-${level}`}>
                        <span className="dot" aria-hidden="true" />
                        {p.status}
                      </span>
                      {podNeedsHint(p) && <HintPanel signals={podSignals(p)} />}
                    </td>
                    <td className="num">
                      {p.ready}/{p.total}
                    </td>
                    <td className={`num ${p.restarts >= 5 ? "error" : ""}`}>{p.restarts}</td>
                    <td>{p.node || "–"}</td>
                    <td className="num">{age(p.created)}</td>
                    <td>
                      <a href={debugHref(clusterId, p.namespace, p.name)} onClick={(e) => e.stopPropagation()}>
                        Debug
                      </a>{" "}
                      <a
                        href={yamlHref(clusterId, { group: "", version: "v1", resource: "pods", namespace: p.namespace, name: p.name })}
                        onClick={(e) => e.stopPropagation()}
                      >
                        YAML
                      </a>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
