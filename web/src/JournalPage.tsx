import { useEffect, useRef, useState, type ReactNode } from "react";
import {
  api,
  ApiError,
  draftApi,
  journalApi,
  type IncidentDraft,
  type AuditEntry,
  type ChangeRow,
  type Cluster,
  type Decision,
  type EvidenceKind,
  type FollowUp,
  type JournalEntry,
  type JournalEvidence,
  type JournalKind,
  type JournalRecord,
  type JournalSection,
  type JournalSummary,
  type SectionKind,
  type Star,
  type TimelineRow,
} from "./api";
import { errorMessage } from "./common";
import { auditGroup, resultLevel, type AuditGroup } from "./audit";
import {
  blocks,
  emptySection,
  inline,
  interview,
  isTextKind,
  journalHref,
  move,
  newRecord,
  recordProblem,
  removeAt,
  replaceAt,
  safeLink,
  SECTION_KINDS,
  SEVERITIES,
  showDate,
  sourceName,
  sourceText,
  STATUS_TEXT,
  today,
} from "./journal";

// ---------------------------------------------------------------- list

/** #/journal: every incident and change record, newest first. */
export function JournalList() {
  const [entries, setEntries] = useState<JournalSummary[] | undefined>();
  const [error, setError] = useState<unknown>(null);
  const [importing, setImporting] = useState(false);
  const load = () => journalApi.list().then(setEntries, setError);
  useEffect(() => {
    load();
  }, []);

  return (
    <section>
      <div className="pagehead">
        <h1>Incident journal</h1>
        <a className="button primary" href="#/journal/new">
          New incident
        </a>
        <a className="button" href="#/journal/new?kind=changes">
          New change record
        </a>
        <button onClick={() => setImporting((v) => !v)}>{importing ? "Close import" : "Import Markdown"}</button>
      </div>
      <p className="muted lead">
        Real problems and changes: what happened, how it was found, what you decided and why, and what it cost. Attach
        evidence from Kubemap, and use the interview view to tell the story in STAR form.
      </p>
      {importing && <ImportPanel onImported={load} />}
      <DraftList written={new Map((entries ?? []).filter((e) => e.source.startsWith("draft:")).map((e) => [e.source.slice(6), e.id]))} />
      {error != null && <div className="notice error">{errorMessage(error)}</div>}
      {entries && entries.length === 0 && (
        <div className="card empty">
          <p>No entries yet.</p>
          <p className="muted small">
            Import <code>docs/incidents/*.md</code> and <code>docs/changes/*.md</code>, or start a new incident.
          </p>
        </div>
      )}
      {entries && entries.length > 0 && (
        <div className="card flush">
          <table className="data clickable">
            <thead>
              <tr>
                <th>Date</th>
                <th>Title</th>
                <th>Kind</th>
                <th>Cluster</th>
                <th>Severity</th>
                <th>Last edit</th>
              </tr>
            </thead>
            <tbody>
              {entries.map((e) => (
                <tr key={e.id} onClick={() => (window.location.hash = journalHref(e.id))}>
                  <td className="nowrap">{showDate(e.date) || "—"}</td>
                  <td>
                    <a href={journalHref(e.id)} onClick={(ev) => ev.stopPropagation()}>
                      {e.title}
                    </a>
                    {e.source && <div className="muted small">{sourceText(e.source)}</div>}
                  </td>
                  <td>
                    <KindBadge kind={e.kind} />
                  </td>
                  <td>{e.cluster || "—"}</td>
                  <td>{e.severity ? <SeverityBadge severity={e.severity} /> : "—"}</td>
                  <td className="muted small">
                    {e.updatedBy}, {new Date(e.updatedAt).toLocaleString()}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

/** KM-90's drafts: what Kubemap saw by itself, ready to be written up. */
function DraftList({ written }: { written: Map<string, string> }) {
  const [drafts, setDrafts] = useState<IncidentDraft[] | undefined>();
  const [error, setError] = useState<unknown>(null);
  const [busy, setBusy] = useState("");
  useEffect(() => {
    draftApi.list().then(setDrafts, setError);
  }, []);
  const writeUp = async (d: IncidentDraft) => {
    setBusy(d.id);
    setError(null);
    try {
      const e = await draftApi.writeUp(d.id);
      window.location.hash = journalHref(e.id, "edit");
    } catch (err) {
      const id = err instanceof ApiError && err.code === "exists" ? written.get(d.id) : undefined;
      if (id) window.location.hash = journalHref(id);
      else setError(err);
    } finally {
      setBusy("");
    }
  };
  if (error == null && (!drafts || drafts.length === 0)) return null;
  return (
    <div className="card drafts">
      <h2>Drafts from Kubemap</h2>
      <p className="muted small">
        When the problem finder sees a critical finding, Kubemap opens a draft and keeps its timeline. Write it up to add
        what you did, your decisions and the lessons.
      </p>
      {error != null && <div className="notice error small">{errorMessage(error)}</div>}
      {drafts && drafts.length > 0 && (
        <table className="data compact">
          <tbody>
            {drafts.map((d) => {
              const entry = written.get(d.id);
              return (
                <tr key={d.id}>
                  <td>
                    <span className={`badge level-${d.status === "open" ? "fail" : "ok"}`}>{d.status}</span>
                  </td>
                  <td>
                    <strong>{d.title}</strong>
                    <div className="muted small">
                      {d.clusterName} · started {new Date(d.started).toLocaleString()}
                      {d.resolved && <> · cleared {new Date(d.resolved).toLocaleString()}</>} · {d.timeline.length} timeline line(s)
                    </div>
                  </td>
                  <td>{d.severity && <SeverityBadge severity={d.severity === "warning" ? "major" : d.severity} />}</td>
                  <td className="nowrap">
                    {entry ? (
                      <a href={journalHref(entry)}>Open the write-up</a>
                    ) : (
                      <button onClick={() => writeUp(d)} disabled={busy !== ""}>
                        {busy === d.id ? "Writing…" : "Write it up"}
                      </button>
                    )}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </div>
  );
}

function KindBadge({ kind }: { kind: JournalKind }) {
  return <span className="badge">{kind === "changes" ? "Changes" : "Incident"}</span>;
}

function SeverityBadge({ severity }: { severity: string }) {
  const level = severity === "critical" ? "fail" : severity === "major" ? "warn" : "info";
  return <span className={`badge level-${level}`}>{severity}</span>;
}

type ImportResult = { name: string; id?: string; exact?: boolean; error?: string; existing?: string; markdown?: string };

/** Import Markdown files (docs/incidents, docs/changes) or pasted text. */
function ImportPanel({ onImported }: { onImported: () => void }) {
  const [results, setResults] = useState<ImportResult[]>([]);
  const [paste, setPaste] = useState("");
  const [pasteName, setPasteName] = useState("");
  const [busy, setBusy] = useState(false);

  const one = async (name: string, markdown: string, replace = false): Promise<ImportResult> => {
    try {
      const r = await journalApi.importMarkdown(markdown, name, replace);
      return { name: name || r.entry.title, id: r.entry.id, exact: r.exact };
    } catch (e) {
      if (e instanceof ApiError && e.code === "exists") return { name, error: e.message, existing: "yes", markdown };
      return { name, error: errorMessage(e) };
    }
  };
  const run = async (items: { name: string; markdown: string }[]) => {
    setBusy(true);
    const out: ImportResult[] = [];
    for (const it of items) out.push(await one(it.name, it.markdown));
    setResults(out);
    setBusy(false);
    onImported();
  };
  const replace = async (i: number) => {
    const r = results[i];
    const res = await one(r.name, r.markdown ?? "", true);
    setResults((rs) => replaceAt(rs, i, res));
    onImported();
  };

  return (
    <div className="card">
      <h2>Import Markdown</h2>
      <p className="muted small">
        Files written like <code>docs/incidents/2026-09-28-disk-pressure.md</code>: a <code># Incident: …</code> title,
        then <code>## </code> sections. Timeline, decisions, follow-ups and STAR tables become editable rows; everything
        else is kept as text. The file name is remembered, so a second import of the same file asks first.
      </p>
      <label>
        Files
        <input
          type="file"
          accept=".md,text/markdown"
          multiple
          disabled={busy}
          onChange={async (e) => {
            const files = Array.from(e.target.files ?? []);
            e.target.value = "";
            await run(await Promise.all(files.map(async (f) => ({ name: sourceName(f.name), markdown: await f.text() }))));
          }}
        />
      </label>
      <details className="paste">
        <summary>Or paste the text</summary>
        <label>
          File name (optional, such as 2026-10-02-ingress-503.md)
          <input value={pasteName} onChange={(e) => setPasteName(e.target.value.trim())} onBlur={() => setPasteName(sourceName(pasteName))} />
        </label>
        <label>
          Markdown
          <textarea rows={8} value={paste} onChange={(e) => setPaste(e.target.value)} />
        </label>
        <div className="actions left">
          <button className="primary" disabled={busy || !paste.trim()} onClick={() => run([{ name: pasteName, markdown: paste }])}>
            Import
          </button>
        </div>
      </details>
      {results.length > 0 && (
        <ul className="import-results">
          {results.map((r, i) => (
            <li key={i}>
              <strong>{r.name}</strong>:{" "}
              {r.id ? (
                <>
                  imported, <a href={journalHref(r.id)}>open it</a>
                  {r.exact ? (
                    <span className="muted small"> · exports back to the same file</span>
                  ) : (
                    <span className="warn small"> · exports with small layout changes (spacing, headings)</span>
                  )}
                </>
              ) : (
                <>
                  <span className="error">{r.error}</span>
                  {r.existing && (
                    <button className="link" onClick={() => replace(i)}>
                      Replace it (evidence stays)
                    </button>
                  )}
                </>
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// ---------------------------------------------------------------- record

function useEntry(id: string) {
  const [data, setData] = useState<{ entry: JournalEntry; evidence: JournalEvidence[] } | undefined>();
  const [error, setError] = useState<unknown>(null);
  const load = () => journalApi.get(id).then(setData, setError);
  useEffect(() => {
    load();
  }, [id]);
  return { data, error, reload: load };
}

function NotFound({ error }: { error: unknown }) {
  const nf = error instanceof ApiError && error.status === 404;
  return (
    <section>
      <p>
        <a href="#/journal">← Journal</a>
      </p>
      <div className="notice error">{nf ? "No such journal entry." : errorMessage(error)}</div>
    </section>
  );
}

/** #/journal/<id>: the full record with its evidence. */
export function JournalView({ id }: { id: string }) {
  const { data, error, reload } = useEntry(id);
  const [exporting, setExporting] = useState(false);
  if (error) return <NotFound error={error} />;
  if (!data) return <p className="muted">Loading…</p>;
  const e = data.entry;

  const remove = async () => {
    if (!window.confirm(`Delete "${e.title}" and its evidence? Export it first if you want to keep a copy.`)) return;
    try {
      await journalApi.remove(e.id);
      window.location.hash = "#/journal";
    } catch (err) {
      window.alert(errorMessage(err));
    }
  };

  return (
    <section className="journal">
      <p>
        <a href="#/journal">← Journal</a>
      </p>
      <div className="pagehead">
        <h1>{e.kind === "incident" ? `Incident: ${e.title}` : e.title}</h1>
        {e.kind === "incident" && (
          <a className="button primary" href={journalHref(e.id, "interview")}>
            Interview view
          </a>
        )}
        <a className="button" href={journalHref(e.id, "edit")}>
          Edit
        </a>
        <button onClick={() => setExporting((v) => !v)}>{exporting ? "Close export" : "Export Markdown"}</button>
        <button className="danger" onClick={remove}>
          Delete
        </button>
      </div>
      <Meta e={e} />
      {exporting && <ExportPanel id={e.id} hasEvidence={data.evidence.length > 0} />}
      {e.sections.map((s, i) => (
        <SectionView key={i} s={s} />
      ))}
      {e.sections.length === 0 && <p className="muted">Nothing written yet. Use Edit to add sections.</p>}
      <EvidencePanel entryId={e.id} evidence={data.evidence} onChange={reload} />
    </section>
  );
}

function Meta({ e }: { e: JournalEntry }) {
  return (
    <p className="muted small journal-meta">
      <KindBadge kind={e.kind} /> {e.date && <>· {showDate(e.date)} </>}
      {e.cluster && (
        <>
          · cluster <code>{e.cluster}</code>{" "}
        </>
      )}
      {e.severity && (
        <>
          · <SeverityBadge severity={e.severity} />{" "}
        </>
      )}
      · created by {e.createdBy} · last edit {e.updatedBy}, {new Date(e.updatedAt).toLocaleString()} (version {e.version})
      {e.source && <> · {sourceText(e.source)}</>}
    </p>
  );
}

function ExportPanel({ id, hasEvidence }: { id: string; hasEvidence: boolean }) {
  const [withEvidence, setWithEvidence] = useState(false);
  const [md, setMd] = useState<{ text: string; filename: string } | undefined>();
  const [error, setError] = useState<unknown>(null);
  const [copied, setCopied] = useState(false);
  useEffect(() => {
    journalApi.exportMarkdown(id, withEvidence).then(setMd, setError);
  }, [id, withEvidence]);
  const download = () => {
    if (!md) return;
    const url = URL.createObjectURL(new Blob([md.text], { type: "text/markdown" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = md.filename;
    a.click();
    URL.revokeObjectURL(url);
  };
  return (
    <div className="card">
      <h2>Export Markdown</h2>
      <p className="muted small">
        The same layout as <code>docs/incidents/</code>: importing this file again gives the same record.
      </p>
      {hasEvidence && (
        <label className="inline">
          <input type="checkbox" checked={withEvidence} onChange={(e) => setWithEvidence(e.target.checked)} />
          List the evidence at the end (a new import keeps that list as text)
        </label>
      )}
      {hasEvidence && (
        <p className="small warn">Evidence may contain cluster details (names, addresses, messages): review the file before sharing it.</p>
      )}
      {error != null && <div className="notice error">{errorMessage(error)}</div>}
      {md && (
        <>
          <pre className="markdown-export">{md.text}</pre>
          <div className="actions left">
            <button className="primary" onClick={download}>
              Download {md.filename}
            </button>
            <button
              onClick={() =>
                navigator.clipboard?.writeText(md.text).then(() => {
                  setCopied(true);
                  setTimeout(() => setCopied(false), 1500);
                })
              }
            >
              {copied ? "Copied" : "Copy"}
            </button>
          </div>
        </>
      )}
    </div>
  );
}

function Inline({ text }: { text: string }) {
  return (
    <>
      {inline(text).map((s, i) =>
        s.t === "code" ? <code key={i}>{s.v}</code> : s.t === "b" ? <strong key={i}>{s.v}</strong> : <span key={i}>{s.v}</span>,
      )}
    </>
  );
}

/** Markdown text as blocks; never as HTML. */
export function MarkdownText({ text }: { text: string }) {
  return (
    <>
      {blocks(text).map((b, i) => {
        switch (b.type) {
          case "p":
            return (
              <p key={i}>
                <Inline text={b.text} />
              </p>
            );
          case "ul":
          case "ol": {
            const items = b.items.map((it, j) => (
              <li key={j}>
                <Inline text={it} />
              </li>
            ));
            return b.type === "ul" ? <ul key={i}>{items}</ul> : <ol key={i}>{items}</ol>;
          }
          case "code":
            return <pre key={i}>{b.text}</pre>;
          case "table":
            return (
              <Table
                key={i}
                head={b.head}
                rows={b.rows.map((r) => r.map((c, k) => <Inline key={k} text={c} />))}
              />
            );
        }
      })}
    </>
  );
}

function Table({ head, rows, className }: { head: string[]; rows: ReactNode[][]; className?: string }) {
  return (
    <div className="tablewrap">
      <table className={`data compact journal-table ${className ?? ""}`}>
        <thead>
          <tr>
            {head.map((h) => (
              <th key={h}>{h}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <tr key={i}>
              {r.map((c, j) => (
                <td key={j}>{c}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

const md = (s: string) => <Inline text={s} />;

function SectionView({ s }: { s: JournalSection }) {
  let body: ReactNode;
  switch (s.kind) {
    case "timeline":
      body = <Table className="timeline" head={["Time", "Event"]} rows={(s.timeline ?? []).map((r) => [r.time, md(r.event)])} />;
      break;
    case "decisions":
      body = (
        <Table
          head={["#", "Decision", "Why", "Trade-off"]}
          rows={(s.decisions ?? []).map((d, i) => [String(i + 1), md(d.decision), md(d.why), md(d.tradeOff)])}
        />
      );
      break;
    case "changes":
      body = (
        <Table
          head={["Time", "Change", "Why", "How it was checked", "Undo"]}
          rows={(s.changes ?? []).map((c) => [c.time, md(c.change), md(c.why), md(c.checked), md(c.undo)])}
        />
      );
      break;
    case "followups":
      body = (
        <ul className="followups">
          {(s.followUps ?? []).map((f, i) => (
            <li key={i}>
              <span className={`badge level-${f.status === "done" ? "ok" : f.status === "doing" ? "info" : "warn"}`}>
                {STATUS_TEXT[f.status] ?? f.status}
              </span>{" "}
              {f.owner && <span className="muted small">{f.owner}: </span>}
              <Inline text={f.text} />
            </li>
          ))}
        </ul>
      );
      break;
    case "star":
      body = <StarView star={s.star} />;
      break;
    default:
      body = <MarkdownText text={s.text ?? ""} />;
  }
  return (
    <div className={`journal-section section-${s.kind}`}>
      {s.heading && <h2>{s.heading}</h2>}
      {body}
    </div>
  );
}

function StarView({ star, big }: { star?: Star | null; big?: boolean }) {
  const parts: [keyof Star, string][] = [
    ["situation", "Situation"],
    ["task", "Task"],
    ["action", "Action"],
    ["result", "Result"],
  ];
  return (
    <div className={`star ${big ? "big" : ""}`}>
      {parts.map(([k, label]) => (
        <div key={k} className="star-part">
          <div className="star-letter" aria-hidden="true">
            {label[0]}
          </div>
          <div>
            <h3>{label}</h3>
            {star?.[k] ? (
              <p>
                <Inline text={star[k]} />
              </p>
            ) : (
              <p className="muted">Not written yet.</p>
            )}
          </div>
        </div>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------- evidence

const EVIDENCE_TEXT: Record<EvidenceKind, string> = {
  findings: "Findings (problem finder)",
  events: "Warning events",
  metrics: "Node metrics (latest and last hour)",
  audit: "Audit log entries",
};

function EvidencePanel({ entryId, evidence, onChange }: { entryId: string; evidence: JournalEvidence[]; onChange: () => void }) {
  const [clusters, setClusters] = useState<Cluster[]>([]);
  const [kind, setKind] = useState<EvidenceKind>("findings");
  const [clusterId, setClusterId] = useState("");
  const [scope, setScope] = useState("");
  const [note, setNote] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  useEffect(() => {
    api.clusters().then(
      (list) => {
        const usable = list;
        setClusters(usable);
        if (usable.length > 0) setClusterId((id) => id || usable[0].id);
      },
      () => setClusters([]),
    );
  }, []);

  const capture = async () => {
    setBusy(true);
    setError(null);
    try {
      await journalApi.addEvidence(entryId, {
        kind,
        clusterId: kind === "audit" && clusterId === "" ? undefined : clusterId,
        namespace: kind === "findings" || kind === "events" ? scope || undefined : undefined,
        node: kind === "metrics" ? scope || undefined : undefined,
        note: note || undefined,
      });
      setNote("");
      onChange();
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  };
  const remove = async (ev: JournalEvidence) => {
    if (!window.confirm(`Remove this evidence (${ev.summary})?`)) return;
    try {
      await journalApi.removeEvidence(entryId, ev.id);
      onChange();
    } catch (e) {
      setError(e);
    }
  };

  return (
    <div className="card evidence">
      <h2>Evidence</h2>
      <p className="muted small">
        A snapshot from Kubemap, kept with this record: what the problem finder, the events, the node metrics or the audit
        log showed at the time. Each links to the page it came from.
      </p>
      {evidence.length === 0 && <p className="muted">No evidence yet.</p>}
      {evidence.map((ev) => (
        <EvidenceItem key={ev.id} ev={ev} onRemove={() => remove(ev)} />
      ))}
      <div className="evidence-form">
        <label>
          Capture
          <select value={kind} onChange={(e) => setKind(e.target.value as EvidenceKind)}>
            {(Object.keys(EVIDENCE_TEXT) as EvidenceKind[]).map((k) => (
              <option key={k} value={k}>
                {EVIDENCE_TEXT[k]}
              </option>
            ))}
          </select>
        </label>
        <label>
          Cluster
          <select value={clusterId} onChange={(e) => setClusterId(e.target.value)}>
            {kind === "audit" && <option value="">Every cluster</option>}
            {clusters.map((c) => (
              <option key={c.id} value={c.id}>
                {c.name} ({c.status})
              </option>
            ))}
          </select>
        </label>
        {kind !== "audit" && (
          <label>
            {kind === "metrics" ? "Node (optional)" : "Namespace (optional)"}
            <input value={scope} onChange={(e) => setScope(e.target.value.trim())} placeholder={kind === "metrics" ? "every node" : "every namespace"} />
          </label>
        )}
        <label className="grow">
          Note (optional)
          <input value={note} onChange={(e) => setNote(e.target.value)} placeholder="why this matters" />
        </label>
        <button className="primary" onClick={capture} disabled={busy || (kind !== "audit" && !clusterId)}>
          {busy ? "Capturing…" : "Capture now"}
        </button>
      </div>
      {kind !== "audit" && clusters.length === 0 && <p className="muted small">The cluster is not available to capture from yet.</p>}
      {error != null && <div className="notice error small">{errorMessage(error)}</div>}
    </div>
  );
}

function EvidenceItem({ ev, onRemove }: { ev: JournalEvidence; onRemove: () => void }) {
  const link = safeLink(ev.link);
  return (
    <div className="evidence-item">
      <div className="evidence-head">
        <span className="badge">{ev.kind}</span>
        <span>{ev.summary}</span>
        <span className="spacer" />
        {link && (
          <a className="small" href={link}>
            Open in Kubemap
          </a>
        )}
        <button className="link small danger" onClick={onRemove}>
          Remove
        </button>
      </div>
      <div className="muted small">
        {new Date(ev.capturedAt).toLocaleString()} by {ev.capturedBy}
        {ev.note && <> · {ev.note}</>}
      </div>
      <details className="tableview">
        <summary>Show the snapshot</summary>
        <EvidenceData ev={ev} />
      </details>
    </div>
  );
}

type Row = Record<string, unknown>;
const rowsOf = (data: unknown, key: string): Row[] => {
  const v = (data as Record<string, unknown> | null)?.[key];
  return Array.isArray(v) ? (v as Row[]) : [];
};
const str = (v: unknown) => (v === undefined || v === null ? "" : String(v));
const time = (v: unknown) => (v ? new Date(String(v)).toLocaleString() : "");
const pct = (v: unknown) => (typeof v === "number" ? `${v.toFixed(1)}%` : "—");

function EvidenceData({ ev }: { ev: JournalEvidence }) {
  switch (ev.kind) {
    case "findings":
      return (
        <div className="tablescroll">
          <Table
            head={["Severity", "Object", "Finding", "Evidence"]}
            rows={rowsOf(ev.data, "findings").map((f) => [str(f.severity), str(f.object), str(f.title), str(f.evidence)])}
          />
        </div>
      );
    case "events":
      return (
        <div className="tablescroll">
          <Table
            head={["Time", "Object", "Reason", "Count", "Message"]}
            rows={rowsOf(ev.data, "events").map((e) => [
              time(e.time),
              `${str(e.namespace)} ${str(e.object)}`,
              str(e.reason),
              str(e.count),
              str(e.message),
            ])}
          />
        </div>
      );
    case "metrics":
      return (
        <>
          {rowsOf(ev.data, "nodes").map((n) => (
            <div key={str(n.node)} className="tablescroll">
              <Table
                head={[`${str(n.node)}: time`, "CPU", "Memory", "Disk", "Inodes", "PSI io"]}
                rows={[...(n.now ? [{ ...(n.now as Row), latest: true }] : []), ...rowsOf(n, "rollups").reverse()].map((r) => [
                  `${time(r.time)}${r.latest ? " (latest report)" : ""}`,
                  pct(r.cpuPct),
                  pct(r.memPct),
                  pct(r.diskPct),
                  pct(r.inodesPct),
                  pct(r.psiIo),
                ])}
              />
            </div>
          ))}
        </>
      );
    case "audit":
      return <AuditTable entries={rowsOf(ev.data, "entries") as unknown as AuditEntry[]} />;
  }
  return <pre>{JSON.stringify(ev.data, null, 2)}</pre>;
}

function AuditTable({ entries, tall }: { entries: AuditEntry[]; tall?: boolean }) {
  return (
    <div className={tall ? "tablewrap" : "tablescroll"}>
      <Table
        head={["Time", "User", "Action", "Target", "Cluster", "Result"]}
        rows={entries.map((a) => [
          time(a.time),
          a.user,
          <code key="a">{a.action}</code>,
          a.target,
          a.clusterId,
          <span key="r" className={`audit-result level-${resultLevel(a.result)}`}>
            {a.result}
          </span>,
        ])}
      />
    </div>
  );
}

// ---------------------------------------------------------------- interview

/** #/journal/<id>/interview: the story in STAR form, and the decisions (KM-91). */
export function InterviewView({ id }: { id: string }) {
  const { data, error } = useEntry(id);
  if (error) return <NotFound error={error} />;
  if (!data) return <p className="muted">Loading…</p>;
  const e = data.entry;
  const v = interview(e);
  return (
    <section className="interview">
      <p>
        <a href={journalHref(e.id)}>← Full record</a>
      </p>
      <div className="pagehead">
        <h1>{e.title}</h1>
        <a className="button" href={journalHref(e.id, "edit")}>
          Edit
        </a>
      </div>
      <p className="muted">
        {[showDate(e.date), e.cluster && `cluster ${e.cluster}`, e.severity && `severity ${e.severity}`].filter(Boolean).join(" · ")}
      </p>
      {v.missing.length > 0 && (
        <div className="notice attention">
          For a complete story, add {v.missing.join(" and ")}. <a href={journalHref(e.id, "edit")}>Edit</a>
        </div>
      )}
      <StarView star={v.star} big />
      <h2>Decisions: what I chose, why, and what it cost</h2>
      {v.decisions.length === 0 ? (
        <p className="muted">No decisions written yet.</p>
      ) : (
        <Table
          className="decisions-big"
          head={["#", "Decision", "Why", "Trade-off"]}
          rows={v.decisions.map((d, i) => [String(i + 1), md(d.decision), md(d.why), md(d.tradeOff)])}
        />
      )}
      {v.lessons.length > 0 && (
        <>
          <h2>What it shows</h2>
          {v.lessons.map((l, i) => (
            <MarkdownText key={i} text={l} />
          ))}
        </>
      )}
      {data.evidence.length > 0 && (
        <>
          <h2>Evidence to show</h2>
          <ul>
            {data.evidence.map((ev) => {
              const link = safeLink(ev.link);
              return (
                <li key={ev.id}>
                  {ev.summary}
                  {link && (
                    <>
                      {" · "}
                      <a href={link}>open</a>
                    </>
                  )}
                </li>
              );
            })}
          </ul>
        </>
      )}
    </section>
  );
}

// ---------------------------------------------------------------- editor

/** #/journal/<id>/edit and #/journal/new: the editor. */
export function JournalEdit({ id, kind }: { id?: string; kind?: JournalKind }) {
  const [rec, setRec] = useState<JournalRecord | undefined>(() => (id ? undefined : newRecord(kind ?? "incident", today())));
  const [version, setVersion] = useState(0);
  const [loadError, setLoadError] = useState<unknown>(null);
  const [error, setError] = useState<unknown>(null);
  const [saving, setSaving] = useState(false);
  const [addKind, setAddKind] = useState<SectionKind>("text");
  const dirty = useRef(false);

  useEffect(() => {
    if (!id) return;
    journalApi.get(id).then(({ entry }) => {
      const { kind, title, date, cluster, severity, sections } = entry;
      setRec({ kind, title, date, cluster, severity, sections });
      setVersion(entry.version);
    }, setLoadError);
  }, [id]);
  useEffect(() => {
    const warn = (e: BeforeUnloadEvent) => {
      if (dirty.current) e.preventDefault();
    };
    window.addEventListener("beforeunload", warn);
    return () => window.removeEventListener("beforeunload", warn);
  }, []);

  if (loadError) return <NotFound error={loadError} />;
  if (!rec) return <p className="muted">Loading…</p>;

  const set = (r: JournalRecord) => {
    dirty.current = true;
    setRec(r);
  };
  const setSection = (i: number, s: JournalSection) => set({ ...rec, sections: replaceAt(rec.sections, i, s) });
  const problem = recordProblem(rec);

  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      const saved = id ? await journalApi.update(id, version, rec) : await journalApi.create(rec);
      dirty.current = false;
      window.location.hash = journalHref(saved.id);
    } catch (e) {
      setError(e);
    } finally {
      setSaving(false);
    }
  };

  return (
    <section className="journal-edit">
      <p>
        <a href={id ? journalHref(id) : "#/journal"}>← {id ? "Back without saving" : "Journal"}</a>
      </p>
      <div className="pagehead">
        <h1>{id ? "Edit" : rec.kind === "changes" ? "New change record" : "New incident"}</h1>
      </div>
      <div className="card">
        <div className="meta-fields">
          <label className="grow">
            Title{rec.kind === "incident" && <span className="muted small"> (exported as "Incident: …")</span>}
            <input value={rec.title} onChange={(e) => set({ ...rec, title: e.target.value })} />
          </label>
          <label>
            Date
            <input type="date" value={rec.date} onChange={(e) => set({ ...rec, date: e.target.value })} />
          </label>
          <label>
            Cluster
            <input value={rec.cluster} onChange={(e) => set({ ...rec, cluster: e.target.value })} placeholder="k8server" />
          </label>
          {rec.kind === "incident" && (
            <label>
              Severity
              <select value={rec.severity} onChange={(e) => set({ ...rec, severity: e.target.value })}>
                <option value="">Not set</option>
                {SEVERITIES.map((s) => (
                  <option key={s} value={s}>
                    {s}
                  </option>
                ))}
              </select>
            </label>
          )}
        </div>
      </div>

      {rec.sections.map((s, i) => {
        // The text under the title stays first (the portal requires it).
        const pinned = (k: number) => k === 0 && rec.sections[0].heading === "";
        return (
        <div key={i} className="card section-edit">
          <div className="section-edit-head">
            {s.heading === "" && i === 0 ? (
              <span className="muted small">Under the title (no heading)</span>
            ) : (
              <input
                className="heading-input"
                value={s.heading}
                aria-label="Section heading"
                onChange={(e) => setSection(i, { ...s, heading: e.target.value })}
              />
            )}
            <span className="muted small">{SECTION_KINDS.find((k) => k.kind === s.kind)?.label ?? (s.kind === "intro" ? "Intro text" : s.kind)}</span>
            <span className="spacer" />
            <button
              className="link"
              disabled={i === 0 || pinned(i) || pinned(i - 1)}
              onClick={() => set({ ...rec, sections: move(rec.sections, i, -1) })}
              aria-label="Move up"
            >
              ↑
            </button>
            <button
              className="link"
              disabled={i === rec.sections.length - 1 || pinned(i)}
              onClick={() => set({ ...rec, sections: move(rec.sections, i, 1) })}
              aria-label="Move down"
            >
              ↓
            </button>
            <button
              className="link danger"
              onClick={() => window.confirm(`Remove "${s.heading || "the intro"}"?`) && set({ ...rec, sections: removeAt(rec.sections, i) })}
            >
              Remove
            </button>
          </div>
          <SectionEditor s={s} onChange={(v) => setSection(i, v)} />
        </div>
        );
      })}

      <div className="card actions left">
        <select value={addKind} onChange={(e) => setAddKind(e.target.value as SectionKind)} aria-label="Section kind">
          {SECTION_KINDS.map((k) => (
            <option key={k.kind} value={k.kind}>
              {k.label}
            </option>
          ))}
        </select>
        <button onClick={() => set({ ...rec, sections: [...rec.sections, emptySection(addKind)] })}>Add section</button>
        {rec.sections[0]?.kind !== "intro" && (
          <button
            onClick={() => set({ ...rec, sections: [emptySection("intro"), ...rec.sections] })}
            title="A short line under the title, such as the date, the cluster and the time zone"
          >
            Add intro text
          </button>
        )}
      </div>

      {problem && <div className="notice attention">{problem}</div>}
      {error != null && (
        <div className="notice error">
          {errorMessage(error)}
          {error instanceof ApiError && error.code === "conflict" && id && (
            <>
              {" "}
              Your text is still here: copy what you need, then <a href={journalHref(id)}>open the latest version</a>.
            </>
          )}
        </div>
      )}
      <div className="actions left">
        <button className="primary" onClick={save} disabled={saving || problem !== null}>
          {saving ? "Saving…" : "Save"}
        </button>
      </div>
    </section>
  );
}

function SectionEditor({ s, onChange }: { s: JournalSection; onChange: (s: JournalSection) => void }) {
  if (isTextKind(s.kind)) {
    return (
      <textarea
        rows={Math.min(20, Math.max(4, (s.text ?? "").split("\n").length + 1))}
        value={s.text ?? ""}
        onChange={(e) => onChange({ ...s, text: e.target.value })}
        placeholder="Markdown: paragraphs, - lists, `code`, **bold**"
      />
    );
  }
  switch (s.kind) {
    case "timeline":
      return (
        <RowsEditor<TimelineRow>
          rows={s.timeline ?? []}
          empty={{ time: "", event: "" }}
          cols={[
            { key: "time", label: "Time", short: true },
            { key: "event", label: "Event" },
          ]}
          onChange={(timeline) => onChange({ ...s, timeline })}
        />
      );
    case "decisions":
      return (
        <RowsEditor<Decision>
          rows={s.decisions ?? []}
          empty={{ decision: "", why: "", tradeOff: "" }}
          numbered
          cols={[
            { key: "decision", label: "Decision" },
            { key: "why", label: "Why" },
            { key: "tradeOff", label: "Trade-off" },
          ]}
          onChange={(decisions) => onChange({ ...s, decisions })}
        />
      );
    case "changes":
      return (
        <RowsEditor<ChangeRow>
          rows={s.changes ?? []}
          empty={{ time: "", change: "", why: "", checked: "", undo: "" }}
          cols={[
            { key: "time", label: "Time", short: true },
            { key: "change", label: "Change" },
            { key: "why", label: "Why" },
            { key: "checked", label: "How it was checked" },
            { key: "undo", label: "Undo" },
          ]}
          onChange={(changes) => onChange({ ...s, changes })}
        />
      );
    case "followups":
      return (
        <RowsEditor<FollowUp>
          rows={s.followUps ?? []}
          empty={{ status: "todo", owner: "", text: "" }}
          cols={[
            { key: "status", label: "Status", options: STATUS_TEXT },
            { key: "owner", label: "Owner", short: true },
            { key: "text", label: "Follow-up", line: true },
          ]}
          onChange={(followUps) => onChange({ ...s, followUps })}
        />
      );
    case "star": {
      const star = s.star ?? { situation: "", task: "", action: "", result: "" };
      const parts: [keyof Star, string, string][] = [
        ["situation", "Situation", "Where you were and what went wrong, in one or two sentences."],
        ["task", "Task", "What you had to achieve."],
        ["action", "Action", "What you did, in order, with the reasoning (say I, not we)."],
        ["result", "Result", "The outcome with numbers, and the follow-ups."],
      ];
      return (
        <div className="star-edit">
          {parts.map(([k, label, hint]) => (
            <label key={k}>
              {label} <span className="muted small">{hint}</span>
              <textarea rows={3} value={star[k]} onChange={(e) => onChange({ ...s, star: { ...star, [k]: e.target.value.replace(/\n/g, " ") } })} />
            </label>
          ))}
        </div>
      );
    }
  }
  return null;
}

type Col<T> = { key: keyof T & string; label: string; short?: boolean; line?: boolean; options?: Record<string, string> };

function RowsEditor<T extends { [K in keyof T]: string }>({
  rows,
  cols,
  empty,
  numbered,
  onChange,
}: {
  rows: T[];
  cols: Col<T>[];
  empty: T;
  numbered?: boolean;
  onChange: (rows: T[]) => void;
}) {
  return (
    <div className="rows-edit">
      <table className="data compact">
        <thead>
          <tr>
            {numbered && <th>#</th>}
            {cols.map((c) => (
              <th key={c.key}>{c.label}</th>
            ))}
            <th />
          </tr>
        </thead>
        <tbody>
          {rows.map((r, i) => (
            <tr key={i}>
              {numbered && <td>{i + 1}</td>}
              {cols.map((c) => (
                <td key={c.key} className={c.short ? "short" : ""}>
                  {c.options ? (
                    <select value={r[c.key]} onChange={(e) => onChange(replaceAt(rows, i, { ...r, [c.key]: e.target.value }))}>
                      {Object.entries(c.options).map(([v, label]) => (
                        <option key={v} value={v}>
                          {label}
                        </option>
                      ))}
                    </select>
                  ) : c.short || c.line ? (
                    <input
                      value={r[c.key]}
                      aria-label={c.label}
                      onChange={(e) => onChange(replaceAt(rows, i, { ...r, [c.key]: e.target.value }))}
                    />
                  ) : (
                    <textarea
                      rows={2}
                      value={r[c.key]}
                      aria-label={c.label}
                      onChange={(e) => onChange(replaceAt(rows, i, { ...r, [c.key]: e.target.value }))}
                    />
                  )}
                </td>
              ))}
              <td className="row-tools">
                <button className="link" disabled={i === 0} onClick={() => onChange(move(rows, i, -1))} aria-label="Move up">
                  ↑
                </button>
                <button className="link" disabled={i === rows.length - 1} onClick={() => onChange(move(rows, i, 1))} aria-label="Move down">
                  ↓
                </button>
                <button className="link danger" onClick={() => onChange(removeAt(rows, i))} aria-label="Remove row">
                  ✕
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <button className="link" onClick={() => onChange([...rows, { ...empty }])}>
        + Add row
      </button>
    </div>
  );
}

// ---------------------------------------------------------------- audit

/** #/audit[?cluster=<id>]: the audit log, newest first (journal evidence links here). */
const AUDIT_GROUPS: { key: AuditGroup; label: string }[] = [
  { key: "all", label: "Everything" },
  { key: "journal", label: "Journal" },
  { key: "login", label: "Logins" },
];

export function AuditPage({ cluster }: { cluster?: string }) {
  const [entries, setEntries] = useState<AuditEntry[] | undefined>();
  const [error, setError] = useState<unknown>(null);
  const [group, setGroup] = useState<AuditGroup>("all");
  const load = () => journalApi.audit(1000).then(setEntries, setError);
  useEffect(() => {
    load();
  }, []);
  const shown = entries?.filter((e) => (!cluster || e.clusterId === cluster) && (group === "all" || auditGroup(e.action) === group));
  return (
    <section className="audit-page">
      <div className="pagehead">
        <h1>Audit log</h1>
        <button onClick={load}>Refresh</button>
      </div>
      <p className="muted lead">
        Who did what in the portal: logins and
        journal edits. Newest first.
        {cluster && (
          <>
            {" "}
            Showing cluster <code>{cluster}</code> only. <a href="#/audit">Show all</a>
          </>
        )}
      </p>
      <div className="filters" role="group" aria-label="Kind of entry">
        {AUDIT_GROUPS.map((g) => (
          <button key={g.key} className={g.key === group ? "chip selected" : "chip"} aria-pressed={g.key === group} onClick={() => setGroup(g.key)}>
            {g.label}
          </button>
        ))}
      </div>
      {error != null && <div className="notice error">{errorMessage(error)}</div>}
      {shown && (shown.length === 0 ? <p className="muted">No entries.</p> : <AuditTable entries={shown} tall />)}
    </section>
  );
}
