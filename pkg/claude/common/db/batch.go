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
			auto_registered, created_at, updated_at, last_hook, harness, sandbox_mode, ask_user_question_timeout, effective_sandbox_config, remote_control
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
	AgentID     string
	PendingName string
	// CreatedAt is the actor's immutable birth timestamp (agents.created_at),
	// stamped at spawn/enrollment BEFORE the harness writes its first .jsonl
	// event. It is the dashboard member Age source: available the instant the
	// agent row exists, so a freshly-spawned agent shows a real Age immediately
	// rather than blank until the .jsonl is parsed into conv_index. Normalised to
	// UTC RFC3339Nano (canonical zone, full precision preserved) so it sorts
	// lexically and agrees byte-for-byte with the CLI listing's agent.MemberCreated.
	CreatedAt string
}

// normalizeCreatedAtUTC canonicalises a stored created_at string to UTC
// RFC3339Nano, preserving sub-second precision. agents.created_at is written
// with time.Now().Format(RFC3339Nano), i.e. in the daemon's LOCAL zone; leaving
// that offset in place would make the Age lexical sort mix zones and make the
// dashboard row disagree with the CLI listing (which formats via time.Time.UTC).
// An unparseable value is returned unchanged rather than blanked.
func normalizeCreatedAtUTC(s string) string {
	if s == "" {
		return ""
	}
	if t := parseTimeOrZero(s); !t.IsZero() {
		return t.UTC().Format(time.RFC3339Nano)
	}
	return s
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

// AgentsByConv bulk-resolves each conv-id to its owning actor (agent_id +
// pending_name) via the agent_conversations JOIN, keyed by conv-id. A conv that
// is not (yet) an agent has no entry — the same "" agent_id signal
// AgentIDForConv returns for a plain conversation.
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
		rows, err := d.Query(`SELECT ac.conv_id, a.agent_id, a.pending_name, a.created_at
			FROM agent_conversations ac
			JOIN agents a ON a.agent_id = ac.agent_id
			WHERE ac.conv_id `+clause, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var convID string
			var ca ConvAgent
			var createdAt string
			if err := rows.Scan(&convID, &ca.AgentID, &ca.PendingName, &createdAt); err != nil {
				_ = rows.Close()
				return nil, err
			}
			// Canonicalise the zone to UTC (keeping full sub-second precision) so
			// the Age lexical sort is valid and the value agrees byte-for-byte with
			// the CLI path (agent.MemberCreated). agents.created_at is written with
			// time.Now(), whose local offset would otherwise break both.
			ca.CreatedAt = normalizeCreatedAtUTC(createdAt)
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
