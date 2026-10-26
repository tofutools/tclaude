package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/usagerules"
)

// Usage limits and usage profiles (see migrate_v278_usage_rules.go and
// pkg/claude/common/usagerules). Rules are stored as JSON and validated by
// usagerules before they reach these functions.

// Usage profile assignment scopes.
const (
	UsageScopeGlobal = "global"
	UsageScopeGroup  = "group"
	UsageScopeAgent  = "agent"
)

// ErrUsageProfileNameTaken reports a duplicate usage profile name.
var ErrUsageProfileNameTaken = errors.New("a usage profile with that name already exists")

// UsageProfile is a named bundle of per-agent rules.
type UsageProfile struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Descr     string            `json:"descr"`
	Rules     []usagerules.Rule `json:"rules"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// UsageProfileAssignment applies a profile at one layer. ScopeID is "" for
// global, the decimal group id for group, and the stable agent id for agent.
type UsageProfileAssignment struct {
	Scope     string `json:"scope"`
	ScopeID   string `json:"scope_id"`
	ProfileID int64  `json:"profile_id"`
	// Source says how an agent-layer assignment was made, e.g. "spawn profile
	// reviewer"; "" for a direct assignment.
	Source string `json:"source,omitempty"`
}

// UsageLimit is one rule over an aggregate: the whole instance (global) or
// one group's members.
type UsageLimit struct {
	ID        int64           `json:"id"`
	Scope     string          `json:"scope"`
	GroupID   int64           `json:"group_id,omitempty"`
	Rule      usagerules.Rule `json:"rule"`
	Enabled   bool            `json:"enabled"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// UsageRuleFiring records one fired step occurrence.
type UsageRuleFiring struct {
	RuleKey   string    `json:"rule_key"`
	Subject   string    `json:"subject"`
	PeriodKey string    `json:"period_key"`
	StepKey   string    `json:"step_key"`
	Action    string    `json:"action"`
	RuleName  string    `json:"rule_name"`
	Value     float64   `json:"value"`
	Budget    float64   `json:"budget"`
	DryRun    bool      `json:"dry_run"`
	FiredAt   time.Time `json:"fired_at"`
}

func marshalRules(rules []usagerules.Rule) (string, error) {
	if rules == nil {
		rules = []usagerules.Rule{}
	}
	b, err := json.Marshal(rules)
	return string(b), err
}

func scanUsageProfile(s rowScanner) (*UsageProfile, error) {
	var p UsageProfile
	var rules string
	var created, updated dbTimestamp
	if err := s.Scan(&p.ID, &p.Name, &p.Descr, &rules, &created, &updated); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(rules), &p.Rules); err != nil {
		return nil, fmt.Errorf("usage profile %q: decode rules: %w", p.Name, err)
	}
	p.CreatedAt, p.UpdatedAt = created.time, updated.time
	return &p, nil
}

const usageProfileSelect = `SELECT id, name, descr, rules, created_at, updated_at FROM usage_profiles`

// ListUsageProfiles returns every usage profile ordered by name.
func ListUsageProfiles() ([]*UsageProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(usageProfileSelect + ` ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*UsageProfile
	for rows.Next() {
		p, err := scanUsageProfile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetUsageProfile returns the profile with id, or nil when absent.
func GetUsageProfile(id int64) (*UsageProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	p, err := scanUsageProfile(d.QueryRow(usageProfileSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// GetUsageProfileByName returns the profile named name (case-insensitive), or
// nil when absent.
func GetUsageProfileByName(name string) (*UsageProfile, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	p, err := scanUsageProfile(d.QueryRow(usageProfileSelect+` WHERE name = ?`, strings.TrimSpace(name)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// CreateUsageProfile inserts p and sets its ID and timestamps.
func CreateUsageProfile(p *UsageProfile) error {
	rules, err := marshalRules(p.Rules)
	if err != nil {
		return err
	}
	d, err := Open()
	if err != nil {
		return err
	}
	now := time.Now()
	res, err := d.Exec(`INSERT INTO usage_profiles (name, descr, rules, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		p.Name, p.Descr, rules, dbTime(now), dbTime(now))
	if isUniqueConstraintErr(err) {
		return ErrUsageProfileNameTaken
	}
	if err != nil {
		return err
	}
	p.ID, err = res.LastInsertId()
	p.CreatedAt, p.UpdatedAt = now, now
	return err
}

// UpdateUsageProfile replaces the name, description and rules of p.ID.
// It reports whether the profile existed.
func UpdateUsageProfile(p *UsageProfile) (bool, error) {
	rules, err := marshalRules(p.Rules)
	if err != nil {
		return false, err
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	now := time.Now()
	res, err := d.Exec(`UPDATE usage_profiles SET name = ?, descr = ?, rules = ?, updated_at = ? WHERE id = ?`,
		p.Name, p.Descr, rules, dbTime(now), p.ID)
	if isUniqueConstraintErr(err) {
		return false, ErrUsageProfileNameTaken
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	p.UpdatedAt = now
	return n > 0, err
}

// DeleteUsageProfile removes a profile and, by cascade, its assignments.
func DeleteUsageProfile(id int64) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`DELETE FROM usage_profiles WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ListUsageProfileAssignments returns every assignment.
func ListUsageProfileAssignments() ([]UsageProfileAssignment, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT scope, scope_id, profile_id, source FROM usage_profile_assignments ORDER BY scope, scope_id, profile_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []UsageProfileAssignment
	for rows.Next() {
		var a UsageProfileAssignment
		if err := rows.Scan(&a.Scope, &a.ScopeID, &a.ProfileID, &a.Source); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UsageGroupScopeID is a group's assignment scope id.
func UsageGroupScopeID(groupID int64) string { return strconv.FormatInt(groupID, 10) }

// SetUsageProfileAssignments replaces the set of profiles assigned at one
// layer. An empty set clears the layer, so the next layer out applies.
func SetUsageProfileAssignments(scope, scopeID string, profileIDs []int64, source string) error {
	switch scope {
	case UsageScopeGlobal:
		scopeID = ""
	case UsageScopeGroup, UsageScopeAgent:
		if strings.TrimSpace(scopeID) == "" {
			return fmt.Errorf("usage profile assignment: %s scope needs an id", scope)
		}
	default:
		return fmt.Errorf("usage profile assignment: unknown scope %q", scope)
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
	if _, err := tx.Exec(`DELETE FROM usage_profile_assignments WHERE scope = ? AND scope_id = ?`, scope, scopeID); err != nil {
		return err
	}
	now := dbTime(time.Now())
	for _, id := range profileIDs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO usage_profile_assignments (scope, scope_id, profile_id, source, created_at) VALUES (?, ?, ?, ?, ?)`,
			scope, scopeID, id, source, now); err != nil {
			return fmt.Errorf("assign usage profile %d: %w", id, err)
		}
	}
	return tx.Commit()
}

func scanUsageLimit(s rowScanner) (*UsageLimit, error) {
	var l UsageLimit
	var groupID sql.NullInt64
	var rule string
	var created, updated dbTimestamp
	if err := s.Scan(&l.ID, &l.Scope, &groupID, &rule, &l.Enabled, &created, &updated); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(rule), &l.Rule); err != nil {
		return nil, fmt.Errorf("usage limit %d: decode rule: %w", l.ID, err)
	}
	l.GroupID = groupID.Int64
	l.CreatedAt, l.UpdatedAt = created.time, updated.time
	return &l, nil
}

const usageLimitSelect = `SELECT id, scope, group_id, rule, enabled, created_at, updated_at FROM usage_limits`

// ListUsageLimits returns every limit, global first, then by group and id.
func ListUsageLimits() ([]*UsageLimit, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(usageLimitSelect + ` ORDER BY scope = 'group', group_id, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*UsageLimit
	for rows.Next() {
		l, err := scanUsageLimit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetUsageLimit returns the limit with id, or nil when absent.
func GetUsageLimit(id int64) (*UsageLimit, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	l, err := scanUsageLimit(d.QueryRow(usageLimitSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return l, err
}

func usageLimitGroup(l *UsageLimit) (any, error) {
	switch l.Scope {
	case UsageScopeGlobal:
		return nil, nil
	case UsageScopeGroup:
		if l.GroupID <= 0 {
			return nil, errors.New("usage limit: group scope needs a group")
		}
		return l.GroupID, nil
	default:
		return nil, fmt.Errorf("usage limit: unknown scope %q", l.Scope)
	}
}

// CreateUsageLimit inserts l and sets its ID and timestamps.
func CreateUsageLimit(l *UsageLimit) error {
	group, err := usageLimitGroup(l)
	if err != nil {
		return err
	}
	rule, err := json.Marshal(l.Rule)
	if err != nil {
		return err
	}
	d, err := Open()
	if err != nil {
		return err
	}
	now := time.Now()
	res, err := d.Exec(`INSERT INTO usage_limits (scope, group_id, rule, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)`,
		l.Scope, group, string(rule), l.Enabled, dbTime(now), dbTime(now))
	if err != nil {
		return err
	}
	l.ID, err = res.LastInsertId()
	l.CreatedAt, l.UpdatedAt = now, now
	return err
}

// UpdateUsageLimit replaces the rule and enabled flag of l.ID. The scope is
// fixed at creation. It reports whether the limit existed.
func UpdateUsageLimit(l *UsageLimit) (bool, error) {
	rule, err := json.Marshal(l.Rule)
	if err != nil {
		return false, err
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	now := time.Now()
	res, err := d.Exec(`UPDATE usage_limits SET rule = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		string(rule), l.Enabled, dbTime(now), l.ID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	l.UpdatedAt = now
	return n > 0, err
}

// DeleteUsageLimit removes a limit.
func DeleteUsageLimit(id int64) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`DELETE FROM usage_limits WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// InsertUsageRuleFiring records f once. It reports false when this occurrence
// was already recorded. A non-nil message is stored in the same transaction,
// so an operator notification is written exactly once with its firing.
func InsertUsageRuleFiring(f UsageRuleFiring, message *HumanMessage) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if f.FiredAt.IsZero() {
		f.FiredAt = time.Now()
	}
	res, err := tx.Exec(`INSERT OR IGNORE INTO usage_rule_firings
		(rule_key, subject, period_key, step_key, action, rule_name, value, budget, dry_run, fired_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.RuleKey, f.Subject, f.PeriodKey, f.StepKey, f.Action, f.RuleName, f.Value, f.Budget, f.DryRun, dbTime(f.FiredAt))
	if err != nil {
		return false, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return false, err
	}
	if message != nil {
		if _, err := insertHumanMessage(tx, message); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// ListUsageRuleFiringsSince returns firings recorded at or after since (all
// of them for a zero since), newest first.
func ListUsageRuleFiringsSince(since time.Time) ([]UsageRuleFiring, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	query := `SELECT rule_key, subject, period_key, step_key, action, rule_name, value, budget, dry_run, fired_at
		FROM usage_rule_firings`
	var args []any
	if !since.IsZero() {
		query += ` WHERE fired_at >= ?`
		args = append(args, dbTimeBoundary(since))
	}
	rows, err := d.Query(query+` ORDER BY fired_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []UsageRuleFiring
	for rows.Next() {
		var f UsageRuleFiring
		var at dbTimestamp
		if err := rows.Scan(&f.RuleKey, &f.Subject, &f.PeriodKey, &f.StepKey, &f.Action, &f.RuleName, &f.Value, &f.Budget, &f.DryRun, &at); err != nil {
			return nil, err
		}
		f.FiredAt = at.time
		out = append(out, f)
	}
	return out, rows.Err()
}

// UsageRuleFiringTimes returns when each step's base crossing fired for one
// (rule, subject, period) — the anchors for timer-based reminders. Repeat
// occurrences are left out.
func UsageRuleFiringTimes(ruleKey, subject, periodKey string) (map[string]time.Time, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT step_key, fired_at FROM usage_rule_firings
		WHERE rule_key = ? AND subject = ? AND period_key = ? AND instr(step_key, '+') = 0`,
		ruleKey, subject, periodKey)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]time.Time{}
	for rows.Next() {
		var key string
		var at dbTimestamp
		if err := rows.Scan(&key, &at); err != nil {
			return nil, err
		}
		out[key] = at.time
	}
	return out, rows.Err()
}

// PruneUsageRuleFirings drops firings older than before. A lifetime-period
// base crossing is kept: its span never rolls over, so dropping it would
// re-fire the step. Repeats can go: their occurrence numbers only grow, so a
// pruned one is never owed again.
func PruneUsageRuleFirings(before time.Time) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`DELETE FROM usage_rule_firings
		WHERE fired_at < ? AND (period_key NOT LIKE 'lifetime|%' OR instr(step_key, '+') > 0)`, dbTimeBoundary(before))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AgentIDsByConv maps every known conversation generation to its stable
// agent id, in one query.
func AgentIDsByConv() (map[string]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT conv_id, agent_id FROM agent_conversations`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var conv, agent string
		if err := rows.Scan(&conv, &agent); err != nil {
			return nil, err
		}
		out[conv] = agent
	}
	return out, rows.Err()
}

// ListUsageRuleFiringsForSubject returns one (rule, subject)'s most recent
// firings, newest first.
func ListUsageRuleFiringsForSubject(ruleKey, subject string) ([]UsageRuleFiring, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT rule_key, subject, period_key, step_key, action, rule_name, value, budget, dry_run, fired_at
		FROM usage_rule_firings WHERE rule_key = ? AND subject = ? ORDER BY fired_at DESC LIMIT 64`, ruleKey, subject)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []UsageRuleFiring
	for rows.Next() {
		var f UsageRuleFiring
		var at dbTimestamp
		if err := rows.Scan(&f.RuleKey, &f.Subject, &f.PeriodKey, &f.StepKey, &f.Action, &f.RuleName, &f.Value, &f.Budget, &f.DryRun, &at); err != nil {
			return nil, err
		}
		f.FiredAt = at.time
		out = append(out, f)
	}
	return out, rows.Err()
}
