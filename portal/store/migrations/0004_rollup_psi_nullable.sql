-- KM-30 review B1: a node whose kernel has no PSI must not be stored as 0 %
-- pressure. The PSI columns become nullable: NULL means "not reported".
-- SQLite cannot drop NOT NULL in place, so the table is rebuilt. Existing
-- rows keep their values (at most 24 h of history).
CREATE TABLE metric_rollups_new (
    cluster_id  TEXT NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    node        TEXT NOT NULL,
    bucket      INTEGER NOT NULL,         -- start of the time bucket
    cpu_pct     REAL NOT NULL,
    mem_pct     REAL NOT NULL,
    swap_pct    REAL NOT NULL,
    disk_pct    REAL NOT NULL,
    inodes_pct  REAL NOT NULL,
    psi_cpu     REAL,                     -- PSI "some" avg10; NULL: kernel has no PSI
    psi_memory  REAL,
    psi_io      REAL,
    PRIMARY KEY (cluster_id, node, bucket)
);
INSERT INTO metric_rollups_new SELECT * FROM metric_rollups;
DROP TABLE metric_rollups;
ALTER TABLE metric_rollups_new RENAME TO metric_rollups;
CREATE INDEX metric_rollups_bucket ON metric_rollups(bucket);
