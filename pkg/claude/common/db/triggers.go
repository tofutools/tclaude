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
	TriggerScopeGlobal              = "global"
	TriggerScopeGroup               = "group"
	TriggerSourcePROpened           = "pr.opened"
	TriggerSourcePRUpdated          = "pr.updated"
	TriggerSourcePRMerged           = "pr.merged"
	TriggerSourceCIFailed           = "ci.failed"
	TriggerSourceCISucceeded        = "ci.succeeded"
	TriggerSourceAgentIdle          = "agent.idle"
	TriggerSourceAgentAwaitingInput = "agent.awaiting_input"

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

func IsTriggerSource(source string) bool {
	return slices.Contains([]string{TriggerSourcePROpened, TriggerSourcePRUpdated, TriggerSourcePRMerged,
		TriggerSourceCIFailed, TriggerSourceCISucceeded, TriggerSourceAgentIdle,
		TriggerSourceAgentAwaitingInput}, strings.TrimSpace(source))
}

func IsTriggerStateSource(source string) bool {
	return slices.Contains([]string{TriggerSourceAgentIdle, TriggerSourceAgentAwaitingInput}, strings.TrimSpace(source))
}

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
	ForSeconds       int64           `json:"for_seconds,omitempty"`
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
	if !IsTriggerSource(r.Source) {
		return fmt.Errorf("%w: unsupported source %q", ErrTriggerInvalid, r.Source)
	}
	if !slices.Contains([]string{TriggerDraftInclude, TriggerDraftExclude, TriggerDraftOnly}, r.DraftFilter) {
		return fmt.Errorf("%w: draft_filter must be include, exclude, or only", ErrTriggerInvalid)
	}
	if r.DebounceSeconds < 0 || r.DebounceSeconds > TriggerMaxDelaySeconds ||
		r.CooldownSeconds < 0 || r.CooldownSeconds > TriggerMaxDelaySeconds ||
		r.ForSeconds < 0 || r.ForSeconds > TriggerMaxDelaySeconds {
		return fmt.Errorf("%w: debounce and cooldown must be between 0 and %d seconds", ErrTriggerInvalid, TriggerMaxDelaySeconds)
	}
	if IsTriggerStateSource(r.Source) && r.ForSeconds <= 0 {
		return fmt.Errorf("%w: agent state sources require for_seconds greater than zero", ErrTriggerInvalid)
	}
	if !IsTriggerStateSource(r.Source) && r.ForSeconds != 0 {
		return fmt.Errorf("%w: for_seconds is only valid for agent state sources", ErrTriggerInvalid)
	}
	if len(r.Actions) == 0 || len(r.Actions) > TriggerMaxActions {
		return fmt.Errorf("%w: actions must contain 1-%d entries", ErrTriggerInvalid, TriggerMaxActions)
	}
	for i := range r.Actions {
		if IsTriggerStateSource(r.Source) && r.Actions[i].Type == TriggerActionMessage &&
			r.Actions[i].Message != nil && strings.TrimSpace(r.Actions[i].Message.Target) == "" {
			r.Actions[i].Message.Target = "agent"
		}
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
		if a.Message.Target != "pr.author_agent" && a.Message.Target != "agent" && a.Message.Target != "group" {
			return errors.New("message target must be pr.author_agent, agent, or group")
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
	ID             int64     `json:"id"`
	AgentPRID      int64     `json:"agent_pr_id"`
	Source         string    `json:"source"`
	EventRef       string    `json:"event_ref"`
	PRURL          string    `json:"pr_url"`
	PRNumber       int       `json:"pr_number"`
	PRBranch       string    `json:"pr_branch,omitempty"`
	PRAuthorAgent  string    `json:"pr_author_agent"`
	AgentID        string    `json:"agent_id,omitempty"`
	AgentHarness   string    `json:"agent_harness,omitempty"`
	FactResult     string    `json:"fact_result,omitempty"`
	FactObservedAt time.Time `json:"fact_observed_at,omitempty"`
	DwellStartedAt time.Time `json:"dwell_started_at,omitempty"`
	Draft          bool      `json:"draft"`
	GroupIDs       []int64   `json:"group_ids"`
	PreviousState  string    `json:"previous_state,omitempty"`
	CurrentState   string    `json:"current_state,omitempty"`
	OccurredAt     time.Time `json:"occurred_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Status         string    `json:"status"`
	ProcessedAt    time.Time `json:"processed_at,omitempty"`
}

type TriggerDwellState struct {
	RuleID         int64     `json:"rule_id"`
	AgentID        string    `json:"agent_id"`
	RuleRevision   int64     `json:"rule_revision"`
	Episode        int64     `json:"episode"`
	Result         string    `json:"result"`
	Detail         string    `json:"detail,omitempty"`
	Harness        string    `json:"harness,omitempty"`
	FactObservedAt time.Time `json:"fact_observed_at,omitempty"`
	TrueSince      time.Time `json:"true_since,omitempty"`
	FiredAt        time.Time `json:"fired_at,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type TriggerFiring struct {
	ID             int64                  `json:"id"`
	RuleID         int64                  `json:"rule_id"`
	RuleRevision   int64                  `json:"rule_revision"`
	EventID        int64                  `json:"event_id"`
	EventRef       string                 `json:"event_ref"`
	Source         string                 `json:"source,omitempty"`
	PreviousState  string                 `json:"previous_state,omitempty"`
	CurrentState   string                 `json:"current_state,omitempty"`
	AgentID        string                 `json:"agent_id,omitempty"`
	AgentHarness   string                 `json:"agent_harness,omitempty"`
	FactResult     string                 `json:"fact_result,omitempty"`
	FactObservedAt time.Time              `json:"fact_observed_at,omitempty"`
	DwellStartedAt time.Time              `json:"dwell_started_at,omitempty"`
	Outcome        string                 `json:"outcome"`
	Detail         string                 `json:"detail,omitempty"`
	StartedAt      time.Time              `json:"started_at"`
	FinishedAt     time.Time              `json:"finished_at,omitempty"`
	Actions        []TriggerActionOutcome `json:"actions"`
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
	CronJobID    int64     `json:"cron_job_id,omitempty"`
	CronRunID    int64     `json:"cron_run_id,omitempty"`
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
		 author_is_agent, draft_filter, debounce_seconds, cooldown_seconds, for_seconds, actions_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Name, r.Enabled, r.OwnerAgent, r.OperatorAuthored, r.ScopeKind, group, r.Source,
		boolPtrToNull(r.AuthorIsAgent), r.DraftFilter, r.DebounceSeconds, r.CooldownSeconds, r.ForSeconds,
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
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var previousSource string
	if err := tx.QueryRow(`SELECT source FROM trigger_rules WHERE id=? AND row_version=?`, id, rowVersion).
		Scan(&previousSource); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrTriggerVersionConflict
		}
		return err
	}
	res, err := tx.Exec(`UPDATE trigger_rules SET name=?, enabled=?, owner_agent=?, operator_authored=?,
		scope_kind=?, group_id=?, source=?, author_is_agent=?, draft_filter=?, debounce_seconds=?,
		cooldown_seconds=?, for_seconds=?, actions_json=?, row_version=row_version+1, revision=revision+1, updated_at=?
		WHERE id=? AND row_version=?`, r.Name, r.Enabled, r.OwnerAgent, r.OperatorAuthored,
		r.ScopeKind, group, r.Source, boolPtrToNull(r.AuthorIsAgent), r.DraftFilter,
		r.DebounceSeconds, r.CooldownSeconds, r.ForSeconds, string(actions), dbTime(time.Now().UTC()), id, rowVersion)
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
	if previousSource != r.Source {
		if _, err := tx.Exec(`DELETE FROM trigger_dwell_states WHERE rule_id=?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
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

func HasEnabledTriggerSource(source string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	var found bool
	err = d.QueryRow(`SELECT EXISTS(SELECT 1 FROM trigger_rules WHERE enabled=1 AND source=?)`, strings.TrimSpace(source)).Scan(&found)
	return found, err
}

const triggerRuleSelect = `SELECT id, name, row_version, revision, enabled, owner_agent,
	operator_authored, scope_kind, COALESCE(group_id,0), source, author_is_agent,
	draft_filter, debounce_seconds, cooldown_seconds, for_seconds, actions_json, created_at, updated_at FROM trigger_rules`

type triggerRuleScanner interface{ Scan(...any) error }

func scanTriggerRule(s triggerRuleScanner) (*TriggerRule, error) {
	var r TriggerRule
	var author sql.NullInt64
	var actions string
	var created, updated dbTimestamp
	err := s.Scan(&r.ID, &r.Name, &r.RowVersion, &r.Revision, &r.Enabled, &r.OwnerAgent, &r.OperatorAuthored,
		&r.ScopeKind, &r.GroupID, &r.Source, &author, &r.DraftFilter, &r.DebounceSeconds, &r.CooldownSeconds,
		&r.ForSeconds, &actions, &created, &updated)
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

func triggerPRGroupsJSONTx(tx *sql.Tx, agentID string) (string, error) {
	var groups string
	err := tx.QueryRow(`SELECT COALESCE(json_group_array(group_id),'[]') FROM
		(SELECT group_id FROM agent_group_members WHERE agent_id=? ORDER BY group_id)`, agentID).Scan(&groups)
	return groups, err
}

func enqueueTriggerPREventTx(tx *sql.Tx, pr AgentPR, branch string, draft bool, now time.Time) error {
	groups, err := triggerPRGroupsJSONTx(tx, pr.AgentID)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO trigger_pr_events
		(agent_pr_id,source,event_ref,pr_url,pr_number,pr_branch,pr_author_agent,draft,group_ids_json,occurred_at,updated_at,status)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(event_ref) DO UPDATE SET
			updated_at=excluded.updated_at,
			pr_branch=CASE WHEN trigger_pr_events.status='pending' AND excluded.pr_branch<>'' THEN excluded.pr_branch ELSE trigger_pr_events.pr_branch END,
			draft=CASE WHEN trigger_pr_events.status='pending' THEN excluded.draft ELSE trigger_pr_events.draft END,
			group_ids_json=CASE WHEN trigger_pr_events.status='pending' THEN excluded.group_ids_json ELSE trigger_pr_events.group_ids_json END`,
		pr.ID, TriggerSourcePROpened, "pr.opened:"+pr.AgentID+":"+pr.PRURL, pr.PRURL, derivePRNumber(pr.PRURL), strings.TrimSpace(branch), pr.AgentID, draft, groups, dbTime(pr.CreatedAt), dbTime(now), TriggerEventPending)
	if err == nil {
		_, err = tx.Exec(`INSERT OR IGNORE INTO trigger_pr_observations(agent_pr_id,event_sequence,branch_context,updated_at) VALUES(?,1,?,?)`,
			pr.ID, strings.TrimSpace(branch), dbTime(now))
	}
	return err
}

func enqueueTriggerTransitionTx(tx *sql.Tx, pr AgentPR, source, previous, current, branch string, draft bool, now time.Time) error {
	groups, err := triggerPRGroupsJSONTx(tx, pr.AgentID)
	if err != nil {
		return err
	}
	branch = strings.TrimSpace(branch)
	if branch == "" {
		_ = tx.QueryRow(`SELECT branch_context FROM trigger_pr_observations WHERE agent_pr_id=?`, pr.ID).Scan(&branch)
	}
	if source == TriggerSourcePRUpdated {
		res, err := tx.Exec(`UPDATE trigger_pr_events SET pr_branch=?,draft=?,group_ids_json=?,
			previous_state=CASE WHEN previous_state='' THEN ? ELSE previous_state END,current_state=?,
			occurred_at=?,updated_at=? WHERE id=(SELECT id FROM trigger_pr_events
			WHERE agent_pr_id=? AND source='pr.updated' AND status='pending' AND processed_at IS NULL
			ORDER BY id DESC LIMIT 1)`, branch, draft, groups, strings.TrimSpace(previous),
			strings.TrimSpace(current), dbTime(now), dbTime(now), pr.ID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n > 0 {
			if err == nil && branch != "" {
				_, err = tx.Exec(`UPDATE trigger_pr_observations SET branch_context=?,updated_at=? WHERE agent_pr_id=?`,
					branch, dbTime(now), pr.ID)
			}
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO trigger_pr_observations(agent_pr_id,event_sequence,branch_context,updated_at)
		VALUES(?,1,?,?) ON CONFLICT(agent_pr_id) DO UPDATE SET event_sequence=event_sequence+1,
		branch_context=CASE WHEN excluded.branch_context<>'' THEN excluded.branch_context ELSE trigger_pr_observations.branch_context END,
		updated_at=excluded.updated_at`, pr.ID, branch, dbTime(now)); err != nil {
		return err
	}
	var sequence int64
	if err := tx.QueryRow(`SELECT event_sequence FROM trigger_pr_observations WHERE agent_pr_id=?`, pr.ID).Scan(&sequence); err != nil {
		return err
	}
	eventRef := fmt.Sprintf("%s:%d:%d", source, pr.ID, sequence)
	_, err = tx.Exec(`INSERT INTO trigger_pr_events
		(agent_pr_id,source,event_ref,pr_url,pr_number,pr_branch,pr_author_agent,draft,group_ids_json,
		 previous_state,current_state,occurred_at,updated_at,status)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, pr.ID, source, eventRef, pr.PRURL, derivePRNumber(pr.PRURL),
		branch, pr.AgentID, draft, groups, strings.TrimSpace(previous), strings.TrimSpace(current),
		dbTime(now), dbTime(now), TriggerEventPending)
	return err
}

func ReconcileTriggerPREvents() (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`INSERT OR IGNORE INTO trigger_pr_events
		(agent_pr_id,source,event_ref,pr_url,pr_number,pr_author_agent,draft,group_ids_json,occurred_at,updated_at,status)
		SELECT p.id,'pr.opened','pr.opened:'||p.agent_id||':'||p.pr_url,p.pr_url,0,p.agent_id,
		CASE WHEN lower(trim(p.state))='draft' THEN 1 ELSE 0 END,
		COALESCE((SELECT json_group_array(m.group_id) FROM agent_group_members m WHERE m.agent_id=p.agent_id),'[]'),
		p.created_at,p.updated_at,'pending' FROM agent_prs p`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListTriggerCIWatchPRs returns the bounded set of non-terminal presented PRs
// that are in scope for at least one enabled CI transition rule.
func ListTriggerCIWatchPRs(limit int) ([]AgentPR, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	query := `SELECT DISTINCT p.id,p.agent_id,p.pr_url,p.summary,p.state,p.created_at,p.updated_at
		FROM agent_prs p JOIN trigger_rules r ON r.enabled=1 AND r.source IN ('ci.failed','ci.succeeded')
		LEFT JOIN trigger_pr_observations o ON o.agent_pr_id=p.id
		WHERE lower(trim(p.state)) NOT IN ('handled','merged','closed')
		  AND (r.scope_kind='global' OR EXISTS (SELECT 1 FROM agent_group_members m
		      WHERE m.agent_id=p.agent_id AND m.group_id=r.group_id))
		ORDER BY COALESCE(o.ci_polled_at,0),p.id`
	var rows *sql.Rows
	if limit > 0 {
		rows, err = d.Query(query+` LIMIT ?`, limit)
	} else {
		rows, err = d.Query(query)
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []AgentPR
	for rows.Next() {
		var p AgentPR
		var created, updated dbTimestamp
		if err := rows.Scan(&p.ID, &p.AgentID, &p.PRURL, &p.Summary, &p.State, &created, &updated); err != nil {
			return nil, err
		}
		p.CreatedAt, p.UpdatedAt = created.Time(), updated.Time()
		out = append(out, p)
	}
	return out, rows.Err()
}

// MarkTriggerCIPollAttempt advances only the scheduler fairness clock. It is
// deliberately separate from ci_observed_at: a failed resolver is unknown and
// must not make an old check summary fresh.
func MarkTriggerCIPollAttempt(agentPRID int64, attemptedAt time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO trigger_pr_observations(agent_pr_id,event_sequence,ci_polled_at,updated_at)
		VALUES(?,1,?,?) ON CONFLICT(agent_pr_id) DO UPDATE SET ci_polled_at=excluded.ci_polled_at`,
		agentPRID, dbTime(attemptedAt.UTC()), dbTime(attemptedAt.UTC()))
	return err
}

// BaselineTriggerPRCI records a fresh watched-state baseline without emitting
// an event. It is used when a PR identity enters the enabled watcher set after
// a gap, so transitions that happened while unwatched are never replayed.
func BaselineTriggerPRCI(agentPRID int64, state string, observedAt time.Time) error {
	state = strings.ToLower(strings.TrimSpace(state))
	if !slices.Contains([]string{"passing", "failing", "pending", "none"}, state) {
		return fmt.Errorf("%w: unsupported CI state %q", ErrTriggerInvalid, state)
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO trigger_pr_observations
		(agent_pr_id,event_sequence,ci_state,ci_observed_at,ci_polled_at,updated_at)
		VALUES(?,1,?,?,?,?) ON CONFLICT(agent_pr_id) DO UPDATE SET ci_state=excluded.ci_state,
		ci_observed_at=excluded.ci_observed_at,ci_polled_at=excluded.ci_polled_at,updated_at=excluded.updated_at`,
		agentPRID, state, dbTime(observedAt.UTC()), dbTime(observedAt.UTC()), dbTime(observedAt.UTC()))
	return err
}

// ObserveTriggerPRCI durably advances the last fresh aggregate CI state for a
// presented PR. The first observation establishes a baseline; later changes
// to passing/failing emit exactly one transition event. Older observations are
// ignored, so a slow poll cannot regress the durable edge detector.
func ObserveTriggerPRCI(agentPRID int64, state string, observedAt time.Time) (bool, error) {
	state = strings.ToLower(strings.TrimSpace(state))
	if !slices.Contains([]string{"passing", "failing", "pending", "none"}, state) {
		return false, fmt.Errorf("%w: unsupported CI state %q", ErrTriggerInvalid, state)
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var pr AgentPR
	var created, updated dbTimestamp
	if err := tx.QueryRow(`SELECT id,agent_id,pr_url,summary,state,created_at,updated_at FROM agent_prs WHERE id=?`, agentPRID).
		Scan(&pr.ID, &pr.AgentID, &pr.PRURL, &pr.Summary, &pr.State, &created, &updated); err != nil {
		return false, err
	}
	pr.CreatedAt, pr.UpdatedAt = created.Time(), updated.Time()
	var previous string
	var priorAt sql.NullInt64
	err = tx.QueryRow(`SELECT ci_state,ci_observed_at FROM trigger_pr_observations WHERE agent_pr_id=?`, agentPRID).Scan(&previous, &priorAt)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.Exec(`INSERT INTO trigger_pr_observations(agent_pr_id,event_sequence,ci_state,ci_observed_at,ci_polled_at,updated_at)
			VALUES(?,1,?,?,?,?)`, agentPRID, state, dbTime(observedAt.UTC()), dbTime(observedAt.UTC()), dbTime(observedAt.UTC()))
		if err == nil {
			err = tx.Commit()
		}
		return false, err
	}
	if err != nil {
		return false, err
	}
	if priorAt.Valid && observedAt.Before(time.Unix(0, priorAt.Int64)) {
		return false, nil
	}
	if _, err := tx.Exec(`UPDATE trigger_pr_observations SET ci_state=?,ci_observed_at=?,ci_polled_at=?,updated_at=? WHERE agent_pr_id=?`,
		state, dbTime(observedAt.UTC()), dbTime(observedAt.UTC()), dbTime(observedAt.UTC()), agentPRID); err != nil {
		return false, err
	}
	emitted := previous != "" && previous != state && (state == "passing" || state == "failing")
	if emitted {
		source := TriggerSourceCISucceeded
		if state == "failing" {
			source = TriggerSourceCIFailed
		}
		if err := enqueueTriggerTransitionTx(tx, pr, source, previous, state, "", strings.EqualFold(pr.State, "draft"), observedAt.UTC()); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return emitted, nil
}

func GetTriggerDwellState(ruleID int64, agentID string) (*TriggerDwellState, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`SELECT rule_id,agent_id,rule_revision,episode,result,detail,harness,
		fact_observed_at,true_since,fired_at,updated_at FROM trigger_dwell_states WHERE rule_id=? AND agent_id=?`,
		ruleID, strings.TrimSpace(agentID))
	state, err := scanTriggerDwellState(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return state, err
}

func ListTriggerDwellStates(ruleID int64) ([]TriggerDwellState, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT rule_id,agent_id,rule_revision,episode,result,detail,harness,
		fact_observed_at,true_since,fired_at,updated_at FROM trigger_dwell_states
		WHERE (?=0 OR rule_id=?) ORDER BY agent_id`, ruleID, ruleID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TriggerDwellState
	for rows.Next() {
		state, err := scanTriggerDwellState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *state)
	}
	return out, rows.Err()
}

func scanTriggerDwellState(row rowScanner) (*TriggerDwellState, error) {
	var state TriggerDwellState
	var observed, since, fired sql.NullInt64
	var updated dbTimestamp
	if err := row.Scan(&state.RuleID, &state.AgentID, &state.RuleRevision, &state.Episode,
		&state.Result, &state.Detail, &state.Harness, &observed, &since, &fired, &updated); err != nil {
		return nil, err
	}
	if observed.Valid {
		state.FactObservedAt = time.Unix(0, observed.Int64).UTC()
	}
	if since.Valid {
		state.TrueSince = time.Unix(0, since.Int64).UTC()
	}
	if fired.Valid {
		state.FiredAt = time.Unix(0, fired.Int64).UTC()
	}
	state.UpdatedAt = updated.Time()
	return &state, nil
}

// ApplyTriggerDwellState atomically persists one planned fact episode and, if
// it matured, enqueues its immutable event. The rule revision/enabled check
// prevents a cached scheduler plan from surviving a concurrent edit/disable.
func ApplyTriggerDwellState(rule *TriggerRule, state TriggerDwellState, detail, harness string,
	factObservedAt, now time.Time, fire bool) (bool, error) {
	if rule == nil || rule.ID <= 0 || !IsTriggerStateSource(rule.Source) {
		return false, ErrTriggerInvalid
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var source string
	var enabled bool
	var revision int64
	if err := tx.QueryRow(`SELECT source,enabled,revision FROM trigger_rules WHERE id=?`, rule.ID).
		Scan(&source, &enabled, &revision); err != nil {
		return false, err
	}
	if !enabled || revision != rule.Revision || source != rule.Source {
		return false, nil
	}
	_, err = tx.Exec(`INSERT INTO trigger_dwell_states
		(rule_id,agent_id,rule_revision,episode,result,detail,harness,fact_observed_at,true_since,fired_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(rule_id,agent_id) DO UPDATE SET
		rule_revision=excluded.rule_revision,episode=excluded.episode,result=excluded.result,
		detail=excluded.detail,harness=excluded.harness,fact_observed_at=excluded.fact_observed_at,
		true_since=excluded.true_since,fired_at=excluded.fired_at,updated_at=excluded.updated_at`,
		rule.ID, state.AgentID, state.RuleRevision, state.Episode, state.Result, detail, harness,
		nullableDBTime(factObservedAt), nullableDBTime(state.TrueSince), nullableDBTime(state.FiredAt), dbTime(now.UTC()))
	if err != nil {
		return false, err
	}
	if fire {
		groups, err := triggerPRGroupsJSONTx(tx, state.AgentID)
		if err != nil {
			return false, err
		}
		eventRef := fmt.Sprintf("%s:%d:%d:%s:%d", source, rule.ID, rule.Revision, state.AgentID, state.Episode)
		res, err := tx.Exec(`INSERT INTO trigger_pr_events
			(source,event_ref,agent_id,agent_harness,fact_result,fact_observed_at,dwell_started_at,
			 group_ids_json,occurred_at,updated_at,status)
			VALUES(?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(event_ref) DO NOTHING`, source, eventRef,
			state.AgentID, strings.TrimSpace(harness), state.Result, nullableDBTime(factObservedAt),
			nullableDBTime(state.TrueSince), groups, dbTime(now.UTC()), dbTime(now.UTC()), TriggerEventPending)
		if err != nil {
			return false, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return false, err
		}
		fire = n == 1
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return fire, nil
}

func ListPendingTriggerPREvents(limit int) ([]TriggerPREvent, error) {
	if limit <= 0 {
		limit = 100
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id,agent_pr_id,source,event_ref,pr_url,pr_number,pr_branch,pr_author_agent,
		agent_id,agent_harness,fact_result,fact_observed_at,dwell_started_at,draft,
		group_ids_json,previous_state,current_state,occurred_at,updated_at,status,processed_at FROM trigger_pr_events
		WHERE status IN ('pending','interrupted') AND processed_at IS NULL ORDER BY occurred_at,id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TriggerPREvent
	for rows.Next() {
		var e TriggerPREvent
		var agentPRID sql.NullInt64
		var groups string
		var occurred, updated dbTimestamp
		var observed, dwell, processed sql.NullInt64
		if err := rows.Scan(&e.ID, &agentPRID, &e.Source, &e.EventRef, &e.PRURL, &e.PRNumber, &e.PRBranch, &e.PRAuthorAgent,
			&e.AgentID, &e.AgentHarness, &e.FactResult, &observed, &dwell, &e.Draft, &groups,
			&e.PreviousState, &e.CurrentState, &occurred, &updated, &e.Status, &processed); err != nil {
			return nil, err
		}
		if agentPRID.Valid {
			e.AgentPRID = agentPRID.Int64
		}
		_ = json.Unmarshal([]byte(groups), &e.GroupIDs)
		e.OccurredAt = occurred.Time()
		e.UpdatedAt = updated.Time()
		if observed.Valid {
			e.FactObservedAt = time.Unix(0, observed.Int64).UTC()
		}
		if dwell.Valid {
			e.DwellStartedAt = time.Unix(0, dwell.Int64).UTC()
		}
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
	rows, err := d.Query(`SELECT f.id,COALESCE(f.rule_id,0),f.rule_revision,f.event_id,f.event_ref,
		COALESCE(e.source,''),COALESCE(e.previous_state,''),COALESCE(e.current_state,''),
		COALESCE(e.agent_id,''),COALESCE(e.agent_harness,''),COALESCE(e.fact_result,''),e.fact_observed_at,e.dwell_started_at,
		f.outcome,f.detail,f.started_at,f.finished_at FROM trigger_firings f
		LEFT JOIN trigger_pr_events e ON e.id=f.event_id
		WHERE (?=0 OR f.rule_id=?) ORDER BY f.started_at DESC,f.id DESC LIMIT ?`, ruleID, ruleID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []TriggerFiring
	for rows.Next() {
		var f TriggerFiring
		var st dbTimestamp
		var observed, dwell, fin sql.NullInt64
		if err := rows.Scan(&f.ID, &f.RuleID, &f.RuleRevision, &f.EventID, &f.EventRef, &f.Source,
			&f.PreviousState, &f.CurrentState, &f.AgentID, &f.AgentHarness, &f.FactResult, &observed, &dwell,
			&f.Outcome, &f.Detail, &st, &fin); err != nil {
			return nil, err
		}
		if observed.Valid {
			f.FactObservedAt = time.Unix(0, observed.Int64).UTC()
		}
		if dwell.Valid {
			f.DwellStartedAt = time.Unix(0, dwell.Int64).UTC()
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
	return CountLiveManagedWorkers(ruleID, 0, actionIndex)
}

// CountLiveManagedWorkers counts durable reservations as live capacity. Exactly
// one of ruleID or cronJobID identifies the worker source.
func CountLiveManagedWorkers(ruleID, cronJobID int64, actionIndex int) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	var n int
	if cronJobID > 0 {
		err = d.QueryRow(`SELECT COUNT(*) FROM trigger_workers WHERE cron_job_id=? AND action_index=? AND state IN ('reserved','pending','live')`, cronJobID, actionIndex).Scan(&n)
	} else {
		err = d.QueryRow(`SELECT COUNT(*) FROM trigger_workers WHERE rule_id=? AND action_index=? AND state IN ('reserved','pending','live')`, ruleID, actionIndex).Scan(&n)
	}
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
	var ruleID, firingID, cronJobID, cronRunID any
	if w.RuleID > 0 {
		ruleID = w.RuleID
	}
	if w.FiringID > 0 {
		firingID = w.FiringID
	}
	if w.CronJobID > 0 {
		cronJobID = w.CronJobID
	}
	if w.CronRunID > 0 {
		cronRunID = w.CronRunID
	}
	res, err := d.Exec(`INSERT INTO trigger_workers(rule_id,firing_id,cron_job_id,cron_run_id,action_index,agent_id,conv_id,pending_label,state,deadline_at,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, ruleID, firingID, cronJobID, cronRunID, w.ActionIndex, w.AgentID, w.ConvID, w.PendingLabel, w.State, deadline, dbTime(w.CreatedAt.UTC()))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func MarkTriggerWorkerDispatched(id int64, convID, pendingLabel string) (bool, error) {
	state := "pending"
	if strings.TrimSpace(convID) != "" {
		state = "live"
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE trigger_workers SET conv_id=?,pending_label=?,state=? WHERE id=? AND state='reserved'`, strings.TrimSpace(convID), strings.TrimSpace(pendingLabel), state, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
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
	rows, err := d.Query(`SELECT id,COALESCE(rule_id,0),COALESCE(firing_id,0),COALESCE(cron_job_id,0),COALESCE(cron_run_id,0),action_index,agent_id,conv_id,pending_label,state,
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
		if err := rows.Scan(&w.ID, &w.RuleID, &w.FiringID, &w.CronJobID, &w.CronRunID, &w.ActionIndex, &w.AgentID, &w.ConvID, &w.PendingLabel, &w.State, &deadline, &created, &completed, &w.Detail); err != nil {
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

func ListActiveCronWorkers(jobID int64) ([]TriggerWorker, error) {
	all, err := ListActiveTriggerWorkers()
	if err != nil {
		return nil, err
	}
	out := make([]TriggerWorker, 0, len(all))
	for _, w := range all {
		if w.CronJobID == jobID {
			out = append(out, w)
		}
	}
	return out, nil
}

func ManagedWorkerIDForAgent(agentID string) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	var id int64
	err = d.QueryRow(`SELECT id FROM trigger_workers WHERE agent_id=?`, strings.TrimSpace(agentID)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

func CompleteTriggerWorker(id int64, state, detail string, now time.Time) error {
	if state != "failed" && state != "exited" && state != "deadline_exceeded" && state != "replaced" && state != "interrupted" {
		return errors.New("invalid trigger worker terminal state")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE trigger_workers SET state=?,detail=?,completed_at=? WHERE id=? AND state IN ('reserved','pending','live')`, state, detail, dbTime(now.UTC()), id)
	return err
}

// InterruptOrphanedCronSpawns closes crash evidence that can otherwise hold a
// Forbid job forever. staleBefore is now at daemon startup (all running work is
// from the prior process) and a bounded cutoff during ordinary ticks.
func InterruptOrphanedCronSpawns(now, staleBefore time.Time) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	detail := "daemon stopped before cron spawn completed"
	workerRes, err := tx.Exec(`UPDATE trigger_workers
		SET state='interrupted',detail=?,completed_at=?
		WHERE state='reserved' AND cron_job_id IS NOT NULL AND created_at<=? AND (
			deadline_at IS NOT NULL AND deadline_at<=? OR cron_run_id IS NULL OR
			NOT EXISTS (SELECT 1 FROM agent_cron_runs r WHERE r.id=trigger_workers.cron_run_id AND r.status='running') OR
			EXISTS (SELECT 1 FROM agent_cron_runs r WHERE r.id=trigger_workers.cron_run_id AND r.status='running' AND r.fired_at<=?)
		)`, detail, dbTime(now.UTC()), dbTime(staleBefore.UTC()), dbTime(now.UTC()), dbTime(staleBefore.UTC()))
	if err != nil {
		return 0, err
	}
	runRes, err := tx.Exec(`UPDATE agent_cron_runs SET status='interrupted',error_msg=?
		WHERE status='running' AND fired_at<=?`, detail, dbTime(staleBefore.UTC()))
	if err != nil {
		return 0, err
	}
	workers, err := workerRes.RowsAffected()
	if err != nil {
		return 0, err
	}
	runs, err := runRes.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return workers + runs, nil
}
