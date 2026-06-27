// Package groupexport defines the format-agnostic, in-memory model for
// a per-group export (tclaude agent groups export / import) and the
// on-disk container that serializes it.
//
// Layering: this package depends on the standard library ONLY. The DB
// layer (pkg/claude/common/db) imports it to collect rows into an
// Export and to apply an Export on import; the daemon imports it to read
// and write conversation .jsonl bytes and to drive the container. Keeping
// groupexport dependency-free is deliberate — it prevents an import cycle
// (db imports groupexport; convops/agentd import db) and keeps the model
// trivially unit-testable.
//
// The model (model.go) carries no serialization logic. The container
// (container.go) is the single, swappable boundary between the model and
// the bytes on disk — phase 1 ships a zip container.
package groupexport

// FormatVersion is the current group-export manifest format version. It
// is written into every export and checked on import: an export whose
// FormatVersion the running binary does not recognise is refused rather
// than mishandled (see container.go). Bump this on any breaking change
// to the Export shape; the export format is meant to live in source
// control, so forward-incompatible changes must be detectable.
//
// v2 (JOH-220) drops the vestigial Group.default_model field: Spawn
// Profiles (agent_groups.default_profile) replaced the per-group spawn
// model, so a v2 export no longer carries default_model. A v1 archive that
// still does imports fine — the importer reads its legacy default_model and
// synthesizes a default spawn profile from it (see db.ImportGroup), so the
// older export's effective spawn default does not silently regress.
const FormatVersion = 2

// Export is the complete, format-agnostic, in-memory representation of
// one per-group export.
//
// It is produced by db.CollectGroupExport (every DB row) together with
// the daemon's export handler (each conv's .jsonl bytes), serialized to
// an on-disk artifact by the container, and consumed on import by
// db.ImportGroup.
//
// Fidelity rules:
//   - Every timestamp is the raw RFC3339(Nano) string exactly as stored
//     in SQLite, so a round-trip is byte-identical with no parse/format
//     drift.
//   - Machine-specific path columns (agent_workdir.dir / worktree_root,
//     agent_groups.default_cwd, …) are deliberately NOT carried: on
//     import every such path is set to the caller's --into target, so a
//     source machine's absolute paths never enter an importable DB
//     field. The only source paths recorded are SourceHome and the
//     per-conv SourceCwd in Conv — kept solely so the importer can
//     rewrite paths embedded inside the .jsonl content.
type Export struct {
	// FormatVersion is FormatVersion at export time.
	FormatVersion int `json:"format_version"`
	// ExportedAt is when the export was taken (RFC3339Nano).
	ExportedAt string `json:"exported_at"`
	// SchemaVersion is the SQLite schema version at export time —
	// informational, lets a future importer reason about row shapes.
	SchemaVersion int `json:"schema_version"`
	// SourceGroup is the group's name in the source DB.
	SourceGroup string `json:"source_group"`
	// SourceHome is the exporting user's home directory — one of the two
	// path bases the importer rewrites out of the .jsonl content.
	SourceHome string `json:"source_home"`
	// SourceOS is runtime.GOOS at export time (linux/darwin/windows).
	SourceOS string `json:"source_os"`

	Group       Group        `json:"group"`
	Members     []Member     `json:"members"`
	Owners      []Owner      `json:"owners"`
	Audit       []AuditEntry `json:"audit"`
	Permissions []Permission `json:"permissions"`
	Enrollments []Enrollment `json:"enrollments"`
	Workdirs    []Workdir    `json:"workdirs"`
	SudoGrants  []SudoGrant  `json:"sudo_grants"`
	HeadAliases []HeadAlias  `json:"head_aliases"`
	Successions []Succession `json:"successions"`
	SpawnHist   []SpawnHist  `json:"spawn_history"`
	CloneHist   []CloneHist  `json:"clone_history"`
	CronJobs    []CronJob    `json:"cron_jobs"`
	CronRuns    []CronRun    `json:"cron_runs"`
	Messages    []Message    `json:"messages"`
	Convs       []Conv       `json:"convs"`
}

// Group is the agent_groups row, minus the autoincrement id (regenerated
// on import) and default_cwd (a source path — reset to the import
// target).
type Group struct {
	Descr          string `json:"descr"`
	DefaultContext string `json:"default_context"`
	// DefaultModel is LEGACY / import-only (JOH-220). The vestigial
	// per-group default_model column was dropped, so a current (v2) export
	// never writes this field — CollectGroupExport leaves it "" and
	// omitempty keeps it out of the manifest. It survives on the struct
	// solely to decode a pre-v2 (v1) archive that still carries it: on
	// import a non-empty value is turned into a synthesized default spawn
	// profile (see db.ImportGroup) so the older export's spawn default does
	// not regress. Decodes to "" (unset) for any v2 archive.
	DefaultModel string `json:"default_model,omitempty"`
	MaxMembers   int    `json:"max_members"`
	CreatedAt    string `json:"created_at"`
	ArchivedAt   string `json:"archived_at"`
}

// Member is an agent_group_members row (the group_id is implicit).
type Member struct {
	ConvID   string `json:"conv_id"`
	Role     string `json:"role"`
	Descr    string `json:"descr"`
	JoinedAt string `json:"joined_at"`
}

// Owner is an agent_group_owners row.
type Owner struct {
	ConvID    string `json:"conv_id"`
	GrantedAt string `json:"granted_at"`
	GrantedBy string `json:"granted_by"`
}

// AuditEntry is an agent_group_audit row — one rename in the group's
// history.
type AuditEntry struct {
	OldName string `json:"old_name"`
	NewName string `json:"new_name"`
	ByConv  string `json:"by_conv"`
	At      string `json:"at"`
}

// Permission is an agent_permissions row — a per-conv permission
// override. Effect is "grant" or "deny".
type Permission struct {
	ConvID    string `json:"conv_id"`
	Slug      string `json:"slug"`
	Effect    string `json:"effect"`
	GrantedAt string `json:"granted_at"`
	GrantedBy string `json:"granted_by"`
}

// Enrollment is an agent_enrollment row — the "this conv is an agent"
// record, including the retired-state fields.
type Enrollment struct {
	ConvID       string `json:"conv_id"`
	EnrolledAt   string `json:"enrolled_at"`
	EnrolledVia  string `json:"enrolled_via"`
	RetiredAt    string `json:"retired_at"`
	RetiredBy    string `json:"retired_by"`
	RetireReason string `json:"retire_reason"`
	PendingName  string `json:"pending_name"`
}

// Workdir is an agent_workdir row, minus the machine-specific dir /
// worktree_root / branch (the dir is reset to the import target). The
// row's existence — "this agent has a workdir record" — is what carries
// over.
type Workdir struct {
	ConvID    string `json:"conv_id"`
	UpdatedAt string `json:"updated_at"`
}

// SudoGrant is an agent_sudo_grants row — a time-boxed elevated grant.
type SudoGrant struct {
	ConvID    string `json:"conv_id"`
	Slug      string `json:"slug"`
	GrantedAt string `json:"granted_at"`
	ExpiresAt string `json:"expires_at"`
	GrantedBy string `json:"granted_by"`
	Reason    string `json:"reason"`
	RevokedAt string `json:"revoked_at"`
}

// HeadAlias is an agent_head_aliases row — a stable handle pointing at a
// conv-id.
type HeadAlias struct {
	Handle       string `json:"handle"`
	AnchorConvID string `json:"anchor_conv_id"`
	CreatedAt    string `json:"created_at"`
	ByConv       string `json:"by_conv"`
}

// Succession is an agent_conv_succession row — an old→new conv-id
// mapping recorded when an agent reincarnated.
type Succession struct {
	OldConvID   string `json:"old_conv_id"`
	NewConvID   string `json:"new_conv_id"`
	Reason      string `json:"reason"`
	SucceededAt string `json:"succeeded_at"`
}

// SpawnHist is an agent_spawn_history row — rate-limit telemetry keyed
// by the spawning conv.
type SpawnHist struct {
	SpawnerConvID string `json:"spawner_conv_id"`
	SpawnedAt     string `json:"spawned_at"`
}

// CloneHist is an agent_clone_history row — rate-limit telemetry keyed
// by the cloned-from conv.
type CloneHist struct {
	SourceConvID string `json:"source_conv_id"`
	ClonedAt     string `json:"cloned_at"`
}

// CronJob is an agent_cron_jobs row (the group_id is implicit). ID is
// the source autoincrement id — carried only so CronRun rows can be
// re-linked after import assigns fresh ids.
type CronJob struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// TargetKind discriminates a conv-target job from a group fan-out job
	// (db.CronTargetConv / db.CronTargetGroup). Carried so a group-target job
	// round-trips as one; empty in older archives, where the importer defaults
	// it to "conv" (the v41 column default).
	TargetKind      string `json:"target_kind,omitempty"`
	OwnerConv       string `json:"owner_conv"`
	TargetConv      string `json:"target_conv"`
	IntervalSeconds int64  `json:"interval_seconds"`
	Subject         string `json:"subject"`
	Body            string `json:"body"`
	Enabled         int64  `json:"enabled"`
	CreatedAt       string `json:"created_at"`
	LastRunAt       string `json:"last_run_at"`
	LastRunStatus   string `json:"last_run_status"`
}

// CronRun is an agent_cron_runs row. JobID is the SOURCE cron-job id;
// the importer rewrites it to the freshly assigned job id.
type CronRun struct {
	JobID    int64  `json:"job_id"`
	FiredAt  string `json:"fired_at"`
	Status   string `json:"status"`
	ErrorMsg string `json:"error_msg"`
}

// Message is an agent_messages row (the group_id is implicit). ID and
// ParentID are the SOURCE autoincrement ids — carried so the importer
// can rebuild the parent/child threading after assigning fresh ids.
type Message struct {
	ID             int64  `json:"id"`
	FromConv       string `json:"from_conv"`
	ToConv         string `json:"to_conv"`
	Subject        string `json:"subject"`
	Body           string `json:"body"`
	CreatedAt      string `json:"created_at"`
	DeliveredAt    string `json:"delivered_at"`
	ReadAt         string `json:"read_at"`
	ParentID       int64  `json:"parent_id"`
	ToRecipients   string `json:"to_recipients"`
	CcRecipients   string `json:"cc_recipients"`
	OriginalToConv string `json:"original_to_conv"`
}

// Conv is one agent's conversation .jsonl.
//
// Content holds the RAW file bytes. It carries the json:"-" tag because
// the zip container stores each conv as a real file entry under
// projects/ — never inline in the manifest. SourceCwd is the conv's
// working directory on the source machine, recorded so the importer can
// rewrite that prefix out of the .jsonl content. Missing is set when the
// .jsonl could not be located at export time; such a conv still imports
// its DB rows, just without conversation history.
type Conv struct {
	ConvID    string `json:"conv_id"`
	SourceCwd string `json:"source_cwd"`
	Title     string `json:"title"`
	Missing   bool   `json:"missing,omitempty"`
	Content   []byte `json:"-"`
}
