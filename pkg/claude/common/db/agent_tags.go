package db

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// agent_tags.go carries the per-agent tag set — a small set of short
// strings labelling an actor, keyed by the stable agent_id (see
// migrateV95toV96). Two kinds of tag share the store: free-form operator
// tags (set from the dashboard / `tclaude agent tags`) and the
// auto-stamped `tf:<template-name>` marker recording which task-force /
// template deployment spawned the agent (JOH-380). Tags are a SET —
// order is not stored; every read returns them sorted (alphabetical) so
// the CLI, dashboard and tests see one deterministic order.
//
// The store is DB + dashboard + CLI only; a tag never reaches a tmux
// pane, so the validation below is UI/sanity hygiene (printable, bounded,
// no control chars) rather than an injection guard.

const (
	// MaxAgentTagLen bounds one tag's length. Tags ride every 2s dashboard
	// snapshot and render as compact chips, so a runaway string mustn't be
	// storable; 48 chars is generous for a label like `tf:frontend-squad`.
	MaxAgentTagLen = 48
	// MaxAgentTags caps how many tags one agent may carry. A soft sanity
	// bound — a member row with dozens of chips is unreadable, and the set
	// is meant to be a handful of labels, not a free-text store.
	MaxAgentTags = 16
)

// NormalizeAgentTag trims a tag and reports the cleaned value. It is the
// single place the tag charset/length policy lives, shared by the write
// ops and the boundary validators. A tag must be non-empty after trim,
// within MaxAgentTagLen, and carry no control characters (newlines
// included) — the last keeps a tag to a single readable chip.
func NormalizeAgentTag(tag string) (string, error) {
	t := strings.TrimSpace(tag)
	if t == "" {
		return "", errors.New("tag is empty")
	}
	// Count runes, not bytes, so the cap reads the way a human sees the tag.
	if n := len([]rune(t)); n > MaxAgentTagLen {
		return "", fmt.Errorf("tag is too long (%d > %d chars)", n, MaxAgentTagLen)
	}
	for _, r := range t {
		if r == '\n' || r == '\r' || unicode.IsControl(r) {
			return "", errors.New("tag contains a control character")
		}
	}
	return t, nil
}

// normalizeAgentTagSet cleans, validates, de-duplicates and sorts a slice
// of raw tags into the canonical stored form. It rejects the whole set if
// any tag is invalid (callers surface that as a 400) and enforces the
// per-agent count cap on the DE-DUPLICATED set. Returns an empty (non-nil)
// slice for no tags.
func normalizeAgentTagSet(tags []string) ([]string, error) {
	seen := map[string]bool{}
	out := make([]string, 0, len(tags))
	for _, raw := range tags {
		t, err := NormalizeAgentTag(raw)
		if err != nil {
			return nil, err
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) > MaxAgentTags {
		return nil, fmt.Errorf("too many tags (%d > %d)", len(out), MaxAgentTags)
	}
	sort.Strings(out)
	return out, nil
}

// ListAgentTags returns one agent's tags, sorted alphabetically. A
// missing agent (or one with no tags) yields an empty slice, nil — the
// same shape callers get for "no tags", so they needn't special-case it.
func ListAgentTags(agentID string) ([]string, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, errors.New("ListAgentTags: agent_id required")
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT tag FROM agent_tags WHERE agent_id = ? ORDER BY tag`, agentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AddAgentTags unions the given tags onto an agent's existing set
// (INSERT OR IGNORE — additive, never removes). It is the auto-stamp and
// CLI-`add` write. Each tag is validated + trimmed; a duplicate of an
// existing tag is a silent no-op. Enforces the per-agent count cap on the
// RESULTING set, so add can't push an agent past MaxAgentTags. A blank
// agent_id or an empty tag list is a no-op (nil).
func AddAgentTags(agentID string, tags ...string) error {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return errors.New("AddAgentTags: agent_id required")
	}
	clean, err := normalizeAgentTagSet(tags)
	if err != nil {
		return err
	}
	if len(clean) == 0 {
		return nil
	}
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Cap the RESULTING set, counting only tags not already present — an
	// idempotent re-add of existing tags must not trip the cap.
	var existing int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM agent_tags WHERE agent_id = ?`, agentID).Scan(&existing); err != nil {
		return err
	}
	var incoming int
	for _, t := range clean {
		var have int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM agent_tags WHERE agent_id = ? AND tag = ?`, agentID, t).Scan(&have); err != nil {
			return err
		}
		if have == 0 {
			incoming++
		}
	}
	if existing+incoming > MaxAgentTags {
		return fmt.Errorf("adding %d tag(s) would exceed the per-agent cap (%d + %d > %d)",
			incoming, existing, incoming, MaxAgentTags)
	}
	for _, t := range clean {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO agent_tags (agent_id, tag) VALUES (?, ?)`, agentID, t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RemoveAgentTags deletes the given tags from an agent's set. Tags not
// present are silently ignored. Untrimmed input is trimmed to match the
// stored form (but not otherwise validated — removing is always safe). A
// blank agent_id or empty tag list is a no-op (nil).
func RemoveAgentTags(agentID string, tags ...string) error {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return errors.New("RemoveAgentTags: agent_id required")
	}
	if len(tags) == 0 {
		return nil
	}
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, raw := range tags {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM agent_tags WHERE agent_id = ? AND tag = ?`, agentID, t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReplaceAgentTags sets an agent's tag set to exactly the given tags
// (delete-all + insert), in one transaction. This is the replace-set
// write the API exposes — add/rm compose on top of it client-side in the
// CLI. The tags are validated + de-duplicated + count-capped up front; an
// empty list clears the set. A blank agent_id is an error.
func ReplaceAgentTags(agentID string, tags []string) error {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return errors.New("ReplaceAgentTags: agent_id required")
	}
	clean, err := normalizeAgentTagSet(tags)
	if err != nil {
		return err
	}
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM agent_tags WHERE agent_id = ?`, agentID); err != nil {
		return err
	}
	for _, t := range clean {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO agent_tags (agent_id, tag) VALUES (?, ?)`, agentID, t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListAllAgentTags returns every agent that carries at least one tag,
// keyed by agent_id, each value sorted alphabetically. The dashboard
// preloads it once per 2s snapshot and looks members up by agent_id
// rather than issuing a query per row (mirrors ListAgentTaskRefs).
// Agents with no tags are omitted so the map stays small.
func ListAllAgentTags() (map[string][]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT agent_id, tag FROM agent_tags ORDER BY agent_id, tag`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var id, tag string
		if err := rows.Scan(&id, &tag); err != nil {
			return nil, err
		}
		out[id] = append(out[id], tag)
	}
	return out, rows.Err()
}
