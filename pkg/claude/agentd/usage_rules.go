package agentd

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usagerules"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// usage_rules.go — the evaluator behind usage limits and usage profiles
// (pkg/claude/common/usagerules). It runs on the usage poller's tick:
//
//   - a limit (global or group) has one counter over an aggregate and fires
//     its ladder against that counter; agent-targeted steps reach every
//     running agent in the limit's scope;
//   - a usage profile applies to each agent separately. Which profiles apply
//     is decided by layer: the agent's own assignments, else the profiles of
//     every group the agent belongs to, else the global default. The nearest
//     non-empty layer wins as a whole, and every profile on that layer applies.
//
// A step fires once per (rule, subject, period span, budget): the firing row
// is the dedupe, so a restart, a deleted notification or a re-tick never
// repeats it, while a new period or a changed budget re-arms the ladder.

// usageRuleFiringRetention bounds the firing history. Lifetime-period
// firings are kept regardless; see db.PruneUsageRuleFirings.
const usageRuleFiringRetention = 120 * 24 * time.Hour

// usageRulesMu serializes evaluation: the snapshot path and the poller may
// both trigger it.
var usageRulesMu sync.Mutex

// Seams for tests: stopping an agent and listing live tmux sessions are
// subprocess boundaries.
var (
	usageRulesHardStop = func(convID string) memberOpResult {
		return stopOneConvWithIntent(convID, true, db.AgentExitActionForceStop, "")
	}
	usageRulesLiveSessions = session.LiveTmuxSessions
)

// usageRuleStatus is one rule's current reading, kept from the latest
// evaluation for the dashboard and the API. Reached is the action of the
// highest step the value has crossed ("" when none); Next is the first step
// not yet crossed.
type usageRuleStatus struct {
	RuleKey   string            `json:"rule_key"`
	Scope     string            `json:"scope"` // global | group | agent
	LimitID   int64             `json:"limit_id,omitempty"`
	GroupID   int64             `json:"group_id,omitempty"`
	Group     string            `json:"group,omitempty"`
	AgentID   string            `json:"agent_id,omitempty"`
	ProfileID int64             `json:"profile_id,omitempty"`
	Profile   string            `json:"profile,omitempty"`
	Layer     string            `json:"layer,omitempty"`  // agent | group | global: where the profile came from
	Paused    string            `json:"paused,omitempty"` // exempt | snoozed | offline: steps wait
	Name      string            `json:"name"`
	Metric    usagerules.Metric `json:"metric"`
	Period    usagerules.Period `json:"period"`
	Window    string            `json:"window,omitempty"`
	Value     float64           `json:"value"`
	Budget    float64           `json:"budget"`
	Pct       float64           `json:"pct"`
	DryRun    bool              `json:"dry_run,omitempty"`
	Reached   usagerules.Action `json:"reached,omitempty"`
	ReachedAt float64           `json:"reached_at,omitempty"`
	Next      *usagerules.Step  `json:"next,omitempty"`
}

var (
	usageRuleStatusMu   sync.RWMutex
	usageRuleStatusLast []usageRuleStatus
	usageRuleStatusAt   time.Time
)

// latestUsageRuleStatus returns the statuses from the latest evaluation and
// when it ran (zero before the first).
func latestUsageRuleStatus() ([]usageRuleStatus, time.Time) {
	usageRuleStatusMu.RLock()
	defer usageRuleStatusMu.RUnlock()
	return usageRuleStatusLast, usageRuleStatusAt
}

// latestUsageLimitStatus is the global and group part of the latest
// evaluation, for the dashboard banner.
func latestUsageLimitStatus() []usageRuleStatus {
	all, _ := latestUsageRuleStatus()
	var out []usageRuleStatus
	for _, s := range all {
		if s.Scope != string(usagerules.ScopeAgent) {
			out = append(out, s)
		}
	}
	return out
}

func usageStatusFor(r usagerules.Rule, value float64) (reached usagerules.Action, reachedAt float64, next *usagerules.Step) {
	pct := usagerules.Pct(value, r.Budget)
	for i := range r.Steps {
		s := r.Steps[i]
		if pct >= s.At {
			reached, reachedAt = s.Action, s.At
			continue
		}
		next = &s
		break
	}
	return reached, reachedAt, next
}

// usageSpendSlice is one agent-attributed slice of spend on one local day.
type usageSpendSlice struct {
	agentID string
	day     string
	kind    string // real | what_if
	usd     float64
}

// usageSpend indexes the cost ledger for rule evaluation.
type usageSpend struct {
	all     []usageSpendSlice
	byAgent map[string][]usageSpendSlice
}

func newUsageSpend(deltas []costDelta, agentByConv map[string]string) *usageSpend {
	s := &usageSpend{byAgent: map[string][]usageSpendSlice{}}
	for _, d := range deltas {
		slice := usageSpendSlice{agentID: agentByConv[d.convID], day: d.day, kind: d.kind, usd: d.usd}
		s.all = append(s.all, slice)
		if slice.agentID != "" {
			s.byAgent[slice.agentID] = append(s.byAgent[slice.agentID], slice)
		}
	}
	return s
}

func usageMetricCounts(m usagerules.Metric, kind string) bool {
	switch m {
	case usagerules.MetricRealUSD:
		return kind == "real"
	case usagerules.MetricWhatIfUSD:
		return kind == "what_if"
	default:
		return true
	}
}

func sumUsageSlices(slices []usageSpendSlice, m usagerules.Metric, fromDay string) float64 {
	total := 0.0
	for _, s := range slices {
		if (fromDay == "" || s.day >= fromDay) && usageMetricCounts(m, s.kind) {
			total += s.usd
		}
	}
	return total
}

// usageRulesState is everything one evaluation reads, loaded once.
type usageRulesState struct {
	now         time.Time
	limits      []*db.UsageLimit
	profiles    map[int64]*db.UsageProfile
	assignments []db.UsageProfileAssignment
	agents      map[string]*db.Agent // active agents by id
	groupsOf    map[string][]int64   // agent id → group ids
	members     map[int64][]string   // group id → agent ids
	groupNames  map[int64]string     // group id → name
	alive       map[string]struct{}  // live tmux sessions
	spend       *usageSpend          // the counted cost ledger
	windows     []accountUsageWindow // nil until needed
	windowsRead bool
	statuses    []usageRuleStatus
	overrides   map[string]*db.UsageAgentOverride // per-agent operator adjustments
}

// pollUsageRules evaluates every enabled limit and every agent's effective
// usage profiles. It is cheap when nothing is configured: two small queries.
func pollUsageRules(now time.Time) {
	usageRulesMu.Lock()
	defer usageRulesMu.Unlock()
	st, err := loadUsageRulesState(now)
	if err != nil {
		slog.Warn("usage rules: load", "error", err)
		return
	}
	if st == nil {
		usageRuleStatusMu.Lock()
		usageRuleStatusLast, usageRuleStatusAt = nil, now
		usageRuleStatusMu.Unlock()
		return
	}
	for _, l := range st.limits {
		st.evaluateLimit(l)
	}
	st.evaluateAgents()
	usageRuleStatusMu.Lock()
	usageRuleStatusLast, usageRuleStatusAt = st.statuses, now
	usageRuleStatusMu.Unlock()
	if _, err := db.PruneUsageRuleFirings(now.Add(-usageRuleFiringRetention)); err != nil {
		slog.Debug("usage rules: prune firings", "error", err)
	}
}

func loadUsageRulesState(now time.Time) (*usageRulesState, error) {
	limits, err := db.ListUsageLimits()
	if err != nil {
		return nil, err
	}
	assignments, err := db.ListUsageProfileAssignments()
	if err != nil {
		return nil, err
	}
	st := &usageRulesState{now: now, assignments: assignments}
	for _, l := range limits {
		if l.Enabled {
			st.limits = append(st.limits, l)
		}
	}
	if len(st.limits) == 0 && len(assignments) == 0 {
		return nil, nil
	}
	profiles, err := db.ListUsageProfiles()
	if err != nil {
		return nil, err
	}
	st.profiles = make(map[int64]*db.UsageProfile, len(profiles))
	for _, p := range profiles {
		st.profiles[p.ID] = p
	}
	if st.overrides, err = db.ListUsageAgentOverrides(); err != nil {
		return nil, err
	}
	agents, err := db.ListActiveAgents()
	if err != nil {
		return nil, err
	}
	st.agents = make(map[string]*db.Agent, len(agents))
	for _, a := range agents {
		st.agents[a.AgentID] = a
	}
	groups, err := db.ListAgentGroups()
	if err != nil {
		return nil, err
	}
	st.groupNames = make(map[int64]string, len(groups))
	ids := make([]int64, 0, len(groups))
	for _, g := range groups {
		st.groupNames[g.ID] = g.Name
		ids = append(ids, g.ID)
	}
	memberRows, err := db.ListAgentGroupMembersBatch(ids)
	if err != nil {
		return nil, err
	}
	agentByConv, err := db.AgentIDsByConv()
	if err != nil {
		return nil, err
	}
	st.groupsOf = map[string][]int64{}
	st.members = map[int64][]string{}
	for groupID, rows := range memberRows {
		for _, m := range rows {
			agentID := agentByConv[m.ConvID]
			if agentID == "" {
				continue
			}
			st.groupsOf[agentID] = append(st.groupsOf[agentID], groupID)
			st.members[groupID] = append(st.members[groupID], agentID)
		}
	}
	rows, olderSpend, err := st.spendRows()
	if err != nil {
		return nil, err
	}
	st.spend = newUsageSpend(mixedCostDeltasFromRows(rows), agentByConv)
	// An agent reports its reading once it has ever spent, so one idle since
	// before the rows read keeps its row at zero.
	for _, convID := range olderSpend {
		if agentID := agentByConv[convID]; agentID != "" {
			if _, seen := st.spend.byAgent[agentID]; !seen {
				st.spend.byAgent[agentID] = nil
			}
		}
	}
	// Liveness gates every agent-directed action. Without it a firing could
	// be recorded (and so used up) without reaching anyone, so a failed
	// listing skips the whole tick rather than evaluating blind.
	if st.alive, err = usageRulesLiveSessions(); err != nil {
		return nil, fmt.Errorf("list live sessions: %w", err)
	}
	return st, nil
}

// spendRows reads the ledger the loaded rules measure, with the conversations
// that spent in rows left out of it. A day, week or month rule stays inside
// db.CostReadLimitDays; only a lifetime rule, the one period that reaches
// further, needs the whole history. So does a stored opening that cannot serve
// the read, because what it saves here also decides which agents report.
func (st *usageRulesState) spendRows() (rows []db.CostDailyRow, olderSpend []string, err error) {
	earliest := st.now.Format(costDayKey)
	lifetime := false
	measure := func(rules []usagerules.Rule) {
		for _, r := range rules {
			if r.Metric == usagerules.MetricWindowPct {
				continue
			}
			if r.Period == usagerules.PeriodLifetime {
				lifetime = true
				continue
			}
			earliest = min(earliest, usagerules.PeriodStartDay(r.Period, st.now))
		}
	}
	for _, l := range st.limits {
		measure([]usagerules.Rule{l.Rule})
	}
	// Only a profile effectiveUsageProfiles can resolve is ever evaluated: the
	// table also holds links, and rows of agents that have retired.
	for _, a := range st.assignments {
		applies := a.Scope == db.UsageScopeGroup || a.Scope == db.UsageScopeGlobal ||
			(a.Scope == db.UsageScopeAgent && st.agents[a.ScopeID] != nil)
		if p := st.profiles[a.ProfileID]; p != nil && applies {
			measure(p.Rules)
		}
	}
	if !lifetime && db.CostOpeningServes(earliest) {
		read, err := countedCostRowsForWindow(earliest, "", false)
		if err != nil {
			return nil, nil, err
		}
		if read.fromOpening {
			if olderSpend, err = db.CostOpeningSpentConvs(); err == nil {
				return read.counted, olderSpend, nil
			}
			slog.Warn("cost opening balance unusable; reading the full cost history", "error", err)
		}
	}
	if lifetime {
		db.NoteLifetimeRuleCostRead()
	} else {
		db.NoteFullCostRead()
		requestCostOpeningRefresh()
	}
	rows, _, _, err = countedCostDailyRows()
	return rows, nil, err
}

// usageLinkedLayer is the layer of an agent's profiles copied from a spawn
// profile's link. Unlike an explicit agent assignment it adds to the layer
// the agent inherits instead of replacing it, so neither a default spawn
// profile's link nor an agent choosing a spawn profile can shed a group or
// global budget.
const usageLinkedLayer = "spawn_profile"

// usageLinkedSourcePrefix marks an agent assignment copied from a spawn
// profile link (applySpawnUsageProfiles).
const usageLinkedSourcePrefix = db.UsageLinkedSourcePrefix

// effectiveUsageProfiles resolves the profiles that apply to one agent and
// the layer they came from: "agent", "group" or "global" ("" when none), or
// "spawn_profile" / "spawn_profile+group" / "spawn_profile+global" when a
// spawn profile's linked profiles add to the inherited layer.
func effectiveUsageProfiles(agentID string, groupIDs []int64, assignments []db.UsageProfileAssignment, profiles map[int64]*db.UsageProfile) ([]*db.UsageProfile, string) {
	groupScope := map[string]bool{}
	for _, id := range groupIDs {
		groupScope[db.UsageGroupScopeID(id)] = true
	}
	byLayer := map[string][]*db.UsageProfile{}
	seen := map[string]map[int64]bool{}
	for _, a := range assignments {
		var layer string
		switch {
		case a.Scope == db.UsageScopeAgent && a.ScopeID == agentID && strings.HasPrefix(a.Source, usageLinkedSourcePrefix):
			layer = usageLinkedLayer
		case a.Scope == db.UsageScopeAgent && a.ScopeID == agentID:
			layer = db.UsageScopeAgent
		case a.Scope == db.UsageScopeGroup && groupScope[a.ScopeID]:
			layer = db.UsageScopeGroup
		case a.Scope == db.UsageScopeGlobal:
			layer = db.UsageScopeGlobal
		default:
			continue
		}
		p := profiles[a.ProfileID]
		if p == nil {
			continue
		}
		if seen[layer] == nil {
			seen[layer] = map[int64]bool{}
		}
		if seen[layer][p.ID] {
			continue // the same profile from two groups applies once
		}
		seen[layer][p.ID] = true
		byLayer[layer] = append(byLayer[layer], p)
	}
	if ps := byLayer[db.UsageScopeAgent]; len(ps) > 0 {
		return ps, db.UsageScopeAgent
	}
	var inherited []*db.UsageProfile
	inheritedLayer := ""
	for _, layer := range []string{db.UsageScopeGroup, db.UsageScopeGlobal} {
		if ps := byLayer[layer]; len(ps) > 0 {
			inherited, inheritedLayer = ps, layer
			break
		}
	}
	linked := byLayer[usageLinkedLayer]
	if len(linked) == 0 {
		return inherited, inheritedLayer
	}
	out := append([]*db.UsageProfile(nil), linked...)
	for _, p := range inherited {
		if !seen[usageLinkedLayer][p.ID] {
			out = append(out, p)
		}
	}
	if inheritedLayer == "" {
		return out, usageLinkedLayer
	}
	return out, usageLinkedLayer + "+" + inheritedLayer
}

func (st *usageRulesState) evaluateAgents() {
	if len(st.assignments) == 0 {
		return
	}
	for agentID, slices := range st.spend.byAgent {
		a := st.agents[agentID]
		if a == nil || a.CurrentConvID == "" {
			continue // retired or unknown: nothing left to act on
		}
		// A stopped agent spends nothing: its due steps wait until it runs
		// again, so a wrap-up or hard stop crossed while it was offline (late
		// cost reports) still applies on resume. A snoozed agent's steps
		// wait for the snooze to end; an exempt agent's never fire. All three
		// still report their readings.
		paused := ""
		override := st.overrides[agentID]
		switch {
		case override != nil && override.Exempt:
			paused = "exempt"
		case override != nil && override.SnoozeUntil.After(st.now):
			paused = "snoozed"
		case !isConvOnlineIn(a.CurrentConvID, st.alive):
			paused = "offline"
		}
		profiles, layer := effectiveUsageProfiles(agentID, st.groupsOf[agentID], st.assignments, st.profiles)
		for _, p := range profiles {
			for _, r := range p.Rules {
				key := usageProfileRuleKey(p.ID, r.Name)
				if override != nil {
					r.Budget += override.Extensions[key]
				}
				value := sumUsageSlices(slices, r.Metric, usagerules.PeriodStartDay(r.Period, st.now))
				st.fire(usageRuleTarget{
					ruleKey: key,
					subject: agentID,
					scope:   usagerules.ScopeAgent,
					rule:    r,
					owner:   "usage profile " + p.Name,
					agents:  []string{agentID},
					paused:  paused,
					status:  usageRuleStatus{AgentID: agentID, ProfileID: p.ID, Profile: p.Name, Layer: layer, Paused: paused},
				}, value, usagerules.PeriodKey(r.Period, st.now, time.Time{}))
			}
		}
	}
}

func (st *usageRulesState) evaluateLimit(l *db.UsageLimit) {
	r := l.Rule
	target := usageRuleTarget{ruleKey: usageLimitRuleKey(l.ID), rule: r, status: usageRuleStatus{LimitID: l.ID, GroupID: l.GroupID}}
	switch l.Scope {
	case db.UsageScopeGlobal:
		target.subject, target.scope, target.owner = usageLimitSubject(l), usagerules.ScopeGlobal, "global limit"
		for id := range st.agents {
			target.agents = append(target.agents, id)
		}
	case db.UsageScopeGroup:
		name, ok := st.groupNames[l.GroupID]
		if !ok {
			return
		}
		target.subject, target.scope, target.owner = usageLimitSubject(l), usagerules.ScopeGroup, "group "+name+" limit"
		target.group, target.status.Group = name, name
		for _, id := range st.members[l.GroupID] {
			if st.agents[id] != nil {
				target.agents = append(target.agents, id)
			}
		}
	default:
		return
	}
	if r.Metric == usagerules.MetricWindowPct {
		w, ok := st.window(r.Window)
		if !ok {
			return // no current reading: never act on a stale or missing quota
		}
		resets, _ := time.Parse(time.RFC3339Nano, w.ResetsAt)
		st.fire(target, w.Pct, st.windowPeriodKey(target, resets))
		return
	}
	from := usagerules.PeriodStartDay(r.Period, st.now)
	value := 0.0
	if l.Scope == db.UsageScopeGlobal {
		value = sumUsageSlices(st.spend.all, r.Metric, from)
	} else {
		for _, id := range st.members[l.GroupID] {
			value += sumUsageSlices(st.spend.byAgent[id], r.Metric, from)
		}
	}
	st.fire(target, value, usagerules.PeriodKey(r.Period, st.now, time.Time{}))
}

// usageWindowResetTolerance is how far apart two reported reset times of one
// quota window may be. Providers jitter them by seconds between readings.
const usageWindowResetTolerance = 10 * time.Minute

// windowPeriodKey keys a quota-window span by its reset time, reusing the key
// of an earlier firing whose reset lies within the tolerance, so jitter
// across a minute boundary never splits one window into two ladders.
func (st *usageRulesState) windowPeriodKey(t usageRuleTarget, resets time.Time) string {
	key := usagerules.PeriodKey(usagerules.PeriodWindow, st.now, resets)
	if resets.IsZero() {
		return key
	}
	firings, err := db.ListUsageRuleFiringsForSubject(t.ruleKey, t.subject)
	if err != nil {
		slog.Warn("usage rules: read window firings", "rule", t.ruleKey, "error", err)
		return key
	}
	for _, f := range firings {
		span, _, _ := strings.Cut(f.PeriodKey, "|")
		prev, err := time.Parse(time.RFC3339, span)
		if err != nil {
			continue
		}
		if d := prev.Sub(resets); d <= usageWindowResetTolerance && d >= -usageWindowResetTolerance {
			return span
		}
	}
	return key
}

// window returns the current reading of a "<provider>:<window>" quota.
func (st *usageRulesState) window(spec string) (accountUsageWindow, bool) {
	if !st.windowsRead {
		st.windowsRead = true
		usage, err := collectAccountUsage(st.now, 0)
		if err != nil {
			slog.Warn("usage rules: read quota windows", "error", err)
		}
		st.windows = usage.Windows
	}
	provider, name, _ := strings.Cut(spec, ":")
	for _, w := range st.windows {
		if w.Provider == provider && w.WindowName == name && w.Available {
			return w, true
		}
	}
	return accountUsageWindow{}, false
}

// usageRuleTarget is one rule bound to its subject and the agents its
// agent-directed steps reach.
type usageRuleTarget struct {
	ruleKey string
	subject string
	scope   usagerules.Scope
	rule    usagerules.Rule
	owner   string   // human-readable origin, e.g. "usage profile default"
	group   string   // group name for a group limit
	fleet   string   // the nodes a fleet limit covers, e.g. "pool gpu-rigs"
	agents  []string // agent ids agent-directed steps reach
	paused  string   // non-empty: report the reading, fire nothing
	status  usageRuleStatus
}

// usageProfileRuleKey keys one usage-profile rule; budget extensions and
// firings refer to it.
func usageProfileRuleKey(profileID int64, ruleName string) string {
	return fmt.Sprintf("profile:%d:%s", profileID, strings.ToLower(ruleName))
}

func usageLimitRuleKey(id int64) string { return "limit:" + strconv.FormatInt(id, 10) }

func usageLimitSubject(l *db.UsageLimit) string {
	if l.Scope == db.UsageScopeGroup {
		return "group:" + strconv.FormatInt(l.GroupID, 10)
	}
	return "global"
}

func formatUsageBudget(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// fire records and performs every due step of t at value. It returns the
// steps that fired now (not the silent repeats), for a fleet limit to carry
// to its peers.
func (st *usageRulesState) fire(t usageRuleTarget, value float64, period string) []usagerules.Firing {
	r := t.rule
	budget := r.Budget
	periodKey := period + "|b=" + formatUsageBudget(budget)
	if r.DryRun {
		// A dry run keeps its own ledger, so switching it off re-arms the
		// ladder for real instead of finding every step already fired.
		periodKey += "|dry"
	}
	pct := usagerules.Pct(value, budget)
	status := t.status
	status.RuleKey, status.Scope, status.Name = t.ruleKey, string(t.scope), r.Name
	status.Metric, status.Period, status.Window = r.Metric, r.Period, r.Window
	status.Value, status.Budget, status.Pct, status.DryRun = value, budget, pct, r.DryRun
	status.Reached, status.ReachedAt, status.Next = usageStatusFor(r, value)
	st.statuses = append(st.statuses, status)
	if t.paused != "" {
		return nil
	}
	var fired map[string]time.Time
	baseFired := func(key string) (time.Time, bool) {
		if fired == nil {
			var err error
			if fired, err = db.UsageRuleFiringTimes(t.ruleKey, t.subject, periodKey); err != nil {
				slog.Warn("usage rules: read firing times", "rule", t.ruleKey, "error", err)
				fired = map[string]time.Time{}
			}
		}
		at, ok := fired[key]
		return at, ok
	}
	firedNow := map[string]bool{}
	var out []usagerules.Firing
	for _, f := range usagerules.Due(r, pct, st.now, baseFired) {
		// A repeat owed in the same evaluation as its first crossing is
		// recorded silently: the crossing's own message already says it.
		quiet := f.Repeat && firedNow[f.Step.Key()]
		var msg *db.HumanMessage
		if !quiet {
			msg = usageRuleOperatorMessage(st, t, f, value, pct)
		}
		inserted, err := db.InsertUsageRuleFiring(db.UsageRuleFiring{
			RuleKey: t.ruleKey, Subject: t.subject, PeriodKey: periodKey, StepKey: f.Key,
			Action: string(f.Step.Action), RuleName: r.Name, Value: value, Budget: budget,
			DryRun: r.DryRun, FiredAt: st.now,
		}, msg)
		if err != nil {
			slog.Warn("usage rules: record firing", "rule", t.ruleKey, "subject", t.subject, "step", f.Key, "error", err)
			continue
		}
		if !inserted {
			continue
		}
		firedNow[f.Key] = true
		if quiet {
			continue
		}
		if msg != nil {
			dispatchHumanMessageNotification("", msg.FromTitle, t.group, msg.Subject, msg.Body)
		}
		if r.DryRun {
			continue
		}
		out = append(out, f)
		st.act(t, f, value, pct)
	}
	if !r.DryRun {
		st.enforceStandingStops(t, pct, firedNow, baseFired)
	}
	return out
}

// enforceStandingStops keeps a fired hard stop in force for the rest of its
// period: an agent in scope that runs again (resumed, or started after the
// stop fired, perhaps while every agent was offline) is stopped again while
// the value stays at or above the step. Extending the budget re-keys the
// period and lifts it; snoozing or exempting an agent pauses its profile
// rules, so the operator can let one agent finish.
func (st *usageRulesState) enforceStandingStops(t usageRuleTarget, pct float64, firedNow map[string]bool, baseFired func(string) (time.Time, bool)) {
	for _, step := range t.rule.Steps {
		if step.Action != usagerules.ActionHardStop || pct < step.At || firedNow[step.Key()] {
			continue
		}
		if _, ok := baseFired(step.Key()); !ok {
			continue
		}
		for _, conv := range st.runningConvs(t.agents) {
			res := usageRulesHardStop(conv)
			slog.Info("usage rules: hard stop still in force", "rule", t.ruleKey, "subject", t.subject, "conv", conv, "result", res.Action, "detail", res.Detail)
			title := agent.CachedTitle(conv)
			if title == "" {
				title = conv
			}
			msg := &db.HumanMessage{FromTitle: "Usage limits", GroupName: t.group, CreatedAt: st.now,
				Subject: fmt.Sprintf("Stopped %s again: %q is still past its hard stop", title, t.rule.Name),
				Body: fmt.Sprintf("%s was running while %q (%s) stands at %.0f%% of its budget, past the hard stop at %g%%. Extend the budget, or snooze or exempt the agent, to let it run.",
					title, t.rule.Name, t.owner, pct, step.At)}
			if _, err := db.InsertHumanMessage(msg); err != nil {
				slog.Warn("usage rules: record re-stop message", "error", err)
			} else {
				dispatchHumanMessageNotification("", msg.FromTitle, t.group, msg.Subject, msg.Body)
			}
		}
		return
	}
}

func usageRuleSubjectLabel(t usageRuleTarget, st *usageRulesState) string {
	switch t.scope {
	case usagerules.ScopeAgent:
		if a := st.agents[t.subject]; a != nil {
			return "Agent " + agent.CachedTitle(a.CurrentConvID)
		}
		return "Agent " + t.subject
	case usagerules.ScopeGroup:
		return "Group " + t.group
	case usagerules.ScopeFleet:
		return "Fleet (" + t.fleet + ")"
	default:
		return "All agents"
	}
}

func usageMetricLabel(r usagerules.Rule) string {
	switch r.Metric {
	case usagerules.MetricRealUSD:
		return "API spend"
	case usagerules.MetricWhatIfUSD:
		return "subscription-equivalent cost"
	case usagerules.MetricWindowPct:
		return r.Window + " quota"
	default:
		return "cost"
	}
}

func usagePeriodLabel(p usagerules.Period) string {
	switch p {
	case usagerules.PeriodDay:
		return "today"
	case usagerules.PeriodWeek:
		return "this week"
	case usagerules.PeriodMonth:
		return "this month"
	case usagerules.PeriodWindow:
		return "this window"
	default:
		return "in total"
	}
}

func usageAmount(r usagerules.Rule, v float64) string {
	if r.Metric == usagerules.MetricWindowPct {
		return fmt.Sprintf("%.0f%%", v)
	}
	return fmt.Sprintf("$%.2f", v)
}

// usageRuleOperatorMessage is the Messages-tab entry for a firing, or nil
// when the step does not reach the operator. Every non-notify step and every
// dry run tells the operator, so nothing drastic ever happens silently.
func usageRuleOperatorMessage(st *usageRulesState, t usageRuleTarget, f usagerules.Firing, value, pct float64) *db.HumanMessage {
	r := t.rule
	toOperator := r.DryRun || f.Step.Action != usagerules.ActionNotify
	for _, target := range f.Step.Notify {
		if target == usagerules.TargetOperator {
			toOperator = true
		}
	}
	if !toOperator {
		return nil
	}
	who := usageRuleSubjectLabel(t, st)
	what := map[usagerules.Action]string{
		usagerules.ActionNotify:     "reached",
		usagerules.ActionWrapUp:     "reached its wrap-up threshold at",
		usagerules.ActionHardStop:   "reached its hard stop at",
		usagerules.ActionBlockSpawn: "blocks new spawns at",
	}[f.Step.Action]
	if t.scope != usagerules.ScopeAgent && f.Step.Action != usagerules.ActionNotify {
		what = map[usagerules.Action]string{
			usagerules.ActionWrapUp:     "asks its agents to wrap up at",
			usagerules.ActionHardStop:   "stops its agents at",
			usagerules.ActionBlockSpawn: "blocks new spawns at",
		}[f.Step.Action]
	}
	subject := fmt.Sprintf("%s %s %.0f%% of %q", who, what, pct, r.Name)
	body := fmt.Sprintf("%s %s %s: %s of %s (%.0f%%), rule %q from the %s.",
		who, usageMetricLabel(r), usagePeriodLabel(r.Period), usageAmount(r, value), usageAmount(r, r.Budget), pct, r.Name, t.owner)
	if r.DryRun {
		subject = "[dry run] " + subject
		body += " Dry run: no agent was messaged, stopped or blocked."
	}
	if r.Metric != usagerules.MetricWindowPct {
		body += " Amounts are estimates from the cost ledger."
	}
	return &db.HumanMessage{FromTitle: "Usage limits", GroupName: t.group, Subject: subject, Body: body, CreatedAt: st.now}
}

// act performs a fired step's effect on agents. Operator messages were
// already written with the firing.
func (st *usageRulesState) act(t usageRuleTarget, f usagerules.Firing, value, pct float64) {
	switch f.Step.Action {
	case usagerules.ActionNotify:
		for _, target := range f.Step.Notify {
			if target == usagerules.TargetAgent {
				st.messageAgents(t, usageRuleAgentNotice(t, f, value, pct))
			}
		}
	case usagerules.ActionWrapUp:
		st.messageAgents(t, usageRuleWrapUpNotice(t, value, pct))
	case usagerules.ActionHardStop:
		for _, conv := range st.runningConvs(t.agents) {
			res := usageRulesHardStop(conv)
			slog.Info("usage rules: hard stop", "rule", t.ruleKey, "subject", t.subject, "conv", conv, "result", res.Action, "detail", res.Detail)
		}
	case usagerules.ActionBlockSpawn:
		// Enforced at spawn time from the firing record.
	}
}

// runningConvs maps agent ids to the current convs of those with a live
// pane. Notices and stops only go to running agents: a stopped agent spends
// nothing, and a backlog of stale budget notices would greet it on resume.
func (st *usageRulesState) runningConvs(agentIDs []string) []string {
	var out []string
	for _, id := range agentIDs {
		a := st.agents[id]
		if a == nil || a.CurrentConvID == "" {
			continue
		}
		if isConvOnlineIn(a.CurrentConvID, st.alive) {
			out = append(out, a.CurrentConvID)
		}
	}
	return out
}

type usageRuleNotice struct{ subject, body string }

func (st *usageRulesState) messageAgents(t usageRuleTarget, n usageRuleNotice) {
	for _, conv := range st.runningConvs(t.agents) {
		if _, err := queueAgentMessage(&db.AgentMessage{ToConv: conv, Subject: n.subject, Body: n.body}); err != nil {
			slog.Warn("usage rules: message agent", "rule", t.ruleKey, "conv", conv, "error", err)
		}
	}
}

func usageRuleScopePhrase(t usageRuleTarget) string {
	switch t.scope {
	case usagerules.ScopeAgent:
		return "You have used"
	case usagerules.ScopeGroup:
		return "Your group " + t.group + " has used"
	case usagerules.ScopeFleet:
		return "Your fleet (" + t.fleet + ") has used"
	default:
		return "This tclaude instance has used"
	}
}

// usageUsedText says how much of t's budget is used. A peer's fleet limit
// shares only its percentage (no budget), never the fleet's spend.
func usageUsedText(t usageRuleTarget, value, pct float64) string {
	r := t.rule
	if r.Budget <= 0 {
		return fmt.Sprintf("%s %.0f%% of its budget for %s, rule %q from the %s.",
			usageRuleScopePhrase(t), pct, usagePeriodLabel(r.Period), r.Name, t.owner)
	}
	return fmt.Sprintf("%s %s of the %s budget for %s (%.0f%%), rule %q from the %s.",
		usageRuleScopePhrase(t), usageAmount(r, value), usageAmount(r, r.Budget), usagePeriodLabel(r.Period), pct, r.Name, t.owner)
}

func usageRuleAgentNotice(t usageRuleTarget, f usagerules.Firing, value, pct float64) usageRuleNotice {
	r := t.rule
	body := usageUsedText(t, value, pct)
	if next := nextUsageStep(r, f.Step.At); next != nil {
		body += fmt.Sprintf(" Next step at %.0f%%: %s.", next.At, usageStepPhrase(next.Action))
	}
	body += " Prefer cheaper approaches where you can, and keep the work focused."
	return usageRuleNotice{subject: fmt.Sprintf("Usage budget: %.0f%% of %q", pct, r.Name), body: body}
}

func usageRuleWrapUpNotice(t usageRuleTarget, value, pct float64) usageRuleNotice {
	r := t.rule
	body := usageUsedText(t, value, pct) + " " +
		"Please wrap up now: stop starting new work, bring what you are doing to a safe stopping point, " +
		"write a short handoff summary (what is done, what is left, where things are) and send it to whoever assigned your work, " +
		"then stay idle until a human or your coordinator tells you to continue."
	if next := nextUsageStep(r, pct); next != nil && next.Action == usagerules.ActionHardStop {
		body += fmt.Sprintf(" The process will be stopped at %.0f%%.", next.At)
	}
	return usageRuleNotice{subject: fmt.Sprintf("Usage budget reached: please wrap up (%q)", r.Name), body: body}
}

func nextUsageStep(r usagerules.Rule, above float64) *usagerules.Step {
	for i := range r.Steps {
		if r.Steps[i].At > above && r.Steps[i].Action != usagerules.ActionNotify {
			return &r.Steps[i]
		}
	}
	return nil
}

func usageStepPhrase(a usagerules.Action) string {
	switch a {
	case usagerules.ActionWrapUp:
		return "you will be asked to wrap up"
	case usagerules.ActionHardStop:
		return "the process will be stopped"
	case usagerules.ActionBlockSpawn:
		return "new spawns will be blocked"
	default:
		return string(a)
	}
}
