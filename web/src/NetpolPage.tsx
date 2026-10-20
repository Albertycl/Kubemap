import { useCallback, useState } from "react";
import { netpolApi, relativeTime, resourceApi, type NetpolMatrix, type NetpolResult, type NetpolSide } from "./api";
import { errorMessage, usePoll } from "./common";
import { checkQuery, emptyForm, matrixCounts, type CheckForm } from "./netpol";

/**
 * NetworkPolicy checker (KM-36 UI): "Can A reach B on port X?" with the
 * deciding policy, and a namespace matrix. #/clusters/<id>/netpol
 */
export function NetpolPage({ clusterId }: { clusterId: string }) {
  const [form, setForm] = useState<CheckForm>(emptyForm);
  const [result, setResult] = useState<{ result: NetpolResult; at: string } | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const set = (patch: Partial<CheckForm>) => setForm((f) => ({ ...f, ...patch }));

  const namespaces = usePoll(() => resourceApi.namespaces(clusterId), 60000);
  const nsNames = (namespaces.data?.namespaces ?? []).map((n) => n.name);

  const run = async (f: CheckForm) => {
    const q = checkQuery(f);
    if (!q.params) {
      setError(q.missing ?? "");
      return;
    }
    setBusy(true);
    setError("");
    try {
      setResult(await netpolApi.check(clusterId, q.params));
    } catch (e) {
      setResult(null);
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };

  return (
    <section>
      <p>
        <a href={`#/clusters/${encodeURIComponent(clusterId)}`}>← Cluster</a>
      </p>
      <div className="pagehead">
        <h1>NetworkPolicy checker</h1>
      </div>
      <p className="muted lead">
        Ask whether a connection is allowed and which policy decides it. Kubemap evaluates the NetworkPolicies the way
        the network plugin does: a pod is isolated for a direction as soon as one policy selects it, and then only a
        matching rule lets traffic through. Both ends count: egress at the source, ingress at the destination.
      </p>

      <div className="card">
        <h2>Can A reach B?</h2>
        <form
          className="netpol-form"
          onSubmit={(e) => {
            e.preventDefault();
            run(form);
          }}
        >
          <fieldset>
            <legend>From</legend>
            <Choice name="src" value={form.srcKind} options={[["pod", "A pod"], ["ip", "An IP address"]]} onChange={(v) => set({ srcKind: v as CheckForm["srcKind"] })} />
            {form.srcKind === "pod" ? (
              <PodPicker clusterId={clusterId} namespaces={nsNames} ns={form.srcNamespace} pod={form.srcPod}
                onNs={(v) => set({ srcNamespace: v, srcPod: "" })} onPod={(v) => set({ srcPod: v })} />
            ) : (
              <label>
                IP address
                <input value={form.srcIp} onChange={(e) => set({ srcIp: e.target.value })} placeholder="10.42.0.15" />
              </label>
            )}
          </fieldset>
          <fieldset>
            <legend>To</legend>
            <Choice name="dst" value={form.dstKind} options={[["service", "A Service"], ["pod", "A pod"], ["ip", "An IP address"]]}
              onChange={(v) => set({ dstKind: v as CheckForm["dstKind"], dstName: "" })} />
            {form.dstKind === "service" ? (
              <ServicePicker clusterId={clusterId} namespaces={nsNames} ns={form.dstNamespace} name={form.dstName} port={form.port}
                onNs={(v) => set({ dstNamespace: v, dstName: "", port: "" })}
                onService={(name, port, protocol) => set({ dstName: name, port, protocol })}
                onPort={(port, protocol) => set({ port, protocol })} />
            ) : form.dstKind === "pod" ? (
              <PodPicker clusterId={clusterId} namespaces={nsNames} ns={form.dstNamespace} pod={form.dstName}
                onNs={(v) => set({ dstNamespace: v, dstName: "" })} onPod={(v) => set({ dstName: v })} />
            ) : (
              <label>
                IP address
                <input value={form.dstIp} onChange={(e) => set({ dstIp: e.target.value })} placeholder="1.1.1.1" />
              </label>
            )}
            {form.dstKind !== "service" && (
              <div className="row">
                <label>
                  Port
                  <input inputMode="numeric" value={form.port} onChange={(e) => set({ port: e.target.value })} placeholder="8080" className="short" />
                </label>
                <label>
                  Protocol
                  <select value={form.protocol} onChange={(e) => set({ protocol: e.target.value })}>
                    <option>TCP</option>
                    <option>UDP</option>
                    <option>SCTP</option>
                  </select>
                </label>
              </div>
            )}
          </fieldset>
          <div className="actions left">
            <button className="primary" type="submit" disabled={busy}>
              {busy ? "Checking…" : "Check"}
            </button>
          </div>
        </form>
        {error && <div className="notice error">{error}</div>}
        {result && <Result r={result.result} at={result.at} />}
      </div>

      <MatrixCard
        clusterId={clusterId}
        namespaces={nsNames}
        onCell={(ns, pod, service, port, protocol) => {
          const f: CheckForm = { ...emptyForm, srcKind: "pod", srcNamespace: ns, srcPod: pod, dstKind: "service", dstNamespace: ns, dstName: service, port: String(port), protocol };
          setForm(f);
          run(f);
          window.scrollTo({ top: 0, behavior: "smooth" });
        }}
      />
    </section>
  );
}

function Choice({ name, value, options, onChange }: { name: string; value: string; options: [string, string][]; onChange: (v: string) => void }) {
  return (
    <div className="choice" role="radiogroup">
      {options.map(([v, label]) => (
        <label key={v} className="toggle">
          <input type="radio" name={name} checked={value === v} onChange={() => onChange(v)} />
          {label}
        </label>
      ))}
    </div>
  );
}

function NsSelect({ namespaces, ns, onNs }: { namespaces: string[]; ns: string; onNs: (v: string) => void }) {
  return (
    <label>
      Namespace
      <select value={ns} onChange={(e) => onNs(e.target.value)}>
        <option value="">Choose…</option>
        {namespaces.map((n) => (
          <option key={n}>{n}</option>
        ))}
      </select>
    </label>
  );
}

function PodPicker(props: { clusterId: string; namespaces: string[]; ns: string; pod: string; onNs: (v: string) => void; onPod: (v: string) => void }) {
  const { clusterId, ns } = props;
  const load = useCallback(() => (ns ? resourceApi.pods(clusterId, ns) : Promise.resolve({ pods: [], truncated: false })), [clusterId, ns]);
  const pods = usePoll(load, 60000, ns);
  const running = (pods.data?.pods ?? []).filter((p) => p.phase === "Running");
  return (
    <div className="row">
      <NsSelect namespaces={props.namespaces} ns={ns} onNs={props.onNs} />
      <label>
        Pod
        <select value={props.pod} onChange={(e) => props.onPod(e.target.value)} disabled={!ns}>
          <option value="">{ns ? (pods.data ? "Choose…" : "Loading…") : "Choose a namespace first"}</option>
          {running.map((p) => (
            <option key={p.name} value={p.name}>
              {p.name}
            </option>
          ))}
        </select>
      </label>
    </div>
  );
}

function ServicePicker(props: {
  clusterId: string;
  namespaces: string[];
  ns: string;
  name: string;
  port: string;
  onNs: (v: string) => void;
  onService: (name: string, port: string, protocol: string) => void;
  onPort: (port: string, protocol: string) => void;
}) {
  const { clusterId, ns } = props;
  const load = useCallback(() => (ns ? netpolApi.services(clusterId, ns) : Promise.resolve({ services: [], truncated: false })), [clusterId, ns]);
  const svcs = usePoll(load, 60000, ns);
  const list = svcs.data?.services ?? [];
  const current = list.find((s) => s.name === props.name);
  return (
    <div className="row">
      <NsSelect namespaces={props.namespaces} ns={ns} onNs={props.onNs} />
      <label>
        Service
        <select
          value={props.name}
          disabled={!ns}
          onChange={(e) => {
            const s = list.find((x) => x.name === e.target.value);
            props.onService(e.target.value, s?.ports[0] ? String(s.ports[0].port) : "", s?.ports[0]?.protocol ?? "TCP");
          }}
        >
          <option value="">{ns ? (svcs.data ? "Choose…" : "Loading…") : "Choose a namespace first"}</option>
          {list.map((s) => (
            <option key={s.name} value={s.name}>
              {s.name}
            </option>
          ))}
        </select>
      </label>
      <label>
        Port
        <select
          value={props.port}
          disabled={!current}
          onChange={(e) => {
            const p = current?.ports.find((x) => String(x.port) === e.target.value);
            props.onPort(e.target.value, p?.protocol ?? "TCP");
          }}
        >
          {!current && <option value="">–</option>}
          {current?.ports.map((p) => (
            <option key={p.port + p.protocol} value={String(p.port)}>
              {p.port}/{p.protocol}
              {p.name ? ` (${p.name})` : ""}
              {p.targetPort ? ` → ${p.targetPort}` : ""}
            </option>
          ))}
        </select>
      </label>
    </div>
  );
}

function Result({ r, at }: { r: NetpolResult; at: string }) {
  return (
    <div className="netpol-result">
      {r.paths.length === 0 ? (
        <div className="verdict-banner none">
          <span className="verdict-word">No destination</span>
          <span>No pod is behind this destination, so there is no connection to allow or block. Check the Service's selector.</span>
        </div>
      ) : (
        <div className={`verdict-banner ${r.allowed ? "ok" : "problem"}`}>
          <span className="verdict-word">{r.allowed ? "Allowed" : "Blocked"}</span>
          <span>{r.summary}</span>
        </div>
      )}
      <p className="small muted">
        {r.from} → port {r.port}/{r.protocol} · checked {relativeTime(at)}
      </p>
      {r.paths.map((p) => (
        <div key={p.to + p.port} className="path">
          <h3>
            <span className={`badge level-${p.allowed ? "ok" : "fail"}`}>{p.allowed ? "allowed" : "blocked"}</span>{" "}
            <span className="mono">{p.to}</span> port {p.port}
          </h3>
          <div className="sides">
            <Side title="Egress at the source" s={p.egress} />
            <Side title="Ingress at the destination" s={p.ingress} />
          </div>
        </div>
      ))}
      {(r.notes?.length ?? 0) > 0 && (
        <ul className="small muted notes">
          {r.notes!.map((n) => (
            <li key={n}>{n}</li>
          ))}
        </ul>
      )}
    </div>
  );
}

function Side({ title, s }: { title: string; s: NetpolSide }) {
  return (
    <div className={`side ${s.allowed ? "ok" : "problem"}`}>
      <div className="small strong">{title}</div>
      <div>{s.decision}</div>
      <div className="small muted">
        {s.isolated
          ? `Isolated by ${s.selecting?.length ? s.selecting.join(", ") : "a policy"}`
          : "Not isolated: no policy selects this pod for this direction, so everything is allowed."}
      </div>
      {(s.allowedBy?.length ?? 0) > 0 && (
        <ul className="small rules">
          {s.allowedBy!.map((x) => (
            <li key={x.policy + x.rule}>
              <span className="mono">{x.policy}</span> rule {x.rule}: {x.detail}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

function MatrixCard({
  clusterId,
  namespaces,
  onCell,
}: {
  clusterId: string;
  namespaces: string[];
  onCell: (ns: string, pod: string, service: string, port: number, protocol: string) => void;
}) {
  const [ns, setNs] = useState("");
  const [m, setM] = useState<NetpolMatrix | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const build = async () => {
    if (!ns) return;
    setBusy(true);
    setError("");
    try {
      setM(await netpolApi.matrix(clusterId, ns));
    } catch (e) {
      setM(null);
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  };
  const n = m ? matrixCounts(m.cells) : undefined;
  return (
    <div className="card">
      <h2>Namespace matrix</h2>
      <p className="muted small">
        Which workloads of a namespace can reach which of its Service ports. One running pod stands for each workload.
        Click a cell to see the deciding policy.
      </p>
      <div className="filters">
        <NsSelect namespaces={namespaces} ns={ns} onNs={setNs} />
        <button onClick={build} disabled={!ns || busy}>
          {busy ? "Checking every pair…" : "Build the matrix"}
        </button>
        {n && (
          <span className="small muted">
            {n.allowed} allowed, {n.blocked} blocked{n.empty ? `, ${n.empty} with no pods behind the Service` : ""}
            {n.errors ? `, ${n.errors} not checked` : ""} · {relativeTime(m!.at)}
          </span>
        )}
      </div>
      {error && <div className="notice error">{error}</div>}
      {m && m.truncated && <div className="notice attention">Only the first 20 workloads and 20 Service ports are shown.</div>}
      {m && (m.sources.length === 0 || m.targets.length === 0) && (
        <p className="muted">{m.sources.length === 0 ? "No running pods in this namespace." : "No Services with a selector in this namespace."}</p>
      )}
      {m && m.sources.length > 0 && m.targets.length > 0 && (
        <div className="matrix-wrap">
          <table className="matrix">
            <thead>
              <tr>
                <th scope="col" className="corner">
                  from ↓ / to →
                </th>
                {m.targets.map((t) => (
                  <th key={t.service + t.port + t.protocol} scope="col" className="mono">
                    <span>{t.service}</span>
                    <span className="muted">
                      :{t.port}
                      {t.protocol !== "TCP" ? "/" + t.protocol : ""}
                    </span>
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {m.sources.map((s, i) => (
                <tr key={s.label}>
                  <th scope="row" className="mono" title={`pod ${s.pod}`}>
                    {s.label.replace(/^(ReplicaSet|Pod)\//, "")}
                  </th>
                  {m.targets.map((t, j) => {
                    const c = m.cells[i][j];
                    const cls = c.error ? "err" : c.noDestination ? "none" : c.allowed ? "ok" : "problem";
                    const label = c.error ? "!" : c.noDestination ? "–" : c.allowed ? "✓" : "✗";
                    const word = c.error ? "not checked" : c.noDestination ? "no pods behind the Service" : c.allowed ? "allowed" : "blocked";
                    return (
                      <td key={j} className={`cell ${cls}`}>
                        <button
                          title={c.error ?? (c.noDestination ? "No pods behind this Service" : c.summary)}
                          aria-label={`${s.label} to ${t.service}:${t.port}: ${word}`}
                          onClick={() => onCell(m.namespace, s.pod, t.service, t.port, t.protocol)}
                        >
                          {label}
                        </button>
                      </td>
                    );
                  })}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
