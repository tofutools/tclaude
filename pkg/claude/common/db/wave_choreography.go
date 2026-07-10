package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// WaveChoreography is the persisted, self-healing runtime state of one group's
// in-flight staged deploy (JOH-244). A template whose agents span more than one
// `wave` spawns its first wave synchronously at deploy, then defers the rest:
// this row carries everything the daemon's background wave runner needs to
// spawn the remaining waves — gated on each prior wave settling — WITHOUT the
// deploy HTTP call blocking on those beats. Because the whole choreography is
// durable, a daemon restart mid-deploy re-arms the pending waves from it (the
// repo's self-healing-over-migrations principle, applied to runtime state).
//
// One row per group (group_id PK). No row = no pending choreography (a
// single-wave deploy never creates one; the last wave landing deletes it).
// DeleteAgentGroup drops it in its cleanup transaction.
//
// The struct is stored as a single JSON `state` blob (marshalled sans the
// GroupID/GroupName/UpdatedAt columns, which are stored + read separately), so
// the shape can evolve without a schema change.
type WaveChoreography struct {
	GroupID   int64  `json:"-"`
	GroupName string `json:"-"`

	// --- static plan (set once at deploy) ---

	// TemplateName is the source template, for logging + surfacing.
	TemplateName string `json:"template_name"`
	// GroupContext is the already-composed, already-normalized group context
	// (mission/task folded in) every spawned agent's briefing carries.
	GroupContext string `json:"group_context"`
	// Cwd is the group's default working directory for every spawn.
	Cwd string `json:"cwd,omitempty"`
	// WorktreePath/WorktreeBranch optionally point spawned agents at a shared
	// code worktree while they launch from Cwd. Used by the dashboard's
	// template-deploy picker for the same sub-repo worktree flow as single spawn.
	WorktreePath   string `json:"worktree_path,omitempty"`
	WorktreeBranch string `json:"worktree_branch,omitempty"`
	// PerAgentWorktrees, when set, creates a fresh worktree for each later-wave
	// agent before spawning it. Stored in the JSON state so delayed waves match
	// the synchronous first wave without a schema migration.
	PerAgentWorktrees *WavePerAgentWorktrees `json:"per_agent_worktrees,omitempty"`
	// ProofToken/ProofDirs carry the spawn-dir write proof for delayed waves.
	// They live in the JSON state so no schema migration is needed.
	ProofToken string   `json:"proof_token,omitempty"`
	ProofDirs  []string `json:"proof_dirs,omitempty"`
	// Caller is the conv that deployed (reply-to / spawned-by attribution for
	// later-wave spawns + the work-pattern deliveries). May be "" (human).
	Caller string `json:"caller,omitempty"`
	// Granter is the display label of the deploying identity (owner/permission
	// grants for later-wave agents).
	Granter string `json:"granter,omitempty"`
	// Assignment is the normalized per-run task/mission text, interpolated into
	// the work pattern's {{task}}/{{mission}} at final-wave delivery.
	Assignment string `json:"assignment,omitempty"`
	// Process is the template's process spec, rendered into each later-wave
	// agent's "## Process" block (a no-op when empty).
	Process []ProcessPhase `json:"process,omitempty"`
	// WorkPattern is the template's work pattern, delivered once the FINAL wave
	// is up (the roster is only complete then).
	WorkPattern []WorkPatternEntry `json:"work_pattern,omitempty"`
	// Waves is the full ordered wave plan (including wave 0, already spawned).
	// NextWave indexes the next entry to spawn.
	Waves []WaveGroup `json:"waves"`
	// MaxWaitSeconds caps how long each wave's idle-gate waits before the next
	// wave spawns anyway.
	MaxWaitSeconds int `json:"max_wait_seconds"`
	// SuppressOwner drops the per-agent template owner flag for every wave of
	// this deploy — set when the roster is deployed INTO an existing group
	// (reinforce mode), which never transfers ownership. Carried on the
	// choreography so LATER waves honour it too, not just the synchronous first
	// wave. Stored in the JSON `state` blob, so no schema change (omitempty keeps
	// pre-existing create-new choreographies byte-identical on the wire).
	SuppressOwner bool `json:"suppress_owner,omitempty"`

	// --- progress cursor (advanced as waves land) ---

	// NextWave is the index into Waves of the NEXT wave to spawn.
	NextWave int `json:"next_wave"`
	// GatingConvs are the conv-ids of the most-recently-spawned wave — the gate
	// the runner waits on before spawning Waves[NextWave].
	GatingConvs []string `json:"gating_convs"`
	// Activated is the subset of GatingConvs the runner has observed actively
	// WORKING at least once. A freshly-spawned agent is idle (it hasn't started
	// its turn), so the gate releases a conv only once it has been seen working
	// AND then gone idle — "has had its beat". Persisted so the observation
	// survives a restart.
	Activated []string `json:"activated"`
	// SpawnedConvs maps every spawned agent's template-name → conv, accumulated
	// across waves, for the final work-pattern routing.
	SpawnedConvs map[string]string `json:"spawned_convs"`
	// SpawnedOrder is the conv spawn order across all waves (the "all"
	// work-pattern target + broadcast audience).
	SpawnedOrder []string `json:"spawned_order"`
	// WaveDeadline is the max-wait deadline for the CURRENT gate (Waves at
	// NextWave-1). Past it, the runner spawns the next wave regardless.
	WaveDeadline time.Time `json:"wave_deadline"`

	UpdatedAt time.Time `json:"-"`
}

// WaveGroup is one wave of the plan: a wave number and the template agents that
// spawn together in it, in ordinal order.
type WaveGroup struct {
	Wave   int                  `json:"wave"`
	Agents []GroupTemplateAgent `json:"agents"`
}

// WavePerAgentWorktrees is the persisted per-agent worktree creation policy for
// staged template deploys.
type WavePerAgentWorktrees struct {
	Repo          string `json:"repo"`
	FromBranch    string `json:"from_branch,omitempty"`
	BranchPrefix  string `json:"branch_prefix,omitempty"`
	WorktreeAsCwd bool   `json:"worktree_as_cwd,omitempty"`
}

// PendingWaves is the count of waves not yet spawned (NextWave..end).
func (c *WaveChoreography) PendingWaves() int {
	n := len(c.Waves) - c.NextWave
	if n < 0 {
		return 0
	}
	return n
}

// UpsertWaveChoreography writes (creates or replaces) a group's choreography
// row. The whole struct is marshalled into the `state` blob; group_id /
// group_name / updated_at are the queryable columns.
func UpsertWaveChoreography(c *WaveChoreography) error {
	d, err := Open()
	if err != nil {
		return err
	}
	blob, err := json.Marshal(c)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = d.Exec(`
		INSERT INTO group_wave_choreography (group_id, group_name, state, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(group_id) DO UPDATE SET
			group_name = excluded.group_name,
			state      = excluded.state,
			updated_at = excluded.updated_at`,
		c.GroupID, c.GroupName, string(blob), now)
	return err
}

// GetWaveChoreography returns a group's choreography row, or (nil, nil) when
// the group has none.
func GetWaveChoreography(groupID int64) (*WaveChoreography, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return scanWaveChoreography(d.QueryRow(
		`SELECT group_id, group_name, state, updated_at FROM group_wave_choreography WHERE group_id = ?`, groupID))
}

// ListWaveChoreographies returns every pending choreography row — the wave
// runner's per-tick worklist. Ordered by group_id for determinism.
func ListWaveChoreographies() ([]*WaveChoreography, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(
		`SELECT group_id, group_name, state, updated_at FROM group_wave_choreography ORDER BY group_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*WaveChoreography{}
	for rows.Next() {
		c, err := scanWaveChoreography(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteWaveChoreography removes a group's choreography row (the last wave
// landed, or the deploy was cancelled). Idempotent.
func DeleteWaveChoreography(groupID int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM group_wave_choreography WHERE group_id = ?`, groupID)
	return err
}

func scanWaveChoreography(s rowScanner) (*WaveChoreography, error) {
	var groupID int64
	var groupName, state, updatedAt string
	if err := s.Scan(&groupID, &groupName, &state, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var c WaveChoreography
	if err := json.Unmarshal([]byte(state), &c); err != nil {
		return nil, err
	}
	c.GroupID = groupID
	c.GroupName = groupName
	c.UpdatedAt = parseTimeOrZero(updatedAt)
	// Non-nil slices/maps so callers can range/index safely.
	if c.GatingConvs == nil {
		c.GatingConvs = []string{}
	}
	if c.Activated == nil {
		c.Activated = []string{}
	}
	if c.SpawnedConvs == nil {
		c.SpawnedConvs = map[string]string{}
	}
	if c.SpawnedOrder == nil {
		c.SpawnedOrder = []string{}
	}
	return &c, nil
}
