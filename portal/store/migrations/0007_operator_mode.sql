-- KM-38: the portal's operator mode switch, per cluster. Off (no row) by
-- default. The portal sends fix actions only while it is on; the cluster
-- side needs the separate operator binding as well (ARCHITECTURE A3).
CREATE TABLE operator_mode (
    cluster_id  TEXT PRIMARY KEY REFERENCES clusters(id) ON DELETE CASCADE,
    enabled     INTEGER NOT NULL,
    changed_at  INTEGER NOT NULL,
    changed_by  TEXT NOT NULL
);
