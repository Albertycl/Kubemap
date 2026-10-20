import { useEffect, useState } from "react";
import {
  accessApi,
  api,
  type Cluster,
  type NamespaceView,
  type RBACGrant,
  type RBACQueryForm,
  type RBACResult,
  type RBACWho,
  type SubjectKind,
} from "./api";
import { CopyBox, errorMessage } from "./common";
import { canICommand, everyoneGrant, parseAccount, PLATFORM_GRANTS, PRESETS, refText, RISKY_TEXT, ruleText, splitRisky } from "./access";

type Tab = "account" | "who" | "namespace";

/** A tab's route; for the account tab, ns may name an account ("namespace/name") to open. */
export const tabHref = (clusterId: string, tab: Tab, ns?: string) => {
  const c = `#/clusters/${encodeURIComponent(clusterId)}/rbac`;
  if (tab === "account") return ns ? `${c}?account=${encodeURIComponent(ns)}` : c;
  return tab === "who" ? `${c}/who` : `${c}/ns/${encodeURIComponent(ns || "monitoring")}`;
};

/**
 * #/clusters/<id>/rbac[/who|/ns/<ns>]: RBAC views (KM-37). An account's
 * rights through its bindings, who can do an action, and one namespace's
 * permissions (monitoring by default).
 */
export function AccessPage({
  clusterId,
  tab,
  namespace,
  account,
}: {
  clusterId: string;
  tab: Tab;
  namespace?: string;
  account?: string;
}) {
  const [cluster, setCluster] = useState<Cluster | undefined>();
  useEffect(() => {
    api.cluster(clusterId).then(setCluster, () => {});
  }, [clusterId]);
  return (
    <section className="access">
      <p>
        <a href={`#/clusters/${encodeURIComponent(clusterId)}`}>← {cluster?.name ?? "Cluster"}</a>
      </p>
      <div className="pagehead">
        <h1>Who can do what</h1>
        <a className="button" href={`#/clusters/${encodeURIComponent(clusterId)}/argocd`}>
          Argo CD apps
        </a>
      </div>
      <p className="muted lead">
        Answers come from the cluster's Roles, ClusterRoles and bindings as the cluster agent sees them, with the rules of
        the Kubernetes RBAC authorizer. Nothing is changed in the cluster.
      </p>
      <nav className="tabs">
        <a className={tab === "account" ? "active" : ""} href={tabHref(clusterId, "account")}>
          An account's rights
        </a>
        <a className={tab === "who" ? "active" : ""} href={tabHref(clusterId, "who")}>
          Who can …
        </a>
        <a className={tab === "namespace" ? "active" : ""} href={tabHref(clusterId, "namespace", namespace)}>
          Namespace permissions
        </a>
      </nav>
      {tab === "account" && <AccountTab key={account} clusterId={clusterId} account={account} />}
      {tab === "who" && <WhoTab clusterId={clusterId} />}
      {tab === "namespace" && <NamespaceTab key={namespace} clusterId={clusterId} namespace={namespace || "monitoring"} />}
    </section>
  );
}

function useAsk(clusterId: string) {
  const [result, setResult] = useState<RBACResult | undefined>();
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const ask = async (form: RBACQueryForm) => {
    setBusy(true);
    setError(null);
    try {
      setResult((await accessApi.query(clusterId, form)).result);
    } catch (e) {
      setResult(undefined);
      setError(e);
    } finally {
      setBusy(false);
    }
  };
  return { result, error, busy, ask };
}

function ActionFields({ form, set }: { form: RBACQueryForm; set: (f: RBACQueryForm) => void }) {
  return (
    <>
      <div className="presets">
        {PRESETS.map((p) => (
          <button key={p.label} className="chip" onClick={() => set({ ...form, verb: p.form.verb, resource: p.form.resource, apiGroup: p.form.apiGroup ?? "" })}>
            {p.label}
          </button>
        ))}
      </div>
      <div className="context-fields">
        <label>
          Verb
          <input value={form.verb ?? ""} placeholder="get" onChange={(e) => set({ ...form, verb: e.target.value.trim() })} />
        </label>
        <label>
          Resource
          <input value={form.resource ?? ""} placeholder="secrets, pods/exec" onChange={(e) => set({ ...form, resource: e.target.value.trim() })} />
        </label>
        <label>
          API group <span className="muted small">(empty: core)</span>
          <input value={form.apiGroup ?? ""} placeholder="apps" onChange={(e) => set({ ...form, apiGroup: e.target.value.trim() })} />
        </label>
        <label>
          Namespace <span className="muted small">(empty: cluster-wide)</span>
          <input value={form.namespace ?? ""} onChange={(e) => set({ ...form, namespace: e.target.value.trim() })} />
        </label>
        <label>
          Object name <span className="muted small">(optional)</span>
          <input value={form.resourceName ?? ""} onChange={(e) => set({ ...form, resourceName: e.target.value.trim() })} />
        </label>
      </div>
    </>
  );
}

function AccountTab({ clusterId, account }: { clusterId: string; account?: string }) {
  const [accounts, setAccounts] = useState<{ namespace: string; name: string }[]>([]);
  const [kind, setKind] = useState<SubjectKind>("ServiceAccount");
  const [who, setWho] = useState(account ?? "");
  const [groups, setGroups] = useState("");
  const [check, setCheck] = useState<RBACQueryForm>({});
  const chain = useAsk(clusterId);
  const verdict = useAsk(clusterId);
  useEffect(() => {
    accessApi.accounts(clusterId).then((r) => setAccounts(r.accounts), () => setAccounts([]));
    // Opened from a link that names the account: show its roles at once.
    const a = account ? parseAccount(account) : null;
    if (a) chain.ask({ subjectKind: "ServiceAccount", subject: a.name, subjectNamespace: a.namespace });
  }, [clusterId]);

  const subject = (): RBACQueryForm | null => {
    if (kind === "ServiceAccount") {
      const a = parseAccount(who);
      return a ? { subjectKind: kind, subject: a.name, subjectNamespace: a.namespace } : null;
    }
    return who.trim() ? { subjectKind: kind, subject: who.trim(), groups: kind === "User" ? groups : undefined } : null;
  };
  const s = subject();
  const checkForm = s ? { ...check, ...s } : null;
  const cmd = checkForm ? canICommand(checkForm) : null;

  return (
    <>
      <div className="card">
        <h2>Pick an account</h2>
        <div className="context-fields">
          <label>
            Kind
            <select value={kind} onChange={(e) => setKind(e.target.value as SubjectKind)}>
              <option value="ServiceAccount">ServiceAccount</option>
              <option value="User">User</option>
              <option value="Group">Group</option>
            </select>
          </label>
          <label>
            {kind === "ServiceAccount" ? "namespace/name" : kind === "User" ? "User name" : "Group name"}
            <input
              list={kind === "ServiceAccount" ? "rbac-accounts" : undefined}
              value={who}
              placeholder={kind === "ServiceAccount" ? "monitoring/kube-prometheus-stack-grafana" : kind === "User" ? "alice" : "system:authenticated"}
              autoComplete="off"
              data-lpignore="true"
              data-1p-ignore="true"
              onChange={(e) => setWho(e.target.value)}
            />
            <datalist id="rbac-accounts">
              {accounts.map((a) => (
                <option key={`${a.namespace}/${a.name}`} value={`${a.namespace}/${a.name}`} />
              ))}
            </datalist>
          </label>
          {kind === "User" && (
            <label>
              Groups <span className="muted small">(comma-separated; only the authenticator knows them)</span>
              <input value={groups} onChange={(e) => setGroups(e.target.value)} placeholder="dev-team" />
            </label>
          )}
        </div>
        {kind === "ServiceAccount" && who && !s && <p className="small error">Write it as namespace/name.</p>}
        <div className="actions left">
          <button className="primary" disabled={!s || chain.busy} onClick={() => s && chain.ask(s)}>
            {chain.busy ? "Reading…" : "Show its roles"}
          </button>
        </div>
      </div>
      {chain.error != null && <div className="notice error">{errorMessage(chain.error)}</div>}
      {chain.result && <ChainView result={chain.result} />}

      <div className="card">
        <h2>May it do this?</h2>
        <ActionFields form={check} set={setCheck} />
        <div className="actions left">
          <button className="primary" disabled={!checkForm || !check.verb || !check.resource || verdict.busy} onClick={() => checkForm && verdict.ask(checkForm)}>
            {verdict.busy ? "Checking…" : "Check"}
          </button>
          {!s && <span className="muted small">Pick an account first.</span>}
        </div>
        {verdict.error != null && <div className="notice error small">{errorMessage(verdict.error)}</div>}
        {verdict.result && verdict.result.mode === "check" && (
          <div className={`check-result verdict-${verdict.result.allowed ? "bad" : "good"}`}>
            <span className={`badge level-${verdict.result.allowed ? "warn" : "ok"}`}>{verdict.result.allowed ? "Allowed" : "Not allowed"}</span>{" "}
            {verdict.result.allowed && verdict.result.grants?.[0] && (
              <span>
                by {refText(verdict.result.grants[0].binding)} → {refText(verdict.result.grants[0].role)}
                {verdict.result.grants[0].via !== "direct" && <span className="muted"> (via {verdict.result.grants[0].via})</span>}
              </span>
            )}
            <Notes notes={verdict.result.notes} />
          </div>
        )}
        {cmd && <CopyBox text={cmd} label="The same question to the API server" />}
      </div>
    </>
  );
}

function Notes({ notes }: { notes?: string[] }) {
  if (!notes?.length) return null;
  return (
    <ul className="muted small notes">
      {notes.map((n) => (
        <li key={n}>{n}</li>
      ))}
    </ul>
  );
}

/** Risky rights every account has: the worst case, shown first and in red. */
function EveryoneNotice({ risky }: { risky: string[] }) {
  return (
    <div className="notice error">
      <strong>Every account in the cluster can: {risky.join(", ")}.</strong> A binding gives these to a group that contains
      everyone (system:authenticated or system:serviceaccounts). Find it in the grants marked "every account" and narrow it.
    </div>
  );
}

function RiskyList({ risky }: { risky?: string[] }) {
  if (!risky?.length) return <span className="badge level-ok">no admin-level rights</span>;
  return (
    <span className="risky">
      {risky.map((r) => (
        <span key={r} className="badge level-fail" title={RISKY_TEXT[r] ?? ""}>
          {r}
        </span>
      ))}
    </span>
  );
}

function ChainView({ result }: { result: RBACResult }) {
  const grants = result.grants ?? [];
  const s = result.subject;
  const { own, everyone, platform } = splitRisky(grants);
  return (
    <div className="card">
      <div className="pagehead">
        <h2>
          {s?.kind} {s?.namespace ? `${s.namespace}/` : ""}
          {s?.name}
        </h2>
        <RiskyList risky={own} />
      </div>
      {everyone.length > 0 && <EveryoneNotice risky={everyone} />}
      {platform.length > 0 && (
        <p className="muted small">
          Through a known platform grant to every account, it also has: {platform.join(", ")} (see the grants marked "platform").
        </p>
      )}
      {(result.groups?.length ?? 0) > 0 && (
        <p className="muted small">
          Evaluated with the groups: {result.groups!.map((g) => <code key={g}>{g} </code>)}
        </p>
      )}
      {own.length > 0 && (
        <ul className="small">
          {own.map((r) => (
            <li key={r}>
              <strong>{r}</strong>: {RISKY_TEXT[r] ?? ""}
            </li>
          ))}
        </ul>
      )}
      {grants.length === 0 ? (
        <p className="muted">No binding applies to this account: it may do nothing through RBAC.</p>
      ) : (
        <ol className="chain">
          {grants.map((g, i) => (
            <GrantItem key={i} g={g} />
          ))}
        </ol>
      )}
      <Notes notes={result.notes} />
    </div>
  );
}

function GrantItem({ g }: { g: RBACGrant }) {
  return (
    <li className={`grant ${g.risky?.length ? "risky-grant" : ""}`}>
      <div className="grant-head">
        <span>{refText(g.binding)}</span>
        <span className="muted">→</span>
        <strong>{refText(g.role)}</strong>
        <span className="badge">{g.scope === "cluster" ? "cluster-wide" : `in ${g.scope}`}</span>
        {g.via !== "direct" && <span className="muted small">via {g.via}</span>}
        {everyoneGrant(g) &&
          (PLATFORM_GRANTS[g.binding] ? (
            <span className="badge" title={PLATFORM_GRANTS[g.binding]}>
              platform grant
            </span>
          ) : (
            <span className={`badge ${g.risky?.length ? "level-fail" : ""}`}>every account</span>
          ))}
        {g.roleMissing && <span className="badge level-warn" title="A role with this name created later would be granted at once">role does not exist</span>}
        {g.risky?.map((r) => (
          <span key={r} className="badge level-fail" title={RISKY_TEXT[r] ?? ""}>
            {r}
          </span>
        ))}
      </div>
      {(g.rules?.length ?? 0) > 0 && (
        <details>
          <summary className="small muted">{g.rules!.length} rule(s)</summary>
          <ul className="rules small">
            {g.rules!.map((r, i) => (
              <li key={i}>
                <code>{ruleText(r)}</code>
              </li>
            ))}
          </ul>
        </details>
      )}
    </li>
  );
}

function WhoTab({ clusterId }: { clusterId: string }) {
  const [form, setForm] = useState<RBACQueryForm>({ verb: "get", resource: "secrets", namespace: "monitoring" });
  const q = useAsk(clusterId);
  return (
    <>
      <div className="card">
        <h2>Who can …</h2>
        <ActionFields form={form} set={setForm} />
        <div className="actions left">
          <button className="primary" disabled={!form.verb || !form.resource || q.busy} onClick={() => q.ask(form)}>
            {q.busy ? "Reading…" : "Find who can"}
          </button>
        </div>
      </div>
      {q.error != null && <div className="notice error">{errorMessage(q.error)}</div>}
      {q.result && <WhoList subjects={q.result.subjects ?? []} notes={q.result.notes} />}
    </>
  );
}

function subjectText(s: { kind: string; name: string; namespace?: string }) {
  return `${s.kind} ${s.namespace ? s.namespace + "/" : ""}${s.name}`;
}

function WhoList({ subjects, notes }: { subjects: RBACWho[]; notes?: string[] }) {
  return (
    <div className="card flush">
      <table className="data compact">
        <thead>
          <tr>
            <th>Who</th>
            <th>Through</th>
            <th>Where</th>
          </tr>
        </thead>
        <tbody>
          {subjects.map((s, i) => (
            <tr key={i} className={s.everyone ? "everyone" : ""}>
              <td>
                {subjectText(s)}
                {s.everyone && <span className="badge level-fail"> everyone</span>}
                {s.resourceNames && <div className="muted small">only: {s.resourceNames.join(", ")}</div>}
              </td>
              <td className="small">
                {refText(s.binding)} → {refText(s.role)}
              </td>
              <td className="small">{s.scope === "cluster" ? "cluster-wide" : s.scope}</td>
            </tr>
          ))}
          {subjects.length === 0 && (
            <tr>
              <td colSpan={3} className="muted">
                Nobody, through RBAC.
              </td>
            </tr>
          )}
        </tbody>
      </table>
      <div className="pad">
        <Notes notes={notes} />
      </div>
    </div>
  );
}

function NamespaceTab({ clusterId, namespace }: { clusterId: string; namespace: string }) {
  const [view, setView] = useState<NamespaceView | undefined>();
  const [error, setError] = useState<unknown>(null);
  const [ns, setNs] = useState(namespace);
  useEffect(() => {
    accessApi.namespace(clusterId, namespace).then((r) => setView(r.view), setError);
  }, [clusterId, namespace]);
  return (
    <>
      <div className="card">
        <form
          className="actions left"
          onSubmit={(e) => {
            e.preventDefault();
            if (ns.trim()) window.location.hash = tabHref(clusterId, "namespace", ns.trim());
          }}
        >
          <label className="inline">
            Namespace <input value={ns} onChange={(e) => setNs(e.target.value)} />
          </label>
          <button className="primary" type="submit">
            Show
          </button>
          {namespace !== "monitoring" && <a href={tabHref(clusterId, "namespace", "monitoring")}>monitoring</a>}
        </form>
        <p className="muted small">Who may do the risky actions in this namespace, and what the namespace's own ServiceAccounts may do anywhere.</p>
      </div>
      {error != null && <div className="notice error">{errorMessage(error)}</div>}
      {!view && !error && <p className="muted">Asking the agent…</p>}
      {view && (
        <>
          {(view.everyoneRisky?.length ?? 0) > 0 && <EveryoneNotice risky={view.everyoneRisky!} />}
          <div className="card flush tablewrap">
            <table className="data compact perm-matrix">
              <thead>
                <tr>
                  <th>Who</th>
                  {view.actions.map((a) => (
                    <th key={a.key} title={`${a.verb} ${a.resource}: ${a.why}`}>
                      {a.label}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {view.subjects.map((s) => (
                  <tr key={subjectText(s)}>
                    <td>
                      {subjectText(s)}
                      {s.everyone && <span className="badge level-fail"> everyone</span>}
                    </td>
                    {view.actions.map((a) => {
                      const g = s.grants[a.key];
                      if (!g) return <td key={a.key} className="cell-no" aria-label="no" />;
                      const title = `${refText(g.binding)} → ${refText(g.role)}${g.resourceNames ? ` (only ${g.resourceNames.join(", ")})` : ""}`;
                      return (
                        <td key={a.key} className={g.resourceNames ? "cell-some" : "cell-yes"} title={title}>
                          {g.resourceNames ? "named" : "yes"}
                        </td>
                      );
                    })}
                  </tr>
                ))}
                {view.errors && (
                  <tr>
                    <td className="muted small">no answer</td>
                    {view.actions.map((a) => (
                      <td key={a.key} className="error small">
                        {view.errors![a.key] ?? ""}
                      </td>
                    ))}
                  </tr>
                )}
              </tbody>
            </table>
          </div>
          <p className="muted small">Hover a cell for the binding and role. "named": only the objects the rule names.</p>
          <div className="card">
            <h2>ServiceAccounts in {view.namespace}</h2>
            {view.accounts.length === 0 ? (
              <p className="muted">None.</p>
            ) : (
              <table className="data compact">
                <tbody>
                  {view.accounts.map((a) => (
                    <tr key={a.name}>
                      <td>
                        <a href={tabHref(clusterId, "account", `${view.namespace}/${a.name}`)}>
                          {a.name}
                        </a>
                      </td>
                      <td className="muted small">{a.error ?? `${a.grants} binding(s)`}</td>
                      <td>
                        {!a.error && <RiskyList risky={a.risky} />}
                        {(a.everyone?.length ?? 0) > 0 && <span className="error small"> · as every account: {a.everyone!.join(", ")}</span>}
                        {(a.platform?.length ?? 0) > 0 && <span className="muted small"> · platform grant: {a.platform!.join(", ")}</span>}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>
          <Notes notes={view.notes} />
        </>
      )}
    </>
  );
}
