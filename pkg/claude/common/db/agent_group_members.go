package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// AddAgentGroupMember inserts (or replaces) a member in a group.
func AddAgentGroupMember(m *AgentGroupMember) error {
	db, err := Open()
	if err != nil {
		return err
	}
	if m.JoinedAt.IsZero() {
		m.JoinedAt = time.Now()
	}
	// Joining a group makes the conv an agent. EnsureAgentForConv mints / links
	// the stable actor; the membership is then keyed on agent_id (JOH-26) so it
	// survives conv rotations without a rekey. Insert-only — a stray add never
	// un-retires; the dashboard add-member flow reinstates retired targets
	// explicitly.
	if _, _, err := EnsureAgentForConv(m.ConvID, "group"); err != nil {
		return err
	}
	agentID, err := AgentIDForConv(m.ConvID)
	if err != nil {
		return err
	}
	if agentID == "" {
		return fmt.Errorf("AddAgentGroupMember: no actor for conv %s", m.ConvID)
	}
	res, err := db.Exec(`INSERT OR REPLACE INTO agent_group_members
		(group_id, agent_id, role, descr, joined_at)
		SELECT ?, ?, ?, ?, ? FROM agents WHERE agent_id = ? AND retired_at IS NULL`,
		m.GroupID, agentID, m.Role, m.Descr,
		dbTime(m.JoinedAt), agentID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("AddAgentGroupMember: agent %s is retired", agentID)
	}
	if _, err := db.Exec(`UPDATE agent_groups SET route_generation = route_generation + 1 WHERE id = ?`, m.GroupID); err != nil {
		return err
	}
	return syncGroupSourcedSettings(db, agentID, 0)
}

// UpdateAgentGroupMember patches non-nil fields on an existing member.
// Pass a nil pointer for any field you don't want to change. Returns
// (rowsAffected, error); 0 rows means no such (group, conv) pair.
func UpdateAgentGroupMember(groupID int64, convID string, role, descr *string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	sets := []string{}
	args := []any{}
	if role != nil {
		sets = append(sets, "role = ?")
		args = append(args, *role)
	}
	if descr != nil {
		sets = append(sets, "descr = ?")
		args = append(args, *descr)
	}
	if len(sets) == 0 {
		return 0, nil
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return 0, err
	}
	if agentID == "" {
		return 0, nil
	}
	args = append(args, groupID, agentID)
	q := "UPDATE agent_group_members SET " + sets[0]
	for i := 1; i < len(sets); i++ {
		q += ", " + sets[i]
	}
	q += " WHERE group_id = ? AND agent_id = ?"
	res, err := db.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RemoveAgentGroupMember removes a (group, conv) pair. Idempotent.
func RemoveAgentGroupMember(groupID int64, convID string) error {
	db, err := Open()
	if err != nil {
		return err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return err
	}
	if agentID == "" {
		return nil
	}
	res, err := db.Exec(`DELETE FROM agent_group_members WHERE group_id = ? AND agent_id = ?`,
		groupID, agentID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		if _, err := db.Exec(`UPDATE agent_groups SET route_generation = route_generation + 1 WHERE id = ?`, groupID); err != nil {
			return err
		}
		return syncGroupSourcedSettings(db, agentID, 0)
	}
	return nil
}

// AgentDeletionCounts records what `DeleteAgentByConvID` removed,
// per affected table. Surfaced in the daemon response so the human
// (or the audit log) can confirm scope.
type AgentDeletionCounts struct {
	GroupMembers   int64 `json:"group_members"`
	GroupOwners    int64 `json:"group_owners"`
	MessagesFrom   int64 `json:"messages_from"`
	MessagesTo     int64 `json:"messages_to"`
	Permissions    int64 `json:"permissions"`
	CronJobsOwned  int64 `json:"cron_jobs_owned"`
	CronJobsTarget int64 `json:"cron_jobs_target"`
	SuccessionOld  int64 `json:"succession_old"`
	SuccessionNew  int64 `json:"succession_new"`
	Embeddings     int64 `json:"embeddings"`
	ConvIndex      int64 `json:"conv_index"`
	ResumeProfiles int64 `json:"resume_profiles"`
	Sessions       int64 `json:"sessions"`
	NotifyPrefs    int64 `json:"notify_prefs"`
	SudoGrants     int64 `json:"sudo_grants"`
	SpawnHistory   int64 `json:"spawn_history"`
	CloneHistory   int64 `json:"clone_history"`
}

// Add accumulates o into c field-by-field. Used by the actor-level
// cross-generation delete (conv.DeleteAgentAllGenerations, JOH-26 PR3d) to sum
// each swept generation's per-table removals into one reported total.
func (c *AgentDeletionCounts) Add(o AgentDeletionCounts) {
	c.GroupMembers += o.GroupMembers
	c.GroupOwners += o.GroupOwners
	c.MessagesFrom += o.MessagesFrom
	c.MessagesTo += o.MessagesTo
	c.Permissions += o.Permissions
	c.CronJobsOwned += o.CronJobsOwned
	c.CronJobsTarget += o.CronJobsTarget
	c.SuccessionOld += o.SuccessionOld
	c.SuccessionNew += o.SuccessionNew
	c.Embeddings += o.Embeddings
	c.ConvIndex += o.ConvIndex
	c.ResumeProfiles += o.ResumeProfiles
	c.Sessions += o.Sessions
	c.NotifyPrefs += o.NotifyPrefs
	c.SudoGrants += o.SudoGrants
	c.SpawnHistory += o.SpawnHistory
	c.CloneHistory += o.CloneHistory
}

// DeleteAgentByConvID purges the conversation generation convID, plus —
// when convID is its actor's live generation — the actor's identity. Single
// transaction; partial failure rolls everything back.
//
// Conv-scoped (always, keyed on convID — these belong to THIS generation):
//
//   - agent_messages (from_conv = ? OR to_conv = ?)
//   - agent_conv_succession (old_conv_id = ? OR new_conv_id = ?)
//   - conv_embeddings
//   - conv_index
//   - conversation_resume_profiles
//   - sessions
//   - native Codex permission profiles are marked cleanup_pending (their
//     generated files are published by the registry reconciler after commit)
//
// Actor-scoped (keyed on the resolved agent_id) — ONLY when convID is the
// actor's current_conv_id (its live generation):
//
//   - agent_group_members, agent_group_owners, agent_permissions,
//     agent_notify_prefs, agent_sudo_grants
//   - agent_cron_jobs (owner_agent = ? OR target_agent = ?), agent-keyed since
//     JOH-26 PR3a. Each delete cascades to agent_cron_runs via the FK.
//   - agent_spawn_history (spawner_agent_id), agent_clone_history
//     (source_agent_id) — agent-keyed rate-limit history with no FK to agents,
//     so deleted explicitly here rather than via cascade.
//   - agents (cascades the remaining agent_conversations links)
//
// Deleting a PREDECESSOR generation (a reincarnate / Claude Code /clear keeps
// the old conv around) instead only unlinks that one conv from its actor — the
// live actor and its identity survive (JOH-26). The succession chain is healed
// (a middle-generation delete re-links pred→succ) and any head_alias anchored on
// the deleted predecessor is rebased onto its successor (JOH-330), so stale-id
// forwarding and alias resolution both still reach the live head. When convID is
// the live generation and the actor has older generations, those predecessors'
// own conv-scoped rows + .jsonl are left behind (cleaning them up across an
// actor's whole generation set is a later stage).
//
// Filesystem state (the .jsonl in ~/.claude/projects/... and the
// ~/.claude/session-env/<convID> file) is the caller's
// responsibility — DeleteAgentByConvID only touches SQLite. The
// daemon handler combines this with file-system cleanup.
//
// Idempotent: calling on a conv-id that doesn't exist returns
// AgentDeletionCounts{} with no error.
func DeleteAgentByConvID(convID string) (AgentDeletionCounts, error) {
	return deleteAgentByConvID(convID, false)
}

// DeleteUnlaunchedAgentByConvID rolls back a newly created, provably
// undispatched birth. A first stable arrival has no permanent tombstone.
func DeleteUnlaunchedAgentByConvID(convID string) (AgentDeletionCounts, error) {
	return deleteAgentByConvID(convID, true)
}
func deleteAgentByConvID(convID string, unlaunched bool) (AgentDeletionCounts, error) {
	var c AgentDeletionCounts
	if convID == "" {
		return c, fmt.Errorf("convID is required")
	}
	d, err := Open()
	if err != nil {
		return c, err
	}
	tx, err := d.Begin()
	if err != nil {
		return c, err
	}
	defer func() { _ = tx.Rollback() }()

	agentID, err := agentIDForConvTx(tx, convID)
	if err != nil {
		return c, err
	}
	// Couple permanent conversation deletion to durable native-registry intent
	// in the same transaction. A crash after commit but before file publication
	// therefore retries at daemon startup instead of leaving an ownerless live-
	// claiming runtime definition indefinitely.
	if _, err := tx.Exec(`UPDATE codex_native_permission_profiles SET cleanup_pending = 1
		WHERE owner_conv_id = ? OR generation IN (
			SELECT generation FROM codex_app_server_runtimes WHERE conv_id = ?
		)`, convID, convID); err != nil {
		return c, fmt.Errorf("mark native Codex profiles for cleanup: %w", err)
	}

	// Capture this conv's succession neighbours BEFORE the conv-scoped loop
	// below wipes its edges. If convID turns out to be a MIDDLE predecessor
	// generation (it has BOTH a predecessor and a successor), deleting it
	// removes pred→convID and convID→succ, which would strand any OLDER
	// generation's stale-id forwarding (ResolveLatestConv walks
	// agent_conv_succession, not agent_conversations). The predecessor branch
	// below re-links pred→succ so an ancestor still resolves forward to the
	// live head. bridgeReason carries the incoming edge's reason (how the
	// predecessor was superseded). Both empty for a head / genesis / non-agent.
	var bridgeOld, bridgeNew, bridgeReason string
	if err := tx.QueryRow(`SELECT old_conv_id, reason FROM agent_conv_succession
		WHERE new_conv_id = ? ORDER BY succeeded_at DESC, rowid DESC LIMIT 1`, convID).
		Scan(&bridgeOld, &bridgeReason); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return c, err
	}
	if err := tx.QueryRow(`SELECT new_conv_id FROM agent_conv_succession
		WHERE old_conv_id = ?`, convID).Scan(&bridgeNew); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return c, err
	}

	// Conv-scoped teardown: this conversation generation's runtime, history,
	// index and message rows. Always removed — they belong to THIS generation,
	// not to the actor as a whole.
	type convStep struct {
		stmt string
		into *int64
	}
	for _, s := range []convStep{
		{`DELETE FROM agent_messages WHERE from_conv = ?`, &c.MessagesFrom},
		{`DELETE FROM agent_messages WHERE to_conv = ?`, &c.MessagesTo},
		{`DELETE FROM agent_conv_succession WHERE old_conv_id = ?`, &c.SuccessionOld},
		{`DELETE FROM agent_conv_succession WHERE new_conv_id = ?`, &c.SuccessionNew},
		{`DELETE FROM conv_embeddings WHERE conv_id = ?`, &c.Embeddings},
		{`DELETE FROM conv_index WHERE conv_id = ?`, &c.ConvIndex},
		{`DELETE FROM conversation_resume_profiles WHERE conv_id = ?`, &c.ResumeProfiles},
		{`DELETE FROM sessions WHERE conv_id = ?`, &c.Sessions},
	} {
		res, err := tx.Exec(s.stmt, convID)
		if err != nil {
			return AgentDeletionCounts{}, fmt.Errorf("delete agent (%s): %w", s.stmt, err)
		}
		n, _ := res.RowsAffected()
		*s.into = n
	}

	// Actor-scoped teardown: the agent_id-keyed identity rows (memberships,
	// ownerships, permission overrides, notify pref) and the actor row itself.
	// Only when convID is the actor's CURRENT (live) generation. Under JOH-26 a
	// reincarnate / Claude Code /clear leaves the old conv around as a past
	// GENERATION of the same active actor — deleting one of those predecessors
	// must NOT wipe the live actor's identity. So:
	//   - current generation  → tear the whole actor down; the `agents` delete
	//     cascades every remaining agent_conversations link.
	//   - predecessor generation → unlink just this conv; the actor and its
	//     identity (and other generations) stay put.
	// agentID == "" means the conv is not an agent at all — nothing actor-level.
	if agentID != "" {
		var current string
		switch err := tx.QueryRow(
			`SELECT current_conv_id FROM agents WHERE agent_id = ?`, agentID).Scan(&current); {
		case errors.Is(err, sql.ErrNoRows):
			// Actor row already gone (e.g. an earlier partial delete) — clear any
			// stale generation link this conv still holds.
			if _, err := tx.Exec(`DELETE FROM agent_conversations WHERE conv_id = ?`, convID); err != nil {
				return AgentDeletionCounts{}, fmt.Errorf("delete agent (unlink generation): %w", err)
			}
		case err != nil:
			return c, err
		case current == convID:
			if unlaunched {
				p, err := scanFederationPresence(tx.QueryRow(`SELECT `+federationPresenceColumns+` FROM agent_federation_presence WHERE agent_id=?`, agentID))
				if err != nil {
					return c, err
				}
				if p != nil {
					if p.State != "here" || p.ArrivalOffer == "" || p.ArrivalRollbackJSON != "" {
						return c, errors.New("stable arrival cannot be deleted as an unlaunched birth")
					}
					if _, err := tx.Exec(`DELETE FROM agent_federation_presence WHERE agent_id=?`, agentID); err != nil {
						return c, err
					}
				}
			} else if err := terminalFederationPresenceTx(tx, agentID); err != nil {
				return c, err
			}
			type actorStep struct {
				stmt string
				into *int64
			}
			for _, s := range []actorStep{
				{`DELETE FROM agent_group_members WHERE agent_id = ?`, &c.GroupMembers},
				{`DELETE FROM agent_group_owners WHERE agent_id = ?`, &c.GroupOwners},
				{`DELETE FROM agent_permissions WHERE agent_id = ?`, &c.Permissions},
				{`DELETE FROM agent_retained_group_permissions WHERE agent_id = ?`, nil},
				{`DELETE FROM agent_notify_prefs WHERE agent_id = ?`, &c.NotifyPrefs},
				// Cron jobs are agent-keyed (JOH-26 PR3a): a job belongs to its
				// owner/target actor, so it is torn down with the actor (and only
				// then) — deleting a predecessor generation leaves the live
				// actor's schedules intact. Each delete cascades to
				// agent_cron_runs via the FK.
				{`DELETE FROM agent_cron_jobs WHERE owner_agent = ?`, &c.CronJobsOwned},
				{`DELETE FROM agent_cron_jobs WHERE target_agent = ?`, &c.CronJobsTarget},
				// Sudo grants and the agent-keyed spawn/clone rate-limit history
				// (JOH-26 PR2/PR3a) are keyed on agent_id but have NO FK to
				// agents, so the `agents` delete below does not cascade them.
				// Tear them down explicitly with the actor — otherwise they
				// orphan (the group export / count paths JOIN through agents, so
				// orphaned rows become invisible residue). Actor-scoped: a
				// predecessor delete leaves the live actor's grants + history
				// intact. This delete-set mirrors the identity-bearing set
				// absorbBareSuccessorActorTx guards on.
				{`DELETE FROM agent_sudo_grants WHERE agent_id = ?`, &c.SudoGrants},
				{`DELETE FROM agent_spawn_history WHERE spawner_agent_id = ?`, &c.SpawnHistory},
				{`DELETE FROM agent_clone_history WHERE source_agent_id = ?`, &c.CloneHistory},
				// The startup snapshot is agent-keyed without an FK, and holds
				// profile context text that should not outlive the actor.
				{`DELETE FROM agent_startup_snapshots WHERE agent_id = ?`, nil},
				{`DELETE FROM agents WHERE agent_id = ?`, nil}, // cascades agent_conversations
			} {
				res, err := tx.Exec(s.stmt, agentID)
				if err != nil {
					return AgentDeletionCounts{}, fmt.Errorf("delete agent (%s): %w", s.stmt, err)
				}
				if s.into != nil {
					n, _ := res.RowsAffected()
					*s.into = n
				}
			}
		default:
			// Predecessor generation: unlink just this conv; the actor lives on.
			if _, err := tx.Exec(`DELETE FROM agent_conversations WHERE conv_id = ?`, convID); err != nil {
				return AgentDeletionCounts{}, fmt.Errorf("delete agent (unlink generation): %w", err)
			}
			// Heal the succession chain when this was a MIDDLE generation: the
			// conv-scoped loop above removed its incoming (pred→convID) and
			// outgoing (convID→succ) edges, so re-link pred→succ. Without this a
			// stale reference to an ANCESTOR the caller did NOT delete would stop
			// forwarding to the live head. Idempotent upsert on old_conv_id (the
			// chain holds one successor per old conv). Skipped for a genesis
			// (no predecessor) or a pre-head (no successor) generation.
			if bridgeOld != "" && bridgeNew != "" && bridgeOld != bridgeNew {
				if _, err := tx.Exec(`INSERT INTO agent_conv_succession
					(old_conv_id, new_conv_id, reason, succeeded_at, agent_id)
					VALUES (?, ?, ?, ?, `+agentForSuccessionExpr+`)
					ON CONFLICT(old_conv_id) DO UPDATE SET
						new_conv_id = excluded.new_conv_id,
						reason = excluded.reason,
						succeeded_at = excluded.succeeded_at,
						agent_id = excluded.agent_id`,
					bridgeOld, bridgeNew, bridgeReason,
					dbTime(time.Now().UTC()), bridgeNew, bridgeOld); err != nil {
					return AgentDeletionCounts{}, fmt.Errorf("delete agent (bridge succession): %w", err)
				}
			}
			// Rebase any head_alias conv-anchored on THIS deleted predecessor onto
			// its immediate successor — the head_alias twin of the bridge above
			// (JOH-330). The conv-scoped loop wiped convID's anchor→successor edge,
			// so an alias left on convID would resolve (ResolveHeadAlias →
			// ResolveLatestConv) to the now-dead anchor instead of the live head;
			// forwarding the anchor one hop keeps it on the live succession chain, so
			// the alias's own chain-walk still lands on the head. bridgeNew is the
			// same forward target the succession bridge uses and is always present
			// for a non-head generation (the rotation off convID recorded the edge);
			// a no-op when no alias is anchored here. The live-head delete falls in
			// the actor-teardown branch above, where the alias legitimately breaks.
			if bridgeNew != "" && bridgeNew != convID {
				if _, err := rebaseHeadAliasAnchorTx(tx, convID, bridgeNew); err != nil {
					return AgentDeletionCounts{}, fmt.Errorf("delete agent (rebase head alias): %w", err)
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return AgentDeletionCounts{}, err
	}
	return c, nil
}

// RemoveAllAgentGroupMembershipsForConv drops every membership row
// referencing convID, regardless of group. Used by the conv-delete
// path so the dashboard's agent listing doesn't keep showing
// orphaned (unknown) entries after the underlying conversation is
// wiped. Idempotent — returns the number of rows removed.
func RemoveAllAgentGroupMembershipsForConv(convID string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return 0, err
	}
	if agentID == "" {
		return 0, nil
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE agent_groups SET route_generation = route_generation + 1 WHERE id IN (SELECT group_id FROM agent_group_members WHERE agent_id = ?)`, agentID); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM agent_group_members WHERE agent_id = ?`, agentID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.Exec(`DELETE FROM agent_retained_group_permissions WHERE agent_id = ?`, agentID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// ListAgentGroupMembers returns the members of a group, ordered by joined_at
// then conv_id. The membership is agent-keyed (JOH-26); the ConvID field is
// resolved to each actor's CURRENT conv for display/delivery.
func ListAgentGroupMembers(groupID int64) ([]*AgentGroupMember, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT m.group_id, ag.current_conv_id, m.role, m.descr, m.joined_at
		FROM agent_group_members m JOIN agents ag ON ag.agent_id = m.agent_id
		WHERE m.group_id = ? ORDER BY m.joined_at, ag.current_conv_id`, groupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*AgentGroupMember
	for rows.Next() {
		m, err := scanAgentGroupMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListGroupsForConv returns all groups that include the given conv_id,
// ordered by group name. conv_id is only the resolution input — it
// resolves to the stable actor and delegates to ListGroupsForAgent, so
// membership is read by agent and survives conv rotations.
func ListGroupsForConv(convID string) ([]*AgentGroup, error) {
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return nil, err
	}
	return ListGroupsForAgent(agentID)
}

// ListGroupsForAgent returns all groups that include the given stable
// agent_id, ordered by group name. This is the agent-keyed primitive
// behind ListGroupsForConv; the empty/unknown agent has no groups.
func ListGroupsForAgent(agentID string) ([]*AgentGroup, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, nil
	}
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(agentGroupAliasedSelect+`
		FROM agent_groups g
		JOIN agent_group_members m ON m.group_id = g.id
		WHERE m.agent_id = ?
		ORDER BY g.name`, agentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*AgentGroup
	for rows.Next() {
		g, err := scanAgentGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GroupNamesByConv returns a map from conv-id to its group names, alphabetised.
// Convs not in any group are absent from the map. The query scans the whole
// membership table (which is small — humans curate it manually), so callers
// don't need to pass a conv list.
func GroupNamesByConv() (map[string][]string, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	// Membership is agent-keyed; resolve to each actor's current conv for the
	// display-facing conv→group-names map.
	rows, err := db.Query(`SELECT ag.current_conv_id, g.name
		FROM agent_group_members m
		JOIN agents ag ON ag.agent_id = m.agent_id
		JOIN agent_groups g ON g.id = m.group_id
		ORDER BY ag.current_conv_id, g.name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string][]string{}
	for rows.Next() {
		var conv, name string
		if err := rows.Scan(&conv, &name); err != nil {
			return nil, err
		}
		out[conv] = append(out[conv], name)
	}
	return out, rows.Err()
}

// SharedGroupsForConvs returns the groups containing both convs, ordered by
// group name. Used by `agent message` to authorise sender→target.
func SharedGroupsForConvs(a, b string) ([]*AgentGroup, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	agentA, err := AgentIDForConv(a)
	if err != nil {
		return nil, err
	}
	agentB, err := AgentIDForConv(b)
	if err != nil {
		return nil, err
	}
	if agentA == "" || agentB == "" {
		return nil, nil
	}
	rows, err := db.Query(agentGroupAliasedSelect+`
		FROM agent_groups g
		JOIN agent_group_members ma ON ma.group_id = g.id AND ma.agent_id = ?
		JOIN agent_group_members mb ON mb.group_id = g.id AND mb.agent_id = ?
		ORDER BY g.name`, agentA, agentB)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*AgentGroup
	for rows.Next() {
		g, err := scanAgentGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// FindMemberInGroup returns the member entry for (group, conv) or nil if not
// present.
func FindMemberInGroup(groupID int64, convID string) (*AgentGroupMember, error) {
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return nil, err
	}
	if agentID == "" {
		return nil, nil
	}
	return FindAgentMemberInGroup(groupID, agentID)
}

// FindAgentMemberInGroup reads membership by its durable actor key. Use this
// for delivery revalidation: resolving through a conversation first creates a
// race with /clear or reincarnation between route and roster reads.
func FindAgentMemberInGroup(
	groupID int64,
	agentID string,
) (*AgentGroupMember, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, nil
	}
	row := db.QueryRow(`SELECT m.group_id, ag.current_conv_id, m.role, m.descr, m.joined_at
		FROM agent_group_members m JOIN agents ag ON ag.agent_id = m.agent_id
		WHERE m.group_id = ? AND m.agent_id = ?`, groupID, agentID)
	m, err := scanAgentGroupMember(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// ActiveGroupMemberRole is one member role held by active agents of a group.
type ActiveGroupMemberRole struct {
	GroupID int64
	Role    string
	Count   int
}

// ListActiveGroupMemberRoles counts, in one read, the non-empty roles held by
// active (unretired) members of every group.
func ListActiveGroupMemberRoles() ([]ActiveGroupMemberRole, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	return queryRows(db, func(rows rowScanner) (ActiveGroupMemberRole, error) {
		var r ActiveGroupMemberRole
		if err := rows.Scan(&r.GroupID, &r.Role, &r.Count); err != nil {
			return ActiveGroupMemberRole{}, err
		}
		return r, nil
	}, `SELECT m.group_id, m.role, COUNT(*)
		FROM agent_group_members m JOIN agents ag ON ag.agent_id = m.agent_id
		WHERE ag.retired_at IS NULL AND trim(m.role) != ''
		GROUP BY m.group_id, m.role ORDER BY m.group_id, m.role`)
}

// FindAgentMembersBySelector returns every row whose conv_id matches
// the selector. Used as a fallback by the agent CLI's target resolver:
// a conv that was just added to a group via spawn lives in
// agent_group_members before its .jsonl is scanned into conv_index, so
// the conv_index-only resolver can't find it by title yet — but its
// conv_id is already resolvable here.
//
// Match rules:
//   - exact conv_id
//   - prefix on conv_id (8+ chars typed, like the rest of tclaude)
//
// Returns rows in the order seen; the caller is responsible for
// deduping by conv_id and reporting ambiguity.
func FindAgentMembersBySelector(selector string) ([]*AgentGroupMember, error) {
	if selector == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	// Membership is agent-keyed; match the selector against ANY of the actor's
	// conversation generations (agent_conversations), then return its rows with
	// the actor's current conv for the ConvID field. The IN-subquery (rather
	// than a JOIN on agent_conversations) avoids fan-out when a prefix matches
	// several generations of the same actor.
	q := `SELECT m.group_id, ag.current_conv_id, m.role, m.descr, m.joined_at
		FROM agent_group_members m
		JOIN agents ag ON ag.agent_id = m.agent_id
		WHERE m.agent_id IN (
			SELECT agent_id FROM agent_conversations WHERE conv_id = ? OR conv_id LIKE ?
		)
		ORDER BY m.joined_at DESC`
	return queryRows(d, func(rows rowScanner) (*AgentGroupMember, error) {
		var m AgentGroupMember
		var joined dbTimestamp
		if err := rows.Scan(&m.GroupID, &m.ConvID, &m.Role, &m.Descr, &joined); err != nil {
			return nil, err
		}
		m.JoinedAt = joined.Time()
		return &m, nil
	}, q, selector, selector+"%")
}
