import { useEffect, useState } from "react";
import { resourceApi, yamlApi, type YamlDiff, type YamlView } from "./api";
import { CopyBox, errorMessage } from "./common";
import { complete, diffStats, parseYamlQuery, yamlHref, YAML_KINDS, type YamlTarget } from "./yamlview";

/**
 * YAML view and edit (KM-39): any object's YAML with secret-looking values
 * redacted; an edited copy gives a diff against the live object and the
 * kubectl commands that dry-run and apply it. Kubemap never applies an edit:
 * you run the commands with your own credentials.
 */
export function YamlPage({ clusterId, query }: { clusterId: string; query?: string }) {
  const target = parseYamlQuery(query);
  const [view, setView] = useState<YamlView | null>(null);
  const [loadError, setLoadError] = useState("");
  const [edited, setEdited] = useState("");
  const [diff, setDiff] = useState<YamlDiff | null>(null);
  const [diffError, setDiffError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!complete(target)) return;
    let live = true;
    yamlApi
      .get(clusterId, target)
      .then((v) => {
        if (!live) return;
        setView(v);
        setEdited(v.yaml);
      })
      .catch((e) => live && setLoadError(errorMessage(e)));
    return () => {
      live = false;
    };
    // The page is keyed by route, so the target is fixed for its life.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  async function showDiff() {
    setBusy(true);
    setDiff(null);
    setDiffError("");
    try {
      setDiff(await yamlApi.diff(clusterId, target, edited));
    } catch (e) {
      setDiffError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }

  const title = view ? `${view.kind} ${target.namespace ? target.namespace + "/" : ""}${target.name}` : "YAML";
  return (
    <section>
      <p>
        <a href={`#/clusters/${encodeURIComponent(clusterId)}`}>← Cluster</a>
      </p>
      <h1>{title}</h1>
      <Picker clusterId={clusterId} initial={target} />
      {!complete(target) ? (
        <p className="muted">Choose a kind, a namespace and a name to show the object's YAML.</p>
      ) : loadError ? (
        <div className="notice error">{loadError}</div>
      ) : !view ? (
        <p className="muted">Loading…</p>
      ) : (
        <>
          <p className="muted small">
            resourceVersion {view.resourceVersion}. Kubemap shows the diff and the kubectl commands; it never applies an
            edit. {view.redacted.length > 0 && `${view.redacted.length} value(s) are redacted; leave them as they are.`}
          </p>
          {view.argoApp && (
            <div className="notice attention">
              Managed by Argo CD ({view.argoApp}): change it in Git. A direct patch is reverted at the next sync.
            </div>
          )}
          {view.redacted.length > 0 && (
            <details className="paste">
              <summary>Redacted values</summary>
              <ul className="small mono">
                {view.redacted.map((p) => (
                  <li key={p}>{p}</li>
                ))}
              </ul>
            </details>
          )}
          <label>
            YAML (edit it, then show the diff)
            <textarea
              className="mono yaml-edit"
              spellCheck={false}
              rows={Math.min(40, Math.max(12, edited.split("\n").length + 1))}
              value={edited}
              onChange={(e) => {
                setEdited(e.target.value);
                setDiff(null);
              }}
            />
          </label>
          <div className="actions">
            <button className="primary" onClick={showDiff} disabled={busy}>
              {busy ? "Comparing…" : "Show the diff and commands"}
            </button>
            <button onClick={() => (setEdited(view.yaml), setDiff(null), setDiffError(""))} disabled={busy || edited === view.yaml}>
              Reset
            </button>
          </div>
          {diffError && <div className="notice error">{diffError}</div>}
          {diff && <DiffResult diff={diff} />}
        </>
      )}
    </section>
  );
}

function DiffResult({ diff }: { diff: YamlDiff }) {
  const { added, removed } = diffStats(diff.diff);
  return (
    <div className="card">
      <h2>{diff.changed ? `Diff against the live object (+${added} −${removed})` : "No change"}</h2>
      {diff.notes.map((n) => (
        <p key={n} className="muted small">
          {n}
        </p>
      ))}
      {diff.changed && (
        <>
          <pre className="yaml-diff">
            {diff.diff.map((l, i) => (
              <div key={i} className={l.op === "+" ? "add" : l.op === "-" ? "del" : "ctx"}>
                {l.op} {l.text}
              </div>
            ))}
          </pre>
          <h3>Run with your own credentials</h3>
          {diff.commands.map((c) => (
            <div key={c.title}>
              <p>
                <strong>{c.title}</strong>
              </p>
              <CopyBox text={c.command} label={c.title} />
            </div>
          ))}
        </>
      )}
    </div>
  );
}

/** Chooses the object: a kind from the list (or any resource), namespace and name. */
function Picker({ clusterId, initial }: { clusterId: string; initial: YamlTarget }) {
  const [t, setT] = useState<YamlTarget>(initial.resource ? initial : { ...initial, group: "apps", version: "v1", resource: "deployments" });
  const kind = YAML_KINDS.find((k) => k.group === t.group && k.version === t.version && k.resource === t.resource);
  const namespaced = !kind || kind.namespaced;
  const [namespaces, setNamespaces] = useState<string[]>([]);
  const [names, setNames] = useState<string[]>([]);
  const [namesNote, setNamesNote] = useState("");

  useEffect(() => {
    resourceApi
      .namespaces(clusterId)
      .then((r) => setNamespaces(r.namespaces.map((n) => n.name).sort()))
      .catch(() => setNamespaces([]));
  }, [clusterId]);

  useEffect(() => {
    if (namespaced && !t.namespace) {
      setNames([]);
      return;
    }
    let live = true;
    setNamesNote("");
    yamlApi
      .names(clusterId, { group: t.group, version: t.version, resource: t.resource, namespace: namespaced ? t.namespace : "" })
      .then((r) => {
        if (!live) return;
        setNames(r.names);
        if (r.truncated) setNamesNote("The list is cut short.");
      })
      .catch(() => live && setNames([]));
    return () => {
      live = false;
    };
  }, [clusterId, t.group, t.version, t.resource, t.namespace, namespaced]);
  return (
    <form
      className="filters"
      onSubmit={(e) => {
        e.preventDefault();
        window.location.hash = yamlHref(clusterId, t);
      }}
    >
      <select
        value={kind ? kind.label : ""}
        onChange={(e) => {
          const k = YAML_KINDS.find((x) => x.label === e.target.value);
          if (k) setT({ ...t, group: k.group, version: k.version, resource: k.resource, namespace: k.namespaced ? t.namespace : "", name: "" });
        }}
        aria-label="Kind"
      >
        {!kind && <option value="">{`${t.resource}${t.group ? "." + t.group : ""}`}</option>}
        {YAML_KINDS.map((k) => (
          <option key={k.label}>{k.label}</option>
        ))}
      </select>
      {namespaced && (
        <select value={t.namespace} onChange={(e) => setT({ ...t, namespace: e.target.value, name: "" })} aria-label="Namespace">
          <option value="">Namespace…</option>
          {t.namespace && !namespaces.includes(t.namespace) && <option>{t.namespace}</option>}
          {namespaces.map((n) => (
            <option key={n}>{n}</option>
          ))}
        </select>
      )}
      <select
        value={t.name}
        onChange={(e) => setT({ ...t, name: e.target.value })}
        aria-label="Name"
        disabled={namespaced && !t.namespace}
        title={namesNote || undefined}
      >
        <option value="">Name…</option>
        {t.name && !names.includes(t.name) && <option>{t.name}</option>}
        {names.map((n) => (
          <option key={n}>{n}</option>
        ))}
      </select>
      <button type="submit" disabled={!t.name}>
        Show YAML
      </button>
    </form>
  );
}
