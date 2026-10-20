import { useCallback, useEffect, useState } from "react";
import { ApiError, chainApi, debugHref, type ChainItem, type ChainSection, type ObjectChain } from "./api";
import { errorMessage, usePoll } from "./common";
import { HintPanel } from "./HintPanel";
import { objectSignals } from "./hints";
import { logsHref } from "./PodsPage";
import { chainHref, itemLevel, parseId, SECTION_PLACE, SECTION_TEXT } from "./chain";

function describe(e: unknown): string {
  if (e instanceof ApiError && e.code === "agent_offline") return "The cluster agent is not connected.";
  if (e instanceof ApiError && e.code === "not_found") return "This object is not in the cluster any more (or never was).";
  return errorMessage(e);
}

/** One object in a list: a link to its own chain. */
function Item({ clusterId, it, owner }: { clusterId: string; it: ChainItem; owner?: string }) {
  const level = itemLevel(it);
  const { shortKind } = parseId(it.id);
  const sig = objectSignals(it);
  const ns = it.namespace && it.namespace !== owner ? `${it.namespace}/` : "";
  return (
    <li className={`chainitem ${it.missing ? "missing" : ""}`}>
      <span className={`dot lvl-${level}`} aria-hidden="true" />
      <span className="kind">{shortKind}</span>{" "}
      <a className="mono" href={chainHref(clusterId, it.id)} title={it.id}>
        {ns}
        {it.name}
      </a>
      {it.missing ? <span className="error small"> does not exist</span> : it.status && <span className="muted small"> {it.status}</span>}
      {it.detail && <div className="muted small detailline">{it.detail}</div>}
      {sig && <HintPanel signals={sig} />}
    </li>
  );
}

function Section({ clusterId, s, owner }: { clusterId: string; s: ChainSection; owner?: string }) {
  return (
    <div className={`card chainsection sec-${s.key}`}>
      <h3>{SECTION_TEXT[s.key] ?? s.key}</h3>
      <ul>
        {s.items.map((it) => (
          <Item key={it.id} clusterId={clusterId} it={it} owner={owner} />
        ))}
      </ul>
      {!!s.more && <p className="muted small">and {s.more} more</p>}
    </div>
  );
}

/** The owners as one line: Deployment web → ReplicaSet web-1 → (the object). */
function OwnerLine({ clusterId, owners }: { clusterId: string; owners: ChainItem[] }) {
  return (
    <p className="ownerline">
      {owners.map((o) => (
        <span key={o.id}>
          <span className="kind">{parseId(o.id).shortKind}</span>{" "}
          <a className="mono" href={chainHref(clusterId, o.id)}>
            {o.name}
          </a>{" "}
          <span className="arrow" aria-hidden="true">→</span>{" "}
        </span>
      ))}
      <span className="muted">this object</span>
    </p>
  );
}

function Search({ clusterId, initial }: { clusterId: string; initial?: string }) {
  const [q, setQ] = useState(initial ?? "");
  const [matches, setMatches] = useState<ChainItem[] | undefined>(undefined);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const find = async (text: string) => {
    if (!text.trim()) return;
    setBusy(true);
    setError("");
    try {
      setMatches((await chainApi.find(clusterId, text.trim())).chain.matches ?? []);
    } catch (e) {
      setError(describe(e));
    } finally {
      setBusy(false);
    }
  };
  return (
    <div className="card">
      <form
        className="filters"
        onSubmit={(e) => {
          e.preventDefault();
          find(q);
        }}
      >
        <input
          className="search"
          type="search"
          value={q}
          maxLength={200}
          onChange={(e) => setQ(e.target.value)}
          placeholder="Name of any resource, e.g. url-shortener or redis"
          aria-label="Find a resource"
        />
        <button className="primary" type="submit" disabled={busy || !q.trim()}>
          {busy ? "Finding…" : "Find"}
        </button>
      </form>
      {error && <div className="notice error">{error}</div>}
      {matches && matches.length === 0 && <p className="muted">Nothing matches “{q}”.</p>}
      {matches && matches.length > 0 && (
        <ul className="chainlist">
          {matches.map((m) => (
            <Item key={m.id} clusterId={clusterId} it={m} />
          ))}
        </ul>
      )}
    </div>
  );
}

/** #/clusters/<id>/chain[/<object id>]: everything one resource is connected to (KM-35). */
export function ChainPage({ clusterId, objectId }: { clusterId: string; objectId?: string }) {
  const load = useCallback(
    () => (objectId ? chainApi.get(clusterId, objectId) : Promise.resolve(undefined)),
    [clusterId, objectId],
  );
  const res = usePoll(load, 30000);
  const [chain, setChain] = useState<ObjectChain | undefined>(undefined);
  useEffect(() => setChain(res.data?.chain), [res.data]);

  const obj = chain?.object;
  const sections = chain?.sections ?? [];
  const owners = sections.find((s) => s.key === "owners");
  const place = (p: "up" | "down" | "side") => sections.filter((s) => s.key !== "owners" && SECTION_PLACE[s.key] === p);
  const up = place("up"), down = place("down"), side = place("side");
  const parsed = objectId ? parseId(objectId) : undefined;

  return (
    <section className="chainpage">
      <p>
        <a href={`#/clusters/${encodeURIComponent(clusterId)}`}>← Cluster</a>
      </p>
      <div className="pagehead">
        <h1>What is connected</h1>
      </div>
      <p className="muted small">
        Any resource with its whole chain: who owns it, the pods it comes down to, and around them the Services, routes,
        configuration, volumes, ServiceAccount, policies and nodes. Click anything to follow the chain from there.
      </p>
      <Search key={objectId ?? ""} clusterId={clusterId} initial={parsed?.name} />

      {objectId && res.error != null && !chain && <div className="notice error">{describe(res.error)}</div>}
      {objectId && !chain && res.error == null && <p className="muted">Loading…</p>}

      {obj && (
        <>
          {up.length > 0 && <div className="chaingrid up">{up.map((s) => <Section key={s.key} clusterId={clusterId} s={s} owner={obj.namespace} />)}</div>}
          {up.length > 0 && <div className="chainarrow" aria-hidden="true">↓</div>}

          <div className={`card chainobject lvl-${itemLevel(obj)}`}>
            {owners && <OwnerLine clusterId={clusterId} owners={owners.items} />}
            <h2>
              <span className="kind">{parseId(obj.id).shortKind}</span> <span className="mono">{obj.name}</span>
            </h2>
            <p className="muted">
              {obj.namespace ? `namespace ${obj.namespace}` : "cluster-wide"}
              {obj.status && ` · ${obj.status}`}
              {obj.missing && <span className="error"> · referred to, but it does not exist</span>}
            </p>
            {objectSignals(obj) && <HintPanel signals={objectSignals(obj)!} />}
            <div className="actions left">
              {(chain.apps ?? []).map((a) => (
                <a key={a} className="button" href={`#/clusters/${encodeURIComponent(clusterId)}/apps/${encodeURIComponent(a)}`}>
                  App map: {a}
                </a>
              ))}
              {obj.kind === "Pod" && obj.namespace && (
                <>
                  <a className="button" href={logsHref(clusterId, obj.namespace, obj.name)}>
                    Logs
                  </a>
                  <a className="button" href={debugHref(clusterId, obj.namespace, obj.name)}>
                    Debug this pod
                  </a>
                </>
              )}
            </div>
            {(chain.notes ?? []).map((n) => (
              <p key={n} className="notice attention small">
                {n}
              </p>
            ))}
            {sections.length === 0 && <p className="muted">Nothing else in the cluster refers to it, and it refers to nothing.</p>}
          </div>

          {down.length > 0 && <div className="chainarrow" aria-hidden="true">↓</div>}
          <div className="chainlayout">
            {down.length > 0 && <div className="chaingrid down">{down.map((s) => <Section key={s.key} clusterId={clusterId} s={s} owner={obj.namespace} />)}</div>}
            {side.length > 0 && <div className="chaingrid side">{side.map((s) => <Section key={s.key} clusterId={clusterId} s={s} owner={obj.namespace} />)}</div>}
          </div>
        </>
      )}
    </section>
  );
}
