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
		return
	}
	for _, l := range st.limits {
		st.evaluateLimit(l)
	}
	st.evaluateAgents()
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
	rows, _, _, err := countedCostDailyRows()
	if err != nil {
		return nil, err
	}
	st.spend = newUsageSpend(mixedCostDeltasFromRows(rows), agentByConv)
	// Liveness gates every agent-directed action. Without it a firing could
	// be recorded (and so used up) without reaching anyone, so a failed
	// listing skips the whole tick rather than evaluating blind.
	if st.alive, err = usageRulesLiveSessions(); err != nil {
		return nil, fmt.Errorf("list live sessions: %w", err)
	}
	return st, nil
}

// effectiveUsageProfiles resolves the profiles that apply to one agent and
// the layer they came from: "agent", "group" or "global" ("" when none).
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
	for _, layer := range []string{db.UsageScopeAgent, db.UsageScopeGroup, db.UsageScopeGlobal} {
		if ps := byLayer[layer]; len(ps) > 0 {
			return ps, layer
		}
	}
	return nil, ""
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
		if !isConvOnlineIn(a.CurrentConvID, st.alive) {
			// A stopped agent spends nothing. Its due steps wait until it
			// runs again, so a wrap-up or hard stop crossed while it was
			// offline (late cost reports) still applies on resume.
			continue
		}
		profiles, _ := effectiveUsageProfiles(agentID, st.groupsOf[agentID], st.assignments, st.profiles)
		for _, p := range profiles {
			for _, r := range p.Rules {
				value := sumUsageSlices(slices, r.Metric, usagerules.PeriodStartDay(r.Period, st.now))
				st.fire(usageRuleTarget{
					ruleKey: fmt.Sprintf("profile:%d:%s", p.ID, strings.ToLower(r.Name)),
					subject: agentID,
					scope:   usagerules.ScopeAgent,
					rule:    r,
					owner:   "usage profile " + p.Name,
					agents:  []string{agentID},
				}, value, usagerules.PeriodKey(r.Period, st.now, time.Time{}))
			}
		}
	}
}

func (st *usageRulesState) evaluateLimit(l *db.UsageLimit) {
	r := l.Rule
	target := usageRuleTarget{ruleKey: "limit:" + strconv.FormatInt(l.ID, 10), rule: r}
	switch l.Scope {
	case db.UsageScopeGlobal:
		target.subject, target.scope, target.owner = "global", usagerules.ScopeGlobal, "global limit"
		for id := range st.agents {
			target.agents = append(target.agents, id)
		}
	case db.UsageScopeGroup:
		name, ok := st.groupNames[l.GroupID]
		if !ok {
			return
		}
		target.subject, target.scope, target.owner = "group:"+strconv.FormatInt(l.GroupID, 10), usagerules.ScopeGroup, "group "+name+" limit"
		target.group = name
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
	agents  []string // agent ids agent-directed steps reach
}

func formatUsageBudget(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// fire records and performs every due step of t at value.
func (st *usageRulesState) fire(t usageRuleTarget, value float64, period string) {
	r := t.rule
	budget := r.Budget
	periodKey := period + "|b=" + formatUsageBudget(budget)
	if r.DryRun {
		// A dry run keeps its own ledger, so switching it off re-arms the
		// ladder for real instead of finding every step already fired.
		periodKey += "|dry"
	}
	pct := usagerules.Pct(value, budget)
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
		st.act(t, f, value, pct)
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
	default:
		return "This tclaude instance has used"
	}
}

func usageRuleAgentNotice(t usageRuleTarget, f usagerules.Firing, value, pct float64) usageRuleNotice {
	r := t.rule
	body := fmt.Sprintf("%s %s of the %s budget for %s (%.0f%%), rule %q from the %s.",
		usageRuleScopePhrase(t), usageAmount(r, value), usageAmount(r, r.Budget), usagePeriodLabel(r.Period), pct, r.Name, t.owner)
	if next := nextUsageStep(r, f.Step.At); next != nil {
		body += fmt.Sprintf(" Next step at %.0f%%: %s.", next.At, usageStepPhrase(next.Action))
	}
	body += " Prefer cheaper approaches where you can, and keep the work focused."
	return usageRuleNotice{subject: fmt.Sprintf("Usage budget: %.0f%% of %q", pct, r.Name), body: body}
}

func usageRuleWrapUpNotice(t usageRuleTarget, value, pct float64) usageRuleNotice {
	r := t.rule
	body := fmt.Sprintf("%s %s of the %s budget for %s (%.0f%%), rule %q from the %s. "+
		"Please wrap up now: stop starting new work, bring what you are doing to a safe stopping point, "+
		"write a short handoff summary (what is done, what is left, where things are) and send it to whoever assigned your work, "+
		"then stay idle until a human or your coordinator tells you to continue.",
		usageRuleScopePhrase(t), usageAmount(r, value), usageAmount(r, r.Budget), usagePeriodLabel(r.Period), pct, r.Name, t.owner)
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
