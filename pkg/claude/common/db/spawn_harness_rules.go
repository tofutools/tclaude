package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	SpawnHarnessAllow = "allow"
	SpawnHarnessDeny  = "deny"
)

// SpawnHarnessRule is one directed source→target edge in the operator's
// cross-harness spawn matrix. GroupID 0 is the global matrix; a positive ID is
// a group-local override. Missing global edges allow and missing group edges
// inherit.
type SpawnHarnessRule struct {
	GroupID       int64  `json:"-"`
	SourceHarness string `json:"source"`
	TargetHarness string `json:"target"`
	Decision      string `json:"decision"`
	Reason        string `json:"reason,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
}

func normalizeSpawnHarnessRule(rule SpawnHarnessRule) (SpawnHarnessRule, error) {
	rule.SourceHarness = strings.ToLower(strings.TrimSpace(rule.SourceHarness))
	rule.TargetHarness = strings.ToLower(strings.TrimSpace(rule.TargetHarness))
	rule.Decision = strings.ToLower(strings.TrimSpace(rule.Decision))
	rule.Reason = strings.TrimSpace(rule.Reason)
	if rule.SourceHarness == "" || rule.TargetHarness == "" {
		return rule, errors.New("source and target harness are required")
	}
	if rule.SourceHarness == rule.TargetHarness {
		return rule, errors.New("same-harness edges are always allowed and cannot be configured")
	}
	if rule.Decision != SpawnHarnessAllow && rule.Decision != SpawnHarnessDeny {
		return rule, fmt.Errorf("decision must be %q or %q", SpawnHarnessAllow, SpawnHarnessDeny)
	}
	if rule.Decision == SpawnHarnessDeny && rule.Reason == "" {
		return rule, errors.New("a deny rule requires a reason")
	}
	if rule.Decision == SpawnHarnessAllow {
		rule.Reason = ""
	}
	return rule, nil
}

// ReplaceSpawnHarnessRules atomically replaces one matrix scope. An empty
// slice clears the scope (global defaults to allow; group defaults to inherit).
func ReplaceSpawnHarnessRules(groupID int64, rules []SpawnHarnessRule) error {
	if groupID < 0 {
		return errors.New("group id must not be negative")
	}
	normalized := make([]SpawnHarnessRule, 0, len(rules))
	seen := make(map[string]struct{}, len(rules))
	for _, raw := range rules {
		rule, err := normalizeSpawnHarnessRule(raw)
		if err != nil {
			return err
		}
		key := rule.SourceHarness + "\x00" + rule.TargetHarness
		if _, ok := seen[key]; ok {
			return fmt.Errorf("duplicate spawn harness edge %s → %s", rule.SourceHarness, rule.TargetHarness)
		}
		seen[key] = struct{}{}
		rule.GroupID = groupID
		normalized = append(normalized, rule)
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
	if _, err := tx.Exec(`DELETE FROM spawn_harness_rules WHERE group_id = ?`, groupID); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, rule := range normalized {
		if _, err := tx.Exec(`INSERT INTO spawn_harness_rules
			(group_id, source_harness, target_harness, decision, reason, updated_at)
			VALUES (?, ?, ?, ?, ?, ?)`, groupID, rule.SourceHarness, rule.TargetHarness,
			rule.Decision, rule.Reason, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func ListSpawnHarnessRules(groupID int64) ([]SpawnHarnessRule, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT group_id, source_harness, target_harness, decision, reason, updated_at
		FROM spawn_harness_rules WHERE group_id = ? ORDER BY source_harness, target_harness`, groupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SpawnHarnessRule
	for rows.Next() {
		var rule SpawnHarnessRule
		if err := rows.Scan(&rule.GroupID, &rule.SourceHarness, &rule.TargetHarness,
			&rule.Decision, &rule.Reason, &rule.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, rule)
	}
	return out, rows.Err()
}

// ResolveSpawnHarnessRule returns the group override when one exists, then the
// global rule. found=false means the effective default is allow.
func ResolveSpawnHarnessRule(groupID int64, source, target string) (rule SpawnHarnessRule, scope string, found bool, err error) {
	source = strings.ToLower(strings.TrimSpace(source))
	target = strings.ToLower(strings.TrimSpace(target))
	if source == target {
		return SpawnHarnessRule{SourceHarness: source, TargetHarness: target, Decision: SpawnHarnessAllow}, "same-harness", true, nil
	}
	d, err := Open()
	if err != nil {
		return rule, "", false, err
	}
	ids := []int64{0}
	if groupID > 0 {
		ids = []int64{groupID, 0}
	}
	for _, id := range ids {
		err = d.QueryRow(`SELECT group_id, source_harness, target_harness, decision, reason, updated_at
			FROM spawn_harness_rules WHERE group_id = ? AND source_harness = ? AND target_harness = ?`,
			id, source, target).Scan(&rule.GroupID, &rule.SourceHarness, &rule.TargetHarness,
			&rule.Decision, &rule.Reason, &rule.UpdatedAt)
		if err == nil {
			if id == 0 {
				return rule, "global", true, nil
			}
			return rule, "group", true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return rule, "", false, err
		}
	}
	return SpawnHarnessRule{SourceHarness: source, TargetHarness: target, Decision: SpawnHarnessAllow}, "default", false, nil
}
