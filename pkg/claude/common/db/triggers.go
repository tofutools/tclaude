package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	TriggerScopeGlobal    = "global"
	TriggerScopeGroup     = "group"
	TriggerSourcePROpened = "pr.opened"

	TriggerDraftInclude = "include"
	TriggerDraftExclude = "exclude"
	TriggerDraftOnly    = "only"

	TriggerActionSpawn   = "spawn"
	TriggerActionMessage = "message"

	TriggerEventPending     = "pending"
	TriggerEventProcessed   = "processed"
	TriggerEventPreexisting = "preexisting"
	TriggerEventInterrupted = "interrupted"
)

const (
	TriggerNameMaxLen            = 80
	TriggerTemplateMaxLen        = 16 * 1024
	TriggerMaxActions            = 16
	TriggerMaxDelaySeconds int64 = 365 * 24 * 60 * 60
)

var (
	ErrTriggerInvalid         = errors.New("invalid trigger rule")
	ErrTriggerNameTaken       = errors.New("trigger rule name already exists")
	ErrTriggerVersionConflict = errors.New("trigger rule version conflict")
)

type TriggerAction struct {
	Type    string                `json:"type"`
	Spawn   *TriggerSpawnAction   `json:"spawn,omitempty"`
	Message *TriggerMessageAction `json:"message,omitempty"`
}

type TriggerSpawnAction struct {
	Profile               string   `json:"profile"`
	RoleRefs              []string `json:"roles,omitempty"`
	NameTemplate          string   `json:"name_template,omitempty"`
	InstructionTemplate   string   `json:"instruction_template"`
	MaxLiveWorkers        int      `json:"max_live_workers,omitempty"`
	WorkerDeadlineSeconds int64    `json:"worker_deadline_seconds,omitempty"`
}

type TriggerMessageAction struct {
	Target          string `json:"target,omitempty"` // pr.author_agent or group
	SubjectTemplate string `json:"subject_template,omitempty"`
	BodyTemplate    string `json:"body_template"`
}

type TriggerRule struct {
	ID               int64           `json:"id"`
	Name             string          `json:"name"`
	RowVersion       int64           `json:"row_version"`
	Revision         int64           `json:"revision"`
	Enabled          bool            `json:"enabled"`
	OwnerAgent       string          `json:"owner_agent,omitempty"`
	OperatorAuthored bool            `json:"operator_authored"`
	ScopeKind        string          `json:"scope"`
	GroupID          int64           `json:"group_id,omitempty"`
	Source           string          `json:"source"`
	AuthorIsAgent    *bool           `json:"author_is_agent,omitempty"`
	DraftFilter      string          `json:"draft_filter"`
	DebounceSeconds  int64           `json:"debounce_seconds,omitempty"`
	CooldownSeconds  int64           `json:"cooldown_seconds,omitempty"`
	Actions          []TriggerAction `json:"actions"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

func (r *TriggerRule) Validate() error {
	r.Name = strings.TrimSpace(r.Name)
	r.OwnerAgent = strings.TrimSpace(r.OwnerAgent)
	r.ScopeKind = strings.TrimSpace(r.ScopeKind)
	r.Source = strings.TrimSpace(r.Source)
	r.DraftFilter = strings.TrimSpace(r.DraftFilter)
	if r.Name == "" || len(r.Name) > TriggerNameMaxLen {
		return fmt.Errorf("%w: name is required and must be at most %d characters", ErrTriggerInvalid, TriggerNameMaxLen)
	}
	if !r.OperatorAuthored && r.OwnerAgent == "" {
		return fmt.Errorf("%w: owning principal is required", ErrTriggerInvalid)
	}
	if r.OperatorAuthored && r.OwnerAgent != "" {
		return fmt.Errorf("%w: operator-owned rule cannot also name an agent owner", ErrTriggerInvalid)
	}
	if r.ScopeKind != TriggerScopeGlobal && r.ScopeKind != TriggerScopeGroup {
		return fmt.Errorf("%w: scope must be global or group", ErrTriggerInvalid)
	}
	if (r.ScopeKind == TriggerScopeGroup) != (r.GroupID > 0) {
		return fmt.Errorf("%w: group scope requires exactly one group", ErrTriggerInvalid)
	}
	if r.Source != TriggerSourcePROpened {
		return fmt.Errorf("%w: source must be %s", ErrTriggerInvalid, TriggerSourcePROpened)
	}
	if !slices.Contains([]string{TriggerDraftInclude, TriggerDraftExclude, TriggerDraftOnly}, r.DraftFilter) {
		return fmt.Errorf("%w: draft_filter must be include, exclude, or only", ErrTriggerInvalid)
	}
	if r.DebounceSeconds < 0 || r.DebounceSeconds > TriggerMaxDelaySeconds ||
		r.CooldownSeconds < 0 || r.CooldownSeconds > TriggerMaxDelaySeconds {
		return fmt.Errorf("%w: debounce and cooldown must be between 0 and %d seconds", ErrTriggerInvalid, TriggerMaxDelaySeconds)
	}
	if len(r.Actions) == 0 || len(r.Actions) > TriggerMaxActions {
		return fmt.Errorf("%w: actions must contain 1-%d entries", ErrTriggerInvalid, TriggerMaxActions)
	}
	for i := range r.Actions {
		if err := r.Actions[i].validate(); err != nil {
			return fmt.Errorf("%w: action %d: %v", ErrTriggerInvalid, i, err)
		}
	}
	return nil
}

func (a *TriggerAction) validate() error {
	a.Type = strings.TrimSpace(a.Type)
	switch a.Type {
	case TriggerActionSpawn:
		if a.Spawn == nil || a.Message != nil {
			return errors.New("spawn payload is required and message payload is forbidden")
		}
		a.Spawn.Profile = strings.TrimSpace(a.Spawn.Profile)
		if a.Spawn.Profile == "" {
			return errors.New("spawn profile is required")
		}
		if strings.TrimSpace(a.Spawn.InstructionTemplate) == "" || len(a.Spawn.InstructionTemplate) > TriggerTemplateMaxLen {
			return errors.New("instruction_template is required and too long")
		}
		if a.Spawn.MaxLiveWorkers <= 0 {
			return errors.New("max_live_workers must be positive")
		}
		if a.Spawn.WorkerDeadlineSeconds < 0 || a.Spawn.WorkerDeadlineSeconds > TriggerMaxDelaySeconds {
			return errors.New("worker_deadline_seconds is out of range")
		}
	case TriggerActionMessage:
		if a.Message == nil || a.Spawn != nil {
			return errors.New("message payload is required and spawn payload is forbidden")
		}
		a.Message.Target = strings.TrimSpace(a.Message.Target)
		if a.Message.Target == "" {
			a.Message.Target = "pr.author_agent"
		}
		if a.Message.Target != "pr.author_agent" && a.Message.Target != "group" {
			return errors.New("message target must be pr.author_agent or group")
		}
		if strings.TrimSpace(a.Message.BodyTemplate) == "" || len(a.Message.BodyTemplate) > TriggerTemplateMaxLen {
			return errors.New("body_template is required and too long")
		}
	default:
		return errors.New("type must be spawn or message")
	}
	return nil
}

type TriggerPREvent struct {
	ID            int64     `json:"id"`
	AgentPRID     int64     `json:"agent_pr_id"`
	EventRef      string    `json:"event_ref"`
	PRURL         string    `json:"pr_url"`
	PRNumber      int       `json:"pr_number"`
	PRBranch      string    `json:"pr_branch,omitempty"`
	PRAuthorAgent string    `json:"pr_author_agent"`
	Draft         bool      `json:"draft"`
	GroupIDs      []int64   `json:"group_ids"`
	OccurredAt    time.Time `json:"occurred_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	Status        string    `json:"status"`
	ProcessedAt   time.Time `json:"processed_at,omitempty"`
}

type TriggerFiring struct {
	ID           int64                  `json:"id"`
	RuleID       int64                  `json:"rule_id"`
	RuleRevision int64                  `json:"rule_revision"`
	EventID      int64                  `json:"event_id"`
	EventRef     string                 `json:"event_ref"`
	Outcome      string                 `json:"outcome"`
	Detail       string                 `json:"detail,omitempty"`
	StartedAt    time.Time              `json:"started_at"`
	FinishedAt   time.Time              `json:"finished_at,omitempty"`
	Actions      []TriggerActionOutcome `json:"actions"`
}

type TriggerActionOutcome struct {
	ID           int64     `json:"id"`
	FiringID     int64     `json:"firing_id"`
	ActionIndex  int       `json:"action_index"`
	ActionType   string    `json:"action_type"`
	Outcome      string    `json:"outcome"`
	Detail       string    `json:"detail,omitempty"`
	SpawnedAgent string    `json:"spawned_agent,omitempty"`
	MessageID    int64     `json:"message_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

type TriggerWorker struct {
	ID           int64     `json:"id"`
	RuleID       int64     `json:"rule_id"`
	FiringID     int64     `json:"firing_id"`
	ActionIndex  int       `json:"action_index"`
	AgentID      string    `json:"agent_id"`
	ConvID       string    `json:"conv_id,omitempty"`
	PendingLabel string    `json:"pending_label,omitempty"`
	State        string    `json:"state"`
	Detail       string    `json:"detail,omitempty"`
	DeadlineAt   time.Time `json:"deadline_at,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	CompletedAt  time.Time `json:"completed_at,omitempty"`
}

func InsertTriggerRule(r *TriggerRule) (int64, error) {
	if err := r.Validate(); err != nil {
		return 0, err
	}
	actions, _ := json.Marshal(r.Actions)
	d, err := Open()
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	var group any
	if r.GroupID > 0 {
		group = r.GroupID
	}
	res, err := d.Exec(`INSERT INTO trigger_rules
		(name, enabled, owner_agent, operator_authored, scope_kind, group_id, source,
		 author_is_agent, draft_filter, debounce_seconds, cooldown_seconds, actions_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Name, r.Enabled, r.OwnerAgent, r.OperatorAuthored, r.ScopeKind, group, r.Source,
		boolPtrToNull(r.AuthorIsAgent), r.DraftFilter, r.DebounceSeconds, r.CooldownSeconds,
		string(actions), dbTime(now), dbTime(now))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return 0, ErrTriggerNameTaken
		}
		return 0, err
	}
	return res.LastInsertId()
}

func UpdateTriggerRule(id, rowVersion int64, r *TriggerRule) error {
	if id <= 0 || rowVersion <= 0 {
		return ErrTriggerVersionConflict
	}
	if err := r.Validate(); err != nil {
		return err
	}
	actions, _ := json.Marshal(r.Actions)
	var group any
	if r.GroupID > 0 {
		group = r.GroupID
	}
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE trigger_rules SET name=?, enabled=?, owner_agent=?, operator_authored=?,
		scope_kind=?, group_id=?, source=?, author_is_agent=?, draft_filter=?, debounce_seconds=?,
		cooldown_seconds=?, actions_json=?, row_version=row_version+1, revision=revision+1, updated_at=?
		WHERE id=? AND row_version=?`, r.Name, r.Enabled, r.OwnerAgent, r.OperatorAuthored,
		r.ScopeKind, group, r.Source, boolPtrToNull(r.AuthorIsAgent), r.DraftFilter,
		r.DebounceSeconds, r.CooldownSeconds, string(actions), dbTime(time.Now().UTC()), id, rowVersion)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return ErrTriggerNameTaken
		}
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrTriggerVersionConflict
	}
	return nil
}

func DeleteTriggerRule(id, rowVersion int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`DELETE FROM trigger_rules WHERE id=? AND row_version=?`, id, rowVersion)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrTriggerVersionConflict
	}
	return nil
}

func SetTriggerRuleEnabled(id, rowVersion int64, enabled bool) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE trigger_rules SET enabled=?, row_version=row_version+1, updated_at=? WHERE id=? AND row_version=?`, enabled, dbTime(time.Now().UTC()), id, rowVersion)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrTriggerVersionConflict
	}
	return nil
}

func GetTriggerRule(id int64) (*TriggerRule, error) { return getTriggerRule(`WHERE id=?`, id) }
func GetTriggerRuleByName(name string) (*TriggerRule, error) {
	return getTriggerRule(`WHERE name=?`, strings.TrimSpace(name))
}

func getTriggerRule(where string, arg any) (*TriggerRule, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(triggerRuleSelect+" "+where, arg)
	r, err := scanTriggerRule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func ListTriggerRules() ([]*TriggerRule, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(triggerRuleSelect + ` ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*TriggerRule
	for rows.Next() {
		r, err := scanTriggerRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const triggerRuleSelect = `SELECT id, name, row_version, revision, enabled, owner_agent,
	operator_authored, scope_kind, COALESCE(group_id,0), source, author_is_agent,
	draft_filter, debounce_seconds, cooldown_seconds, actions_json, created_at, updated_at FROM trigger_rules`

type triggerRuleScanner interface{ Scan(...any) error }

func scanTriggerRule(s triggerRuleScanner) (*TriggerRule, error) {
	var r TriggerRule
	var author sql.NullInt64
	var actions string
	var created, updated dbTimestamp
	err := s.Scan(&r.ID, &r.Name, &r.RowVersion, &r.Revision, &r.Enabled, &r.OwnerAgent, &r.OperatorAuthored,
		&r.ScopeKind, &r.GroupID, &r.Source, &author, &r.DraftFilter, &r.DebounceSeconds, &r.CooldownSeconds,
		&actions, &created, &updated)
	if err != nil {
		return nil, err
	}
	if author.Valid {
		b := author.Int64 != 0
		r.AuthorIsAgent = &b
	}
	if err := json.Unmarshal([]byte(actions), &r.Actions); err != nil {
		return nil, fmt.Errorf("decode trigger %d actions: %w", r.ID, err)
	}
	r.CreatedAt = created.Time()
	r.UpdatedAt = updated.Time()
	return &r, nil
}

func derivePRNumber(raw string) int {
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 || parts[len(parts)-2] != "pull" {
		return 0
	}
	var n int
	_, _ = fmt.Sscanf(parts[len(parts)-1], "%d", &n)
	return n
}

func enqueueTriggerPREventTx(tx *sql.Tx, pr AgentPR, branch string, draft bool, now time.Time) error {
	var groups string
	if err := tx.QueryRow(`SELECT COALESCE(json_group_array(group_id),'[]') FROM
		(SELECT group_id FROM agent_group_members WHERE agent_id=? ORDER BY group_id)`, pr.AgentID).Scan(&groups); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO trigger_pr_events
		(agent_pr_id,event_ref,pr_url,pr_number,pr_branch,pr_author_agent,draft,group_ids_json,occurred_at,updated_at,status)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(agent_pr_id) DO UPDATE SET
			updated_at=excluded.updated_at,
			pr_branch=CASE WHEN trigger_pr_events.status='pending' AND excluded.pr_branch<>'' THEN excluded.pr_branch ELSE trigger_pr_events.pr_branch END,
			draft=CASE WHEN trigger_pr_events.status='pending' THEN excluded.draft ELSE trigger_pr_events.draft END,
			group_ids_json=CASE WHEN trigger_pr_events.status='pending' THEN excluded.group_ids_json ELSE trigger_pr_events.group_ids_json END`,
		pr.ID, "pr.opened:"+pr.AgentID+":"+pr.PRURL, pr.PRURL, derivePRNumber(pr.PRURL), strings.TrimSpace(branch), pr.AgentID, draft, groups, dbTime(pr.CreatedAt), dbTime(now), TriggerEventPending)
	return err
}

func ReconcileTriggerPREvents() (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`INSERT OR IGNORE INTO trigger_pr_events
		(agent_pr_id,event_ref,pr_url,pr_number,pr_author_agent,draft,group_ids_json,occurred_at,updated_at,status)
		SELECT p.id,'pr.opened:'||p.agent_id||':'||p.pr_url,p.pr_url,0,p.agent_id,
		CASE WHEN lower(trim(p.state))='draft' THEN 1 ELSE 0 END,
		COALESCE((SELECT json_group_array(m.group_id) FROM agent_group_members m WHERE m.agent_id=p.agent_id),'[]'),
		p.created_at,p.updated_at,'pending' FROM agent_prs p`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func ListPendingTriggerPREvents(limit int) ([]TriggerPREvent, error) {
	if limit <= 0 {
		limit = 100
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id,agent_pr_id,event_ref,pr_url,pr_number,pr_branch,pr_author_agent,draft,
		group_ids_json,occurred_at,updated_at,status,processed_at FROM trigger_pr_events
		WHERE status IN ('pending','interrupted') AND processed_at IS NULL ORDER BY occurred_at,id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TriggerPREvent
	for rows.Next() {
		var e TriggerPREvent
		var groups string
		var occurred, updated dbTimestamp
		var processed sql.NullInt64
		if err := rows.Scan(&e.ID, &e.AgentPRID, &e.EventRef, &e.PRURL, &e.PRNumber, &e.PRBranch, &e.PRAuthorAgent, &e.Draft, &groups, &occurred, &updated, &e.Status, &processed); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(groups), &e.GroupIDs)
		e.OccurredAt = occurred.Time()
		e.UpdatedAt = updated.Time()
		if e.PRNumber == 0 {
			e.PRNumber = derivePRNumber(e.PRURL)
		}
		if processed.Valid {
			e.ProcessedAt = time.Unix(0, processed.Int64).UTC()
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func MarkTriggerPREventProcessed(id int64, now time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE trigger_pr_events SET status=CASE WHEN status='interrupted' THEN status ELSE 'processed' END,processed_at=? WHERE id=? AND status IN ('pending','interrupted') AND processed_at IS NULL`, dbTime(now.UTC()), id)
	return err
}

func MarkTriggerPREventInterrupted(id int64, now time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE trigger_pr_events SET status='interrupted',processed_at=NULL WHERE id=? AND status='pending'`, id)
	return err
}

// InterruptRunningTriggerFirings closes crash-left firing rows without replaying
// their side effects, then moves their source events to an equally explicit
// terminal state. A duplicate automated spawn is riskier than an operator-
// visible missed firing, so restart recovery is evidence-first and no-replay.
func InterruptRunningTriggerFirings(now time.Time) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var ids []int64
	rows, err := tx.Query(`SELECT event_id FROM trigger_firings WHERE outcome='running' AND finished_at IS NULL`)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE trigger_firings SET outcome='interrupted',detail='daemon stopped before firing completed',finished_at=? WHERE outcome='running' AND finished_at IS NULL`, dbTime(now.UTC())); err != nil {
		return 0, err
	}
	for _, eventID := range ids {
		if _, err := tx.Exec(`UPDATE trigger_pr_events SET status='interrupted',processed_at=NULL WHERE id=? AND status='pending'`, eventID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int64(len(ids)), nil
}

func InsertTriggerFiring(ruleID, revision, eventID int64, eventRef, outcome, detail string, now time.Time) (int64, bool, error) {
	d, err := Open()
	if err != nil {
		return 0, false, err
	}
	res, err := d.Exec(`INSERT INTO trigger_firings(rule_id,rule_revision,event_id,event_ref,outcome,detail,started_at)
		VALUES(?,?,?,?,?,?,?) ON CONFLICT(rule_id,rule_revision,event_id) DO NOTHING`, ruleID, revision, eventID, eventRef, outcome, detail, dbTime(now.UTC()))
	if err != nil {
		return 0, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, false, err
	}
	if n == 0 {
		return 0, false, nil
	}
	id, err := res.LastInsertId()
	return id, true, err
}

func FinishTriggerFiring(id int64, outcome, detail string, now time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE trigger_firings SET outcome=?,detail=?,finished_at=? WHERE id=?`, outcome, detail, dbTime(now.UTC()), id)
	return err
}

func InsertTriggerActionOutcome(o *TriggerActionOutcome) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT OR IGNORE INTO trigger_action_outcomes(firing_id,action_index,action_type,outcome,detail,spawned_agent,message_id,created_at) VALUES(?,?,?,?,?,?,NULLIF(?,0),?)`, o.FiringID, o.ActionIndex, o.ActionType, o.Outcome, o.Detail, o.SpawnedAgent, o.MessageID, dbTime(o.CreatedAt.UTC()))
	return err
}

func LatestCompletedTriggerFiring(ruleID int64) (*TriggerFiring, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var f TriggerFiring
	var started dbTimestamp
	var finished sql.NullInt64
	err = d.QueryRow(`SELECT id,COALESCE(rule_id,0),rule_revision,event_id,event_ref,outcome,detail,started_at,finished_at FROM trigger_firings WHERE rule_id=? AND finished_at IS NOT NULL AND outcome IN ('ok','partial_failure') ORDER BY finished_at DESC,id DESC LIMIT 1`, ruleID).Scan(&f.ID, &f.RuleID, &f.RuleRevision, &f.EventID, &f.EventRef, &f.Outcome, &f.Detail, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f.StartedAt = started.Time()
	if finished.Valid {
		f.FinishedAt = time.Unix(0, finished.Int64).UTC()
	}
	return &f, nil
}

func ListTriggerFirings(ruleID int64, limit int) ([]TriggerFiring, error) {
	if limit <= 0 {
		limit = 20
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id,COALESCE(rule_id,0),rule_revision,event_id,event_ref,outcome,detail,started_at,finished_at FROM trigger_firings WHERE (?=0 OR rule_id=?) ORDER BY started_at DESC,id DESC LIMIT ?`, ruleID, ruleID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TriggerFiring
	for rows.Next() {
		var f TriggerFiring
		var st dbTimestamp
		var fin sql.NullInt64
		if err := rows.Scan(&f.ID, &f.RuleID, &f.RuleRevision, &f.EventID, &f.EventRef, &f.Outcome, &f.Detail, &st, &fin); err != nil {
			return nil, err
		}
		f.StartedAt = st.Time()
		if fin.Valid {
			f.FinishedAt = time.Unix(0, fin.Int64).UTC()
		}
		acts, err := ListTriggerActionOutcomes(f.ID)
		if err != nil {
			return nil, err
		}
		f.Actions = acts
		out = append(out, f)
	}
	return out, rows.Err()
}

func ListTriggerActionOutcomes(firingID int64) ([]TriggerActionOutcome, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id,firing_id,action_index,action_type,outcome,detail,spawned_agent,COALESCE(message_id,0),created_at FROM trigger_action_outcomes WHERE firing_id=? ORDER BY action_index`, firingID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TriggerActionOutcome
	for rows.Next() {
		var o TriggerActionOutcome
		var ts dbTimestamp
		if err := rows.Scan(&o.ID, &o.FiringID, &o.ActionIndex, &o.ActionType, &o.Outcome, &o.Detail, &o.SpawnedAgent, &o.MessageID, &ts); err != nil {
			return nil, err
		}
		o.CreatedAt = ts.Time()
		out = append(out, o)
	}
	return out, rows.Err()
}

func CountLiveTriggerWorkers(ruleID int64, actionIndex int) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	var n int
	err = d.QueryRow(`SELECT COUNT(*) FROM trigger_workers WHERE rule_id=? AND action_index=? AND state IN ('reserved','pending','live')`, ruleID, actionIndex).Scan(&n)
	return n, err
}
func InsertTriggerWorker(w *TriggerWorker) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	var deadline any
	if !w.DeadlineAt.IsZero() {
		deadline = dbTime(w.DeadlineAt.UTC())
	}
	res, err := d.Exec(`INSERT INTO trigger_workers(rule_id,firing_id,action_index,agent_id,conv_id,pending_label,state,deadline_at,created_at) VALUES(?,?,?,?,?,?,?,?,?)`, w.RuleID, w.FiringID, w.ActionIndex, w.AgentID, w.ConvID, w.PendingLabel, w.State, deadline, dbTime(w.CreatedAt.UTC()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func MarkTriggerWorkerDispatched(id int64, convID, pendingLabel string) error {
	state := "pending"
	if strings.TrimSpace(convID) != "" {
		state = "live"
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE trigger_workers SET conv_id=?,pending_label=?,state=? WHERE id=? AND state='reserved'`, strings.TrimSpace(convID), strings.TrimSpace(pendingLabel), state, id)
	return err
}
func RuleSpawnedAgent(ruleID int64, agentID string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	var n int
	err = d.QueryRow(`SELECT EXISTS(SELECT 1 FROM trigger_workers WHERE rule_id=? AND agent_id=?)`, ruleID, strings.TrimSpace(agentID)).Scan(&n)
	return n != 0, err
}

func ListActiveTriggerWorkers() ([]TriggerWorker, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id,COALESCE(rule_id,0),firing_id,action_index,agent_id,conv_id,pending_label,state,
		deadline_at,created_at,completed_at,detail FROM trigger_workers
		WHERE state IN ('reserved','pending','live') ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TriggerWorker
	for rows.Next() {
		var w TriggerWorker
		var deadline, completed sql.NullInt64
		var created dbTimestamp
		if err := rows.Scan(&w.ID, &w.RuleID, &w.FiringID, &w.ActionIndex, &w.AgentID, &w.ConvID, &w.PendingLabel, &w.State, &deadline, &created, &completed, &w.Detail); err != nil {
			return nil, err
		}
		w.CreatedAt = created.Time()
		if deadline.Valid {
			w.DeadlineAt = time.Unix(0, deadline.Int64).UTC()
		}
		if completed.Valid {
			w.CompletedAt = time.Unix(0, completed.Int64).UTC()
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func CompleteTriggerWorker(id int64, state, detail string, now time.Time) error {
	if state != "failed" && state != "exited" && state != "deadline_exceeded" {
		return errors.New("invalid trigger worker terminal state")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE trigger_workers SET state=?,detail=?,completed_at=? WHERE id=? AND state IN ('reserved','pending','live')`, state, detail, dbTime(now.UTC()), id)
	return err
}
