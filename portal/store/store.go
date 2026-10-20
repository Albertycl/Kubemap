// Package store is the portal's database layer: SQLite through the pure-Go
// driver modernc.org/sqlite, so the portal builds with CGO_ENABLED=0.
//
// Session tokens are returned to the caller once and stored only as SHA-256
// hashes. The package does no logging.
package store

import (
	"context"
	"errors"
	"time"
)

// Cluster status values.
const (
	StatusActive       = "active"
	StatusDisconnected = "disconnected"
)

const (
	// RollupRetention is how long metric rollups are kept.
	RollupRetention = 24 * time.Hour
)

var (
	ErrNotFound = errors.New("store: not found")
	ErrExists   = errors.New("store: already exists")
	// ErrInvalidState is returned for a status change that is not allowed,
	// such as accepting a cluster that is not pending.
	ErrInvalidState = errors.New("store: invalid state")
	// ErrConflict is returned when a record changed since the caller read
	// it (a journal edit naming an old version).
	ErrConflict = errors.New("store: changed by someone else")
)

type Cluster struct {
	ID           string
	Name         string
	Status       string
	CreatedAt    time.Time
	AcceptedAt   time.Time // zero until accepted
	LastSeen     time.Time // zero until the agent first connects
	AgentVersion string

	// Reported by the cluster agent when it registers (KM-18). RegisteredAt
	// is zero until an agent has used an import token for this cluster.
	RegisteredAt      time.Time
	SourceIP          string
	KubernetesVersion string
	NodeCount         int
	ClusterUID        string
}

// ClusterInfo is what a registering cluster agent reports.
type ClusterInfo struct {
	AgentVersion      string
	SourceIP          string
	KubernetesVersion string
	NodeCount         int
	ClusterUID        string
}

type Node struct {
	ClusterID string
	Name      string
	Status    string
	LastSeen  time.Time
	// AgentVersion is the node agent's version from its register message.
	// Empty in an upsert keeps the stored one.
	AgentVersion string
}

type AuditEntry struct {
	Time      time.Time // zero means now
	User      string
	ClusterID string
	Action    string
	Target    string
	Result    string
}

// User is a portal login account. PasswordHash is a bcrypt hash; the store
// never sees a plaintext password.
type User struct {
	Username           string
	PasswordHash       string
	MustChangePassword bool
	CreatedAt          time.Time
}

// Session is a logged-in browser session.
type Session struct {
	Username  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Rollup is one node's metrics for one time bucket. Values are percentages
// (0-100) except PSI, which is the "some" avg10 value. The PSI values mean
// something only when HasPSI is set; they are stored as NULL otherwise.
type Rollup struct {
	ClusterID string
	Node      string
	Bucket    time.Time
	CPUPct    float64
	MemPct    float64
	SwapPct   float64
	DiskPct   float64
	InodesPct float64
	PSICPU    float64
	PSIMemory float64
	PSIIO     float64
	HasPSI    bool
}

// JournalEntry is one incident journal record (KM-89). Body is the record
// as JSON; portal/journal owns its schema.
type JournalEntry struct {
	ID       string
	Kind     string // incident, changes
	Title    string
	Date     string // YYYY-MM-DD, or empty
	Cluster  string
	Severity string
	Body     []byte
	// Source is the imported file name; one entry per source.
	Source string
	// Version grows by one per edit. An update must name the version it
	// read, or it fails with ErrConflict.
	Version   int
	CreatedAt time.Time
	CreatedBy string
	UpdatedAt time.Time
	UpdatedBy string
}

// JournalEvidence is a snapshot from Kubemap attached to a journal entry.
type JournalEvidence struct {
	ID         string
	EntryID    string
	Kind       string // findings, events, metrics, audit
	ClusterID  string
	Summary    string
	Link       string // the portal page it came from (a #/ route)
	Note       string
	Data       []byte // JSON
	CapturedAt time.Time
	CapturedBy string
}

// JournalDraft is a draft incident the portal made from the problem finder
// (KM-90). Body is the draft as JSON; portal/incidents owns its schema.
type JournalDraft struct {
	ID        string
	ClusterID string
	Started   time.Time
	Body      []byte
}

// Store is what the portal needs from its database.
type Store interface {
	// EnsureCluster returns the cluster with this ID and creates it when it
	// is missing. The server's own cluster is created this way at start.
	EnsureCluster(ctx context.Context, id, name string) (Cluster, error)
	GetCluster(ctx context.Context, id string) (Cluster, error)
	ListClusters(ctx context.Context) ([]Cluster, error)
	// UpdateClusterInfo stores what the cluster agent reported.
	UpdateClusterInfo(ctx context.Context, id string, info ClusterInfo) error
	// MarkClusterSeen records a heartbeat. A disconnected cluster becomes
	// active again; a pending cluster stays pending. An empty agentVersion
	// keeps the stored one.
	MarkClusterSeen(ctx context.Context, id, agentVersion string) error
	// MarkClusterDisconnected moves an active cluster to disconnected.
	MarkClusterDisconnected(ctx context.Context, id string) error
	// ResetConnections marks every active cluster disconnected and every
	// node "disconnected". The portal calls it at start, when no agent is
	// connected yet.
	ResetConnections(ctx context.Context) error

	UpsertNode(ctx context.Context, n Node) error
	// SetNodeStatus changes a node's status and keeps its last_seen.
	SetNodeStatus(ctx context.Context, clusterID, name, status string) error
	WriteAudit(ctx context.Context, e AuditEntry) error
	// ListAudit returns the newest audit entries first, at most limit.
	ListAudit(ctx context.Context, limit int) ([]AuditEntry, error)

	// WriteRollups inserts or replaces rollups in one transaction.
	WriteRollups(ctx context.Context, rs []Rollup) error
	// ListRollups returns one node's rollups with bucket >= since, oldest first.
	ListRollups(ctx context.Context, clusterID, node string, since time.Time) ([]Rollup, error)
	// ListNodes returns the nodes of a cluster, by name.
	ListNodes(ctx context.Context, clusterID string) ([]Node, error)
	// PruneRollups deletes rollups older than RollupRetention.
	PruneRollups(ctx context.Context) (deleted int64, err error)

	CountUsers(ctx context.Context) (int, error)
	// CreateUser fails with ErrExists if the username is taken.
	CreateUser(ctx context.Context, u User) error
	GetUser(ctx context.Context, username string) (User, error)
	// SetPassword replaces the password hash and ends all of the user's
	// sessions in the same transaction.
	SetPassword(ctx context.Context, username, passwordHash string, mustChange bool) error

	// TOTP two-factor state (see users.go). Secrets are encrypted by the
	// caller; recovery codes are SHA-256 hashes.
	GetTOTP(ctx context.Context, username string) (TOTP, error)
	SetPendingTOTP(ctx context.Context, username string, secretEnc []byte) error
	EnableTOTP(ctx context.Context, username string, step int64, codeHashes []string) error
	AdvanceTOTPStep(ctx context.Context, username string, step int64) (bool, error)
	UseRecoveryCode(ctx context.Context, username, codeHash string) (bool, error)
	CountRecoveryCodes(ctx context.Context, username string) (int, error)
	DeleteTOTP(ctx context.Context, username string) (bool, error)
	// CountEnabledTOTP is the number of users with two-factor login on.
	CountEnabledTOTP(ctx context.Context) (int, error)

	// CreateSession returns a new session token. Only its hash is stored.
	CreateSession(ctx context.Context, username string, expiresAt time.Time) (token string, err error)
	// GetSession returns ErrNotFound for an unknown or expired token.
	GetSession(ctx context.Context, token string) (Session, error)
	// ExtendSession moves the expiry of a live session.
	ExtendSession(ctx context.Context, token string, expiresAt time.Time) error
	DeleteSession(ctx context.Context, token string) error
	// DeleteOtherSessions ends all of the user's sessions except keepToken's.
	DeleteOtherSessions(ctx context.Context, username, keepToken string) error
	// PruneSessions deletes expired sessions.
	PruneSessions(ctx context.Context) (deleted int64, err error)
	// PruneAudit deletes audit entries older than keep; keep <= 0 keeps all.
	PruneAudit(ctx context.Context, keep time.Duration) (deleted int64, err error)
	// PruneDrafts deletes drafts older than DraftRetention, except each
	// cluster's newest.
	PruneDrafts(ctx context.Context) (deleted int64, err error)
	// CountJournalEvidence counts an entry's snapshots.
	CountJournalEvidence(ctx context.Context, entryID string) (int, error)

	// CreateJournalEntry stores a new entry (version 1) and returns it with
	// its ID. A second entry with the same non-empty Source gives ErrExists.
	CreateJournalEntry(ctx context.Context, e JournalEntry) (JournalEntry, error)
	GetJournalEntry(ctx context.Context, id string) (JournalEntry, error)
	FindJournalEntryBySource(ctx context.Context, source string) (JournalEntry, error)
	// ListJournalEntries returns every entry, newest date first.
	ListJournalEntries(ctx context.Context) ([]JournalEntry, error)
	// UpdateJournalEntry replaces the fields of entry e.ID if its stored
	// version is still e.Version (else ErrConflict), and returns the result.
	// Source, CreatedAt and CreatedBy do not change.
	UpdateJournalEntry(ctx context.Context, e JournalEntry) (JournalEntry, error)
	// DeleteJournalEntry deletes the entry and its evidence.
	DeleteJournalEntry(ctx context.Context, id string) error
	// AddJournalEvidence attaches evidence; ErrNotFound for an unknown entry.
	AddJournalEvidence(ctx context.Context, ev JournalEvidence) (JournalEvidence, error)
	// ListJournalEvidence returns an entry's evidence, oldest first.
	ListJournalEvidence(ctx context.Context, entryID string) ([]JournalEvidence, error)
	DeleteJournalEvidence(ctx context.Context, entryID, id string) error
	// SaveJournalDraft creates or replaces a draft by ID; ErrNotFound for an
	// unknown cluster. Drafts go with their cluster.
	SaveJournalDraft(ctx context.Context, d JournalDraft) error
	GetJournalDraft(ctx context.Context, id string) (JournalDraft, error)
	// LatestJournalDraft returns a cluster's newest draft, or ErrNotFound.
	LatestJournalDraft(ctx context.Context, clusterID string) (JournalDraft, error)
	// ListJournalDrafts returns every draft, newest first.
	ListJournalDrafts(ctx context.Context) ([]JournalDraft, error)

	Close() error
}
