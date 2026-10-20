-- KM-89: the incident journal. An entry is an incident or a day's change
-- record. The searchable fields are columns; the record itself (timeline,
-- decisions, STAR, follow-ups, ...) is JSON owned by portal/journal.
CREATE TABLE journal_entries (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,             -- incident, changes
    title       TEXT NOT NULL,
    date        TEXT NOT NULL DEFAULT '',  -- YYYY-MM-DD
    cluster     TEXT NOT NULL DEFAULT '',  -- a cluster name, free text
    severity    TEXT NOT NULL DEFAULT '',
    body        TEXT NOT NULL,             -- JSON
    source      TEXT NOT NULL DEFAULT '',  -- imported file name
    version     INTEGER NOT NULL,          -- +1 per edit; edits must name it
    created_at  INTEGER NOT NULL,
    created_by  TEXT NOT NULL,
    updated_at  INTEGER NOT NULL,
    updated_by  TEXT NOT NULL
);
CREATE INDEX journal_entries_date ON journal_entries(date);
CREATE UNIQUE INDEX journal_entries_source ON journal_entries(source) WHERE source != '';

-- Evidence is a snapshot taken from Kubemap (findings, events, metrics,
-- audit entries), with a link to the page it came from.
CREATE TABLE journal_evidence (
    id          TEXT PRIMARY KEY,
    entry_id    TEXT NOT NULL REFERENCES journal_entries(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL,
    cluster_id  TEXT NOT NULL DEFAULT '',
    summary     TEXT NOT NULL,
    link        TEXT NOT NULL DEFAULT '',
    note        TEXT NOT NULL DEFAULT '',
    data        TEXT NOT NULL,             -- JSON snapshot
    captured_at INTEGER NOT NULL,
    captured_by TEXT NOT NULL
);
CREATE INDEX journal_evidence_entry ON journal_evidence(entry_id, captured_at);

-- KM-90 draft incidents, kept here so they survive a restart. The drafter
-- owns the JSON; a person turns a draft into a journal entry (its source is
-- then "draft:<id>").
CREATE TABLE journal_drafts (
    id          TEXT PRIMARY KEY,
    cluster_id  TEXT NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    started     INTEGER NOT NULL,
    body        TEXT NOT NULL             -- JSON (incidents.Draft)
);
CREATE INDEX journal_drafts_cluster ON journal_drafts(cluster_id, started);
