package db

import (
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// AgentPR is one explicitly presented pull request. The agent_id is the stable
// actor key; (agent_id, PRURL) is unique so one agent can refresh its own PR
// badge while another agent may present the same PR independently. State is
// intentionally loose: callers currently use "", "open", "draft", "merged",
// or "handled".
type AgentPR struct {
	ID                int64
	AgentID           string
	PRURL             string
	Summary           string
	State             string
	ValidatedRepoRoot string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// UpsertAgentPR presents or refreshes a PR for an agent, deduped by
// (agent_id, PR URL).
func UpsertAgentPR(agentID, prURL, summary, state string) (AgentPR, error) {
	return upsertAgentPRDetails(agentID, prURL, summary, state, "", strings.EqualFold(strings.TrimSpace(state), "draft"), "")
}

// UpsertValidatedAgentPR records the daemon-proved local repository root.
// Credentialed consumers ignore rows without this proof, quarantining rows
// written before repository validation existed.
func UpsertValidatedAgentPR(agentID, prURL, summary, state, repoRoot string) (AgentPR, error) {
	return upsertAgentPRDetails(agentID, prURL, summary, state, "", strings.EqualFold(strings.TrimSpace(state), "draft"), strings.TrimSpace(repoRoot))
}

// UpsertAgentPRDetails is the trigger-aware presentation boundary. When the
// opt-in trigger feature is enabled, the PR row and its opened/updated
// observation commit together: agentd may crash before evaluation, but a
// restart can reconcile the durable pending row. With the feature off,
// presentation stays unchanged and writes no trigger event. Re-presenting the
// same PR never creates a second opening edge; pending update edges coalesce.
func UpsertAgentPRDetails(agentID, prURL, summary, state, branch string, draft bool) (AgentPR, error) {
	return upsertAgentPRDetails(agentID, prURL, summary, state, branch, draft, "")
}

// UpsertValidatedAgentPRDetails combines repository validation provenance with
// the trigger-aware presentation fields used when the GitHub proxy creates a PR.
func UpsertValidatedAgentPRDetails(agentID, prURL, summary, state, branch string, draft bool, repoRoot string) (AgentPR, error) {
	return upsertAgentPRDetails(agentID, prURL, summary, state, branch, draft, strings.TrimSpace(repoRoot))
}

func upsertAgentPRDetails(agentID, prURL, summary, state, branch string, draft bool, repoRoot string) (AgentPR, error) {
	agentID = strings.TrimSpace(agentID)
	prURL = strings.TrimSpace(prURL)
	summary = strings.TrimSpace(summary)
	state = strings.TrimSpace(state)
	if agentID == "" {
		return AgentPR{}, errors.New("UpsertAgentPR: agent_id required")
	}
	if prURL == "" {
		return AgentPR{}, errors.New("UpsertAgentPR: pr_url required")
	}
	now := time.Now().UTC()
	d, err := Open()
	if err != nil {
		return AgentPR{}, err
	}
	tx, err := d.Begin()
	if err != nil {
		return AgentPR{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var existed bool
	var previousState string
	err = tx.QueryRow(`SELECT state FROM agent_prs WHERE agent_id=? AND pr_url=?`, agentID, prURL).Scan(&previousState)
	switch {
	case err == nil:
		existed = true
	case errors.Is(err, sql.ErrNoRows):
	default:
		return AgentPR{}, err
	}
	if _, err := tx.Exec(`INSERT INTO agent_prs
		(agent_id, pr_url, summary, state, validated_repo_root, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(agent_id, pr_url) DO UPDATE SET
			summary = excluded.summary,
			state = excluded.state,
			validated_repo_root = excluded.validated_repo_root,
			updated_at = excluded.updated_at`,
		agentID, prURL, summary, state, repoRoot, dbTime(now), dbTime(now)); err != nil {
		return AgentPR{}, err
	}
	var row AgentPR
	var created, updated dbTimestamp
	if err := tx.QueryRow(`SELECT id, agent_id, pr_url, summary, state, validated_repo_root, created_at, updated_at
		FROM agent_prs WHERE agent_id = ? AND pr_url = ?`, agentID, prURL).
		Scan(&row.ID, &row.AgentID, &row.PRURL, &row.Summary, &row.State, &row.ValidatedRepoRoot, &created, &updated); err != nil {
		return AgentPR{}, err
	}
	row.CreatedAt = created.Time()
	row.UpdatedAt = updated.Time()
	cfg, configErr := config.Load()
	if configErr == nil && cfg.TriggersEnabled() {
		if !existed {
			err = enqueueTriggerPREventTx(tx, row, branch, draft, now)
		} else {
			err = enqueueTriggerTransitionTx(tx, row, TriggerSourcePRUpdated, previousState, state, branch, draft, now)
			if err == nil && !strings.EqualFold(strings.TrimSpace(previousState), "merged") && strings.EqualFold(state, "merged") {
				err = enqueueTriggerTransitionTx(tx, row, TriggerSourcePRMerged, previousState, "merged", branch, draft, now)
			}
		}
		if err != nil {
			return AgentPR{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return AgentPR{}, err
	}
	return row, nil
}

// MarkAgentPRHandled marks one PR as handled without deleting the historical
// row. Handled PRs are omitted from dashboard presentation.
func MarkAgentPRHandled(agentID, prURL string) (int64, error) {
	agentID = strings.TrimSpace(agentID)
	prURL = strings.TrimSpace(prURL)
	if agentID == "" {
		return 0, errors.New("MarkAgentPRHandled: agent_id required")
	}
	if prURL == "" {
		return 0, errors.New("MarkAgentPRHandled: pr_url required")
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`UPDATE agent_prs
		SET state = 'handled', updated_at = ?
		WHERE agent_id = ? AND pr_url = ?`,
		dbTime(time.Now().UTC()), agentID, prURL)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// UpdateAgentPRState records a presented PR state observation without touching
// its summary. UpdatedAt advances even when the state is unchanged: dashboard
// reconciliation uses it as the observation's freshness clock, not merely a
// mutation timestamp. It is used by the daemon's best-effort GitHub polling
// path, where writing an old summary from a stale in-memory row would be
// surprising.
//
// It never resurrects a handled row or regresses a merged row. Both guards
// protect slow polling races: a refresh may start from an open snapshot, then
// finish after another poll marked the row handled or merged. A GitHub PR
// cannot transition out of merged, so the late open/closed result is stale
// regardless of its write time. Only an explicit re-present via UpsertAgentPR
// may bring a handled PR back.
func UpdateAgentPRState(agentID, prURL, state string) (int64, error) {
	return updateAgentPRState(agentID, prURL, state, true)
}

// UpdateAgentPRStateQuiet applies a resolver result to a duplicate presentation
// without creating a second lifecycle edge for the same canonical PR.
func UpdateAgentPRStateQuiet(agentID, prURL, state string) (int64, error) {
	return updateAgentPRState(agentID, prURL, state, false)
}

func updateAgentPRState(agentID, prURL, state string, emitTrigger bool) (int64, error) {
	agentID = strings.TrimSpace(agentID)
	prURL = strings.TrimSpace(prURL)
	state = strings.TrimSpace(state)
	if agentID == "" {
		return 0, errors.New("UpdateAgentPRState: agent_id required")
	}
	if prURL == "" {
		return 0, errors.New("UpdateAgentPRState: pr_url required")
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var before AgentPR
	var created, updated dbTimestamp
	err = tx.QueryRow(`SELECT id,agent_id,pr_url,summary,state,created_at,updated_at FROM agent_prs
		WHERE agent_id=? AND pr_url=?`, agentID, prURL).Scan(&before.ID, &before.AgentID, &before.PRURL,
		&before.Summary, &before.State, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	before.CreatedAt, before.UpdatedAt = created.Time(), updated.Time()
	now := time.Now().UTC()
	res, err := tx.Exec(`UPDATE agent_prs
		SET state = ?, updated_at = ?
		WHERE agent_id = ? AND pr_url = ? AND state <> 'handled'
			AND (LOWER(TRIM(state)) <> 'merged' OR LOWER(TRIM(?)) = 'merged')`,
		state, dbTime(now), agentID, prURL, state)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return n, err
	}
	if cfg, cfgErr := config.Load(); emitTrigger && cfgErr == nil && cfg.TriggersEnabled() &&
		!strings.EqualFold(strings.TrimSpace(before.State), "merged") && strings.EqualFold(state, "merged") {
		previous := before.State
		before.State = state
		before.UpdatedAt = now
		if err := enqueueTriggerTransitionTx(tx, before, TriggerSourcePRMerged, previous, "merged", "", strings.EqualFold(previous, "draft"), now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// GetAgentPR returns the row for an agent+URL pair, or the zero value when
// missing.
func GetAgentPR(agentID, prURL string) (AgentPR, error) {
	agentID = strings.TrimSpace(agentID)
	prURL = strings.TrimSpace(prURL)
	if agentID == "" {
		return AgentPR{}, errors.New("GetAgentPR: agent_id required")
	}
	if prURL == "" {
		return AgentPR{}, errors.New("GetAgentPR: pr_url required")
	}
	d, err := Open()
	if err != nil {
		return AgentPR{}, err
	}
	var row AgentPR
	var created, updated dbTimestamp
	err = d.QueryRow(`SELECT id, agent_id, pr_url, summary, state, validated_repo_root, created_at, updated_at
		FROM agent_prs WHERE agent_id = ? AND pr_url = ?`, agentID, prURL).
		Scan(&row.ID, &row.AgentID, &row.PRURL, &row.Summary, &row.State, &row.ValidatedRepoRoot, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return AgentPR{}, nil
	}
	if err != nil {
		return AgentPR{}, err
	}
	row.CreatedAt = created.Time()
	row.UpdatedAt = updated.Time()
	return row, nil
}

// ListUnhandledAgentPRs returns all presented PRs whose state is not handled.
// This preserves the pre-Git-proxy presentation behavior when that proxy is
// disabled.
func ListUnhandledAgentPRs() (map[string][]AgentPR, error) {
	return listUnhandledAgentPRs(false)
}

// ListValidatedUnhandledAgentPRs excludes rows without repository provenance
// for credentialed consumers used while the Git proxy is active.
func ListValidatedUnhandledAgentPRs() (map[string][]AgentPR, error) {
	return listUnhandledAgentPRs(true)
}

func listUnhandledAgentPRs(requireValidation bool) (map[string][]AgentPR, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	query := `SELECT id, agent_id, pr_url, summary, state, validated_repo_root, created_at, updated_at
		FROM agent_prs WHERE state <> 'handled'`
	if requireValidation {
		query += ` AND validated_repo_root <> ''`
	}
	query += ` ORDER BY updated_at DESC, id DESC`
	rows, err := d.Query(query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	out := map[string][]AgentPR{}
	for rows.Next() {
		var row AgentPR
		var created, updated dbTimestamp
		if err := rows.Scan(&row.ID, &row.AgentID, &row.PRURL, &row.Summary, &row.State, &row.ValidatedRepoRoot, &created, &updated); err != nil {
			return nil, err
		}
		row.CreatedAt = created.Time()
		row.UpdatedAt = updated.Time()
		out[row.AgentID] = append(out[row.AgentID], row)
	}
	return out, rows.Err()
}
