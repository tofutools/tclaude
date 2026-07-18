package db

import (
	"database/sql"
	"errors"
	"strings"
)

// AgentTaskRef is one agent's task-reference link — an http(s) URL
// pointing at the work item the agent is on (a Linear issue, a GitHub
// issue/PR, a ticket, …) plus an optional human-set display label. When
// Label is empty, callers derive a display label from the URL. Stored
// per-agent (see migrateV87toV88), so it survives conv rotation and an
// agent can set its own with no group argument.
type AgentTaskRef struct {
	URL   string
	Label string
}

// SetAgentTaskRef sets (or, with url == "", clears) an agent's task
// reference link. A plain UPDATE keyed by the stable agent_id — a no-op
// when the agent is unknown (returns rowsAffected 0). Clearing the URL
// also clears any explicit label so a stale label can't outlive the link
// it belonged to. Returns the number of rows updated so a caller can
// distinguish "set" from "no such agent".
func SetAgentTaskRef(agentID, url, label string) (int64, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return 0, errors.New("SetAgentTaskRef: agent_id required")
	}
	url = strings.TrimSpace(url)
	label = strings.TrimSpace(label)
	if url == "" {
		// Clearing the link clears the override label with it.
		label = ""
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`UPDATE agents SET task_ref_url = ?, task_ref_label = ? WHERE agent_id = ?`,
		url, label, agentID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SetAgentTaskRefIfEmpty sets an agent's task-reference link only when no
// link is currently stored — a single compare-and-set UPDATE, so a
// concurrent edit (an operator setting a different link between a caller's
// read and write) can never be clobbered by a stale value. Returns the
// number of rows updated: 0 means either the agent already has a link (the
// caller's value must not win) or no such agent exists — GetAgentTaskRef
// distinguishes the two when a caller needs to.
func SetAgentTaskRefIfEmpty(agentID, url, label string) (int64, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return 0, errors.New("SetAgentTaskRefIfEmpty: agent_id required")
	}
	url = strings.TrimSpace(url)
	label = strings.TrimSpace(label)
	if url == "" {
		return 0, errors.New("SetAgentTaskRefIfEmpty: url required (use SetAgentTaskRef to clear)")
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`UPDATE agents SET task_ref_url = ?, task_ref_label = ?
		WHERE agent_id = ? AND task_ref_url = ''`,
		url, label, agentID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GetAgentTaskRef returns an agent's stored task-reference URL and
// explicit label (both "" when unset). A missing agent yields ("", "",
// nil) — the same shape as an agent with no link — so callers needn't
// special-case it.
func GetAgentTaskRef(agentID string) (AgentTaskRef, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return AgentTaskRef{}, errors.New("GetAgentTaskRef: agent_id required")
	}
	d, err := Open()
	if err != nil {
		return AgentTaskRef{}, err
	}
	var ref AgentTaskRef
	err = d.QueryRow(`SELECT task_ref_url, task_ref_label FROM agents WHERE agent_id = ?`, agentID).
		Scan(&ref.URL, &ref.Label)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentTaskRef{}, nil
	}
	if err != nil {
		return AgentTaskRef{}, err
	}
	return ref, nil
}

// ListAgentTaskRefs returns every agent that has a task-reference URL
// set, keyed by agent_id. Rows with an empty URL are omitted so the map
// stays small — the dashboard preloads it once per snapshot and looks up
// members by agent_id rather than issuing a query per row.
func ListAgentTaskRefs() (map[string]AgentTaskRef, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT agent_id, task_ref_url, task_ref_label FROM agents WHERE task_ref_url <> ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]AgentTaskRef{}
	for rows.Next() {
		var id string
		var ref AgentTaskRef
		if err := rows.Scan(&id, &ref.URL, &ref.Label); err != nil {
			return nil, err
		}
		out[id] = ref
	}
	return out, rows.Err()
}

// ListAgentTaskRefsByAgentIDs batch-loads task references for the requested
// stable actors, keyed by agent_id. It is one SELECT for up to batchChunkSize
// actors (not one query per actor), and only splits larger sets to stay below
// SQLite's bound-variable limit. The dashboard passes its deduplicated visible
// actor set so the 2-second snapshot never scans task links belonging to
// invisible or retired agents. An empty set returns without opening SQLite.
func ListAgentTaskRefsByAgentIDs(agentIDs []string) (map[string]AgentTaskRef, error) {
	out := make(map[string]AgentTaskRef, len(agentIDs))
	if len(agentIDs) == 0 {
		return out, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	for _, chunk := range chunkStrings(agentIDs, batchChunkSize) {
		clause, args := inClause(chunk)
		rows, err := d.Query(`SELECT agent_id, task_ref_url, task_ref_label
			FROM agents WHERE task_ref_url <> '' AND agent_id `+clause, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var ref AgentTaskRef
			if err := rows.Scan(&id, &ref.URL, &ref.Label); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[id] = ref
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
