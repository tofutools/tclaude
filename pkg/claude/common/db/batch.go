package db

import (
	"log/slog"
	"time"
)

// Batch loaders for the dashboard snapshot hot path (TCL-368). Building one
// member/agent/owner row used to fire ~13 SQLite point queries, and the
// handler rebuilds the same conv 2-3× per poll (member loop, addAgent, owners
// pass) at a 2s cadence. These loaders replace the per-row point queries with
// one chunked IN-query per table: the caller collects the whole conv set once,
// bulk-loads every table keyed by conv-id, and looks each row up from a map.
//
// Every loader chunks its id list at batchChunkSize so a few hundred agents
// never blow past SQLite's bound-variable ceiling, and returns a map keyed by
// conv-id (or repo hash) — a missing conv simply has no entry, mirroring the
// zero-value / nil-row shape the singular getters return for an unknown conv.

// batchChunkSize bounds how many ids ride in a single IN (...) clause. SQLite's
// default SQLITE_MAX_VARIABLE_NUMBER is comfortably above this, so ≤500 keeps
// every chunk well within the limit while keeping the round-trip count low.
const batchChunkSize = 500

// chunkStrings splits ids into consecutive slices of at most size elements.
// The last chunk carries the remainder; an empty input yields no chunks.
func chunkStrings(ids []string, size int) [][]string {
	if size <= 0 {
		size = len(ids)
	}
	var out [][]string
	for len(ids) > 0 {
		n := min(size, len(ids))
		out = append(out, ids[:n])
		ids = ids[n:]
	}
	return out
}

func chunkInt64s(ids []int64, size int) [][]int64 {
	if size <= 0 {
		size = len(ids)
	}
	var out [][]int64
	for len(ids) > 0 {
		n := min(size, len(ids))
		out = append(out, ids[:n])
		ids = ids[n:]
	}
	return out
}

// ListAgentGroupMembersBatch loads all requested memberships in at most one
// query per batchChunkSize groups. Empty input returns without opening SQLite.
func ListAgentGroupMembersBatch(groupIDs []int64) (map[int64][]*AgentGroupMember, error) {
	out := make(map[int64][]*AgentGroupMember, len(groupIDs))
	if len(groupIDs) == 0 {
		return out, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	for _, chunk := range chunkInt64s(groupIDs, batchChunkSize) {
		args := make([]any, len(chunk))
		for i, groupID := range chunk {
			args[i] = groupID
		}
		rows, err := d.Query(`SELECT m.group_id, ag.current_conv_id, m.role, m.descr, m.joined_at
			FROM agent_group_members m JOIN agents ag ON ag.agent_id = m.agent_id
			WHERE m.group_id IN (`+sqlPlaceholders(len(chunk))+`)
			ORDER BY m.group_id, m.joined_at, ag.current_conv_id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			member, err := scanAgentGroupMember(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[member.GroupID] = append(out[member.GroupID], member)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ListAgentGroupOwnersBatch is the owner-side companion to the membership
// batch loader. Empty input likewise performs no DB open or query.
func ListAgentGroupOwnersBatch(groupIDs []int64) (map[int64][]*AgentGroupOwner, error) {
	out := make(map[int64][]*AgentGroupOwner, len(groupIDs))
	if len(groupIDs) == 0 {
		return out, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	for _, chunk := range chunkInt64s(groupIDs, batchChunkSize) {
		args := make([]any, len(chunk))
		for i, groupID := range chunk {
			args[i] = groupID
		}
		rows, err := d.Query(`SELECT o.group_id, o.agent_id, ag.current_conv_id, o.granted_at, o.granted_by
			FROM agent_group_owners o JOIN agents ag ON ag.agent_id = o.agent_id
			WHERE o.group_id IN (`+sqlPlaceholders(len(chunk))+`)
			ORDER BY o.group_id, o.granted_at DESC`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var owner AgentGroupOwner
			var grantedAt string
			if err := rows.Scan(&owner.GroupID, &owner.AgentID, &owner.ConvID, &grantedAt, &owner.GrantedBy); err != nil {
				_ = rows.Close()
				return nil, err
			}
			owner.GrantedAt = parseTimeOrZero(grantedAt)
			out[owner.GroupID] = append(out[owner.GroupID], &owner)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetConvIndexBatch bulk-loads conv_index rows for the given conv-ids, keyed by
// conv-id. A conv with no row simply has no map entry — the same "nil row"
// signal GetConvIndex returns for an unknown conv.
func GetConvIndexBatch(convIDs []string) (map[string]*ConvIndexRow, error) {
	out := make(map[string]*ConvIndexRow, len(convIDs))
	if len(convIDs) == 0 {
		return out, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	for _, chunk := range chunkStrings(convIDs, batchChunkSize) {
		clause, args := inClause(chunk)
		rows, err := d.Query(`SELECT conv_id, project_dir, full_path, file_mtime, file_size,
			first_prompt, summary, custom_title, message_count,
			created, modified, git_branch, project_path, is_sidechain, indexed_at,
			archived_at, git_branch_startup, harness
			FROM conv_index WHERE conv_id `+clause, args...)
		if err != nil {
			return nil, err
		}
		scanned, err := scanConvIndexRows(rows)
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		for _, r := range scanned {
			out[r.ConvID] = r
		}
	}
	return out, nil
}

// FindSessionsByConvIDs bulk-loads the session rows for the given conv-ids,
// grouped by conv-id and — within each conv — ordered most-recently-updated
// first, exactly as FindSessionsByConvID returns them. The daemon uses the
// first live-tmux row per conv, so the per-conv ordering must be preserved.
//
// Unlike scanSessions (all-or-nothing), this is BEST-EFFORT per row: a single
// undecodable row — realistically a corrupt effective_sandbox_config JSON blob —
// is logged and skipped rather than failing the whole batch. That matters here
// because this loader backs the whole dashboard poll: an aborted read would
// return an empty map and render EVERY agent offline with empty state, whereas
// the singular FindSessionsByConvID confines a bad row to its one conv. Skipping
// the row keeps every other conv's liveness/state intact; the affected conv
// degrades to its remaining good rows (or offline if it had only the bad one).
func FindSessionsByConvIDs(convIDs []string) (map[string][]*SessionRow, error) {
	out := make(map[string][]*SessionRow, len(convIDs))
	if len(convIDs) == 0 {
		return out, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	for _, chunk := range chunkStrings(convIDs, batchChunkSize) {
		clause, args := inClause(chunk)
		rows, err := d.Query(`SELECT id, tmux_session, pid, cwd, conv_id, status, status_detail, subagent_count, subagents_json,
			auto_registered, created_at, updated_at, last_hook, harness, sandbox_mode, ask_user_question_timeout, effective_sandbox_config, remote_control, approval_policy, approval_auto_review, resume_provenance
			FROM sessions WHERE conv_id `+clause+`
			ORDER BY conv_id, updated_at DESC`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			s, err := scanSessionRow(rows)
			if err != nil {
				slog.Warn("db: skipping undecodable session row in batch load",
					"error", err, "module", "db")
				continue
			}
			out[s.ConvID] = append(out[s.ConvID], s)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ListAgentWorkdirsByConv bulk-loads agent_workdir rows for the given conv-ids,
// keyed by conv-id. A conv with no row has no entry (the caller then falls back
// to the launch cwd, as with GetAgentWorkdir's zero value).
func ListAgentWorkdirsByConv(convIDs []string) (map[string]AgentWorkdir, error) {
	out := make(map[string]AgentWorkdir, len(convIDs))
	if len(convIDs) == 0 {
		return out, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	for _, chunk := range chunkStrings(convIDs, batchChunkSize) {
		clause, args := inClause(chunk)
		rows, err := d.Query(`SELECT conv_id, dir, worktree_root, branch, updated_at
			FROM agent_workdir WHERE conv_id `+clause, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var w AgentWorkdir
			var updatedStr string
			if err := rows.Scan(&w.ConvID, &w.Dir, &w.WorktreeRoot, &w.Branch, &updatedStr); err != nil {
				_ = rows.Close()
				return nil, err
			}
			w.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)
			out[w.ConvID] = w
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ListAgentWorkspacesByConv bulk-loads agent_workspace rows for the given
// conv-ids, keyed by conv-id. A conv with no row has no entry (the caller falls
// back to the older writers, as with GetAgentWorkspace's zero value).
func ListAgentWorkspacesByConv(convIDs []string) (map[string]AgentWorkspace, error) {
	out := make(map[string]AgentWorkspace, len(convIDs))
	if len(convIDs) == 0 {
		return out, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	for _, chunk := range chunkStrings(convIDs, batchChunkSize) {
		clause, args := inClause(chunk)
		rows, err := d.Query(`SELECT conv_id, cwd, branch, repo_url, default_branch,
				pr_number, pr_url, pr_state, updated_at
			FROM agent_workspace WHERE conv_id `+clause, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var w AgentWorkspace
			var updatedStr string
			if err := rows.Scan(&w.ConvID, &w.Cwd, &w.Branch, &w.RepoURL, &w.DefaultBranch,
				&w.PRNumber, &w.PRURL, &w.PRState, &updatedStr); err != nil {
				_ = rows.Close()
				return nil, err
			}
			w.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedStr)
			out[w.ConvID] = w
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ConvAgent is the actor identity of one conversation generation — the stable
// agent_id plus the actor's spawn-time pending name and birth timestamp —
// resolved through agent_conversations. It carries exactly the actor facts the
// dashboard row needs (the id for task-ref / tag / presented-PR lookups, the
// pending name as a title fallback, and created_at as the member Age source)
// without a per-conv AgentIDForConv + GetAgent pair.
type ConvAgent struct {
	AgentID       string
	CurrentConvID string
	PendingName   string
	Retired       bool
	Superseded    bool
	// CreatedAt is the actor's immutable birth timestamp (agents.created_at),
	// stamped at spawn/enrollment BEFORE the harness writes its first .jsonl
	// event. It is the dashboard member Age source: available the instant the
	// agent row exists, so a freshly-spawned agent shows a real Age immediately
	// rather than blank until the .jsonl is parsed into conv_index. Canonicalised
	// to UTC RFC3339Nano so it agrees byte-for-byte with the CLI listing's
	// agent.MemberCreated. Consumers compare Age values as instants, not strings.
	CreatedAt string
}

// CanonicalAgeTimestamp normalises a stored timestamp string to UTC
// RFC3339Nano, preserving all available sub-second precision. The dashboard
// and CLI use the same representation, but ordering never relies on its text:
// server and browser Age sorters parse it and compare the resulting instants.
// Empty stays empty; an unparseable value is returned unchanged rather than
// blanked so callers can degrade it to an unknown sort key without losing the
// stored diagnostic value.
func CanonicalAgeTimestamp(s string) string {
	if s == "" {
		return ""
	}
	if t := parseTimeOrZero(s); !t.IsZero() {
		return t.UTC().Format(time.RFC3339Nano)
	}
	return s
}

// CanonicalAgeTimestampFromTime formats an actor birth time.Time (from
// agents.created_at) into the same UTC RFC3339Nano representation
// CanonicalAgeTimestamp produces, so the CLI listing's value is byte-identical
// to the dashboard's. A zero time yields "" (unknown Age).
func CanonicalAgeTimestampFromTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// EarliestAgeTimestamp returns the earliest valid instant in values, emitted as
// canonical UTC RFC3339Nano. An actor's created_at normally wins because it
// predates later conversation generations, while an older conv_index.Created
// repairs legacy/backfilled actors whose row was stamped at migration or lazy
// enrollment time. If no value parses, the first non-empty value is preserved
// for diagnostics; Age sorters treat it as unknown.
func EarliestAgeTimestamp(values ...string) string {
	var earliest time.Time
	fallback := ""
	for _, value := range values {
		if value == "" {
			continue
		}
		if fallback == "" {
			fallback = value
		}
		parsed := parseTimeOrZero(value)
		if parsed.IsZero() {
			continue
		}
		if earliest.IsZero() || parsed.Before(earliest) {
			earliest = parsed
		}
	}
	if earliest.IsZero() {
		return fallback
	}
	return CanonicalAgeTimestampFromTime(earliest)
}

// PendingNamesByAgent bulk-loads the non-empty spawn-time display-name
// fallback for the requested stable actors, keyed by agent_id. Stable-keyed
// surfaces use this instead of walking back through a conversation generation:
// the actor name remains resolvable after a /clear or reincarnation rotates the
// current conv-id.
func PendingNamesByAgent(agentIDs []string) (map[string]string, error) {
	out := make(map[string]string, len(agentIDs))
	if len(agentIDs) == 0 {
		return out, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	for _, chunk := range chunkStrings(agentIDs, batchChunkSize) {
		clause, args := inClause(chunk)
		rows, err := d.Query(`SELECT agent_id, pending_name FROM agents
			WHERE pending_name != '' AND agent_id `+clause, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var agentID, pendingName string
			if err := rows.Scan(&agentID, &pendingName); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[agentID] = pendingName
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// AgentsByConv bulk-resolves each conv-id to its owning actor via the
// agent_conversations JOIN, keyed by conv-id. CurrentConvID, Retired, and the
// indexed succession existence check let snapshot callers reject a retired
// actor or superseded generation without separately loading the full retired
// roster and succession history. A conv that is not (yet) an agent has no
// entry — the same "" agent_id signal AgentIDForConv returns for a plain
// conversation.
func AgentsByConv(convIDs []string) (map[string]ConvAgent, error) {
	out := make(map[string]ConvAgent, len(convIDs))
	if len(convIDs) == 0 {
		return out, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	for _, chunk := range chunkStrings(convIDs, batchChunkSize) {
		clause, args := inClause(chunk)
		rows, err := d.Query(`SELECT ac.conv_id, a.agent_id, a.current_conv_id,
				a.pending_name, a.created_at, a.retired_at,
				s.old_conv_id IS NOT NULL
			FROM agent_conversations ac
			JOIN agents a ON a.agent_id = ac.agent_id
			LEFT JOIN agent_conv_succession s ON s.old_conv_id = ac.conv_id
			WHERE ac.conv_id `+clause, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var convID string
			var ca ConvAgent
			var createdAt, retiredAt string
			if err := rows.Scan(&convID, &ca.AgentID, &ca.CurrentConvID,
				&ca.PendingName, &createdAt, &retiredAt, &ca.Superseded); err != nil {
				_ = rows.Close()
				return nil, err
			}
			ca.Retired = retiredAt != ""
			// Canonicalise to UTC RFC3339Nano (keeping full sub-second precision)
			// so the value agrees byte-for-byte with the CLI path
			// (agent.MemberCreated). Age consumers compare parsed instants.
			ca.CreatedAt = CanonicalAgeTimestamp(createdAt)
			out[convID] = ca
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// LoadGitCacheBatch bulk-loads git_cache rows for the given repo-hash keys,
// keyed by repo hash. A key with no row has no entry — the same nil signal
// LoadGitCache returns for a cold miss (the caller then schedules a refresh).
func LoadGitCacheBatch(repoHashes []string) (map[string]*GitCacheRow, error) {
	out := make(map[string]*GitCacheRow, len(repoHashes))
	if len(repoHashes) == 0 {
		return out, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	for _, chunk := range chunkStrings(repoHashes, batchChunkSize) {
		clause, args := inClause(chunk)
		rows, err := d.Query(`SELECT repo_hash, data, fetched_at FROM git_cache WHERE repo_hash `+clause, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key, dataStr, fetchedStr string
			if err := rows.Scan(&key, &dataStr, &fetchedStr); err != nil {
				_ = rows.Close()
				return nil, err
			}
			row := &GitCacheRow{Data: []byte(dataStr)}
			row.FetchedAt, _ = time.Parse(time.RFC3339Nano, fetchedStr)
			out[key] = row
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
