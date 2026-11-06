package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	triggerlogic "github.com/tofutools/tclaude/pkg/claude/triggers"
)

var (
	triggerTickInterval   = 2 * time.Second
	triggerCIPollInterval = 30 * time.Second
)

const triggerCIPollBatch = 20

// triggerAuthorityMu gives rule mutation/retirement and firing the same
// ordering guarantee cron uses: every side effect re-reads the rule and live
// principal while holding this lock.
var triggerAuthorityMu sync.Mutex
var managedWorkerBeforePromotionForTest func(int64)

var triggerCIWatchState = struct {
	sync.Mutex
	initialized bool
	identities  map[string]struct{}
}{identities: make(map[string]struct{})}

func startTriggerScheduler(stop <-chan struct{}) {
	go func() {
		runTriggerTick(time.Now().UTC())
		t := time.NewTicker(triggerTickInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-t.C:
				runTriggerTick(now.UTC())
			}
		}
	}()
}

func startTriggerSourcePollers(stop <-chan struct{}) {
	initializeTriggerCIWatchState()
	go func() {
		pollTriggerCITransitions(time.Now().UTC())
		t := time.NewTicker(triggerCIPollInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-t.C:
				pollTriggerCITransitions(now.UTC())
			}
		}
	}()
	go func() {
		var backoff recentlyMergedPRPollBackoff
		delay := time.Duration(0)
		for {
			if delay > 0 {
				t := time.NewTimer(delay)
				select {
				case <-stop:
					t.Stop()
					return
				case <-t.C:
				}
			}
			enabled, enabledErr := db.HasEnabledTriggerSource(db.TriggerSourcePRMerged)
			if enabledErr != nil {
				slog.Warn("triggers: inspect merged PR watchers", "error", enabledErr)
				delay = recentlyMergedPRPollInterval
				continue
			}
			if !enabled {
				delay = recentlyMergedPRPollInterval
				continue
			}
			attempted, err := pollRecentlyMergedPRs()
			var warn bool
			delay, warn, _, _ = backoff.next(attempted, err)
			if warn {
				slog.Warn("triggers: merged PR poll failed", "error", err)
			}
		}
	}()
}

type triggerCIIdentityWatch struct {
	key           string
	owner         db.AgentPR
	presentations []db.AgentPR
	rebaseline    bool
}

func triggerCIIdentityWatches(prs []db.AgentPR) []triggerCIIdentityWatch {
	byKey := make(map[string]int)
	var out []triggerCIIdentityWatch
	for _, pr := range prs {
		key := prStateKey(pr.PRURL)
		if key == "" {
			key = strings.ToLower(strings.TrimSpace(pr.PRURL))
		}
		index, found := byKey[key]
		if !found {
			index = len(out)
			byKey[key] = index
			out = append(out, triggerCIIdentityWatch{key: key, owner: pr})
		}
		out[index].presentations = append(out[index].presentations, pr)
		if pr.ID < out[index].owner.ID {
			out[index].owner = pr
		}
	}
	return out
}

func initializeTriggerCIWatchState() {
	triggerCIWatchState.Lock()
	defer triggerCIWatchState.Unlock()
	triggerCIWatchState.identities = make(map[string]struct{})
	prs, err := db.ListTriggerCIWatchPRs(0)
	if err != nil {
		triggerCIWatchState.initialized = false
		return
	}
	for _, watch := range triggerCIIdentityWatches(prs) {
		triggerCIWatchState.identities[watch.key] = struct{}{}
	}
	triggerCIWatchState.initialized = true
}

func setTriggerCIWatchSet(watches []triggerCIIdentityWatch) []triggerCIIdentityWatch {
	triggerCIWatchState.Lock()
	defer triggerCIWatchState.Unlock()
	previous := triggerCIWatchState.identities
	if !triggerCIWatchState.initialized {
		previous = map[string]struct{}{}
	}
	current := make(map[string]struct{}, len(watches))
	for i := range watches {
		_, existed := previous[watches[i].key]
		watches[i].rebaseline = !existed
		if existed {
			current[watches[i].key] = struct{}{}
		}
	}
	triggerCIWatchState.identities = current
	triggerCIWatchState.initialized = true
	return watches
}

func markTriggerCIIdentityWatched(key string) {
	triggerCIWatchState.Lock()
	triggerCIWatchState.identities[key] = struct{}{}
	triggerCIWatchState.Unlock()
}

func pollTriggerCITransitions(now time.Time) {
	prs, err := db.ListTriggerCIWatchPRs(0)
	if err != nil {
		slog.Warn("triggers: list CI-watched PRs", "error", err)
		return
	}
	watches := setTriggerCIWatchSet(triggerCIIdentityWatches(prs))
	if len(watches) > triggerCIPollBatch {
		watches = watches[:triggerCIPollBatch]
	}
	for _, watch := range watches {
		pr := watch.owner
		key := prChecksCacheKey(pr.PRURL)
		if _, busy := prChecksInflight.LoadOrStore(key, struct{}{}); busy {
			continue
		}
		info, ok := prChecksResolver(pr.PRURL)
		prChecksInflight.Delete(key)
		if !ok {
			// A failed refresh is unknown. Never reinterpret an older cached
			// green/red summary as the current state.
			for _, presented := range watch.presentations {
				if err := db.MarkTriggerCIPollAttempt(presented.ID, now); err != nil {
					slog.Warn("triggers: record failed CI poll attempt", "error", err, "url", presented.PRURL)
				}
			}
			continue
		}
		info.FetchedAt = now
		info.Summary = resolvedChecksSummary(info, now)
		savePRChecks(pr.PRURL, info)
		state := strings.ToLower(strings.TrimSpace(info.PRState))
		if state != "" {
			for _, presented := range watch.presentations {
				update := db.UpdateAgentPRStateQuiet
				if presented.ID == pr.ID {
					update = db.UpdateAgentPRState
				}
				if _, err := update(presented.AgentID, presented.PRURL, state); err != nil {
					slog.Warn("triggers: apply fresh PR state", "error", err, "url", presented.PRURL, "state", state)
				}
			}
		}
		if isTerminalPresentedPRState(state) {
			for _, presented := range watch.presentations {
				_ = db.MarkTriggerCIPollAttempt(presented.ID, now)
			}
			continue
		}
		if watch.rebaseline {
			err = db.BaselineTriggerPRCI(pr.ID, info.Summary.State, now)
			if err == nil {
				markTriggerCIIdentityWatched(watch.key)
			}
		} else {
			_, err = db.ObserveTriggerPRCI(pr.ID, info.Summary.State, now)
		}
		if err != nil {
			slog.Warn("triggers: record CI transition", "error", err, "url", pr.PRURL)
		}
		for _, presented := range watch.presentations {
			if presented.ID != pr.ID {
				_ = db.MarkTriggerCIPollAttempt(presented.ID, now)
			}
		}
	}
}

// PollTriggerCITransitionsForTest synchronously runs one bounded watched-PR poll.
func PollTriggerCITransitionsForTest(now time.Time) { pollTriggerCITransitions(now) }

func runTriggerTick(now time.Time) {
	if _, err := db.InterruptRunningTriggerFirings(now); err != nil {
		slog.Warn("triggers: close interrupted firings", "error", err)
		return
	}
	if _, err := db.ReconcileTriggerPREvents(); err != nil {
		slog.Warn("triggers: reconcile PR events", "error", err)
		return
	}
	evaluateTriggerDwell(now)
	events, err := db.ListPendingTriggerPREvents(100)
	if err != nil {
		slog.Warn("triggers: list pending PR events", "error", err)
		return
	}
	for _, event := range events {
		processTriggerPREvent(event, now)
	}
	reconcileTriggerWorkers(now)
}

type triggerFactObservation struct {
	result   string
	detail   string
	harness  string
	observed time.Time
	since    time.Time
}

func evaluateTriggerDwell(now time.Time) {
	allRules, err := db.ListTriggerRules()
	if err != nil {
		slog.Warn("triggers: list dwell rules", "error", err)
		return
	}
	var rules []*db.TriggerRule
	for _, rule := range allRules {
		if rule.Enabled && db.IsTriggerStateSource(rule.Source) {
			rules = append(rules, rule)
		}
	}
	// This runs every scheduler tick; without a dwell rule there is nothing to
	// observe, so skip the agent, session and tmux reads entirely.
	if len(rules) == 0 {
		return
	}
	agents, err := db.ListActiveAgents()
	if err != nil {
		slog.Warn("triggers: list dwell targets", "error", err)
		return
	}
	convIDs := make([]string, 0, len(agents))
	for _, agent := range agents {
		convIDs = append(convIDs, agent.CurrentConvID)
	}
	// Only the active agents' current conversations, not every retained row.
	byConv, sessionsErr := db.FindSessionsByConvIDs(convIDs)
	alive, aliveErr := cachedLiveTmuxSessions()
	for _, rule := range rules {
		seen := make(map[string]struct{})
		for _, agent := range agents {
			if !triggerDwellAgentInScope(rule, agent.AgentID) {
				continue
			}
			seen[agent.AgentID] = struct{}{}
			observation := observeTriggerAgentFact(rule.Source, agent, byConv[agent.CurrentConvID], alive,
				sessionsErr, aliveErr, now)
			previous, err := db.GetTriggerDwellState(rule.ID, agent.AgentID)
			if err != nil {
				slog.Warn("triggers: read dwell state", "rule", rule.ID, "agent", agent.AgentID, "error", err)
				continue
			}
			var prior *triggerlogic.DwellState
			if previous != nil {
				prior = &triggerlogic.DwellState{RuleRevision: previous.RuleRevision, Episode: previous.Episode,
					Result: previous.Result, TrueSince: previous.TrueSince, FiredAt: previous.FiredAt}
			}
			plan := triggerlogic.PlanDwell(prior, triggerlogic.DwellInput{RuleRevision: rule.Revision,
				For: time.Duration(rule.ForSeconds) * time.Second, Result: observation.result,
				FactSince: observation.since, Now: now})
			state := db.TriggerDwellState{RuleID: rule.ID, AgentID: agent.AgentID, RuleRevision: plan.State.RuleRevision,
				Episode: plan.State.Episode, Result: plan.State.Result, Detail: observation.detail,
				Harness: observation.harness, FactObservedAt: observation.observed, TrueSince: plan.State.TrueSince,
				FiredAt: plan.State.FiredAt, UpdatedAt: now}
			if _, err := db.ApplyTriggerDwellState(rule, state, observation.detail, observation.harness,
				observation.observed, now, plan.Fire); err != nil {
				slog.Warn("triggers: apply dwell state", "rule", rule.ID, "agent", agent.AgentID, "error", err)
			}
		}
		states, err := db.ListTriggerDwellStates(rule.ID)
		if err != nil {
			slog.Warn("triggers: list prior dwell targets", "rule", rule.ID, "error", err)
			continue
		}
		for _, previous := range states {
			if _, ok := seen[previous.AgentID]; ok {
				continue
			}
			prior := triggerlogic.DwellState{RuleRevision: previous.RuleRevision, Episode: previous.Episode,
				Result: previous.Result, TrueSince: previous.TrueSince, FiredAt: previous.FiredAt}
			plan := triggerlogic.PlanDwell(&prior, triggerlogic.DwellInput{RuleRevision: rule.Revision,
				For: time.Duration(rule.ForSeconds) * time.Second, Result: triggerlogic.FactUnknown, Now: now})
			state := db.TriggerDwellState{RuleID: rule.ID, AgentID: previous.AgentID,
				RuleRevision: plan.State.RuleRevision, Episode: plan.State.Episode, Result: plan.State.Result,
				Detail: "agent is not currently active in the rule scope", Harness: previous.Harness,
				FactObservedAt: now, FiredAt: plan.State.FiredAt, UpdatedAt: now}
			if _, err := db.ApplyTriggerDwellState(rule, state, state.Detail, state.Harness, now, now, false); err != nil {
				slog.Warn("triggers: interrupt out-of-scope dwell", "rule", rule.ID, "agent", previous.AgentID, "error", err)
			}
		}
	}
}

func triggerDwellAgentInScope(rule *db.TriggerRule, agentID string) bool {
	if rule.ScopeKind == db.TriggerScopeGlobal {
		return true
	}
	groups, err := db.ListGroupsForAgent(agentID)
	if err != nil {
		return false
	}
	for _, group := range groups {
		if group.ID == rule.GroupID && !group.IsArchived() {
			return true
		}
	}
	return false
}

func observeTriggerAgentFact(source string, agent *db.Agent, rows []*db.SessionRow,
	alive map[string]struct{}, sessionsErr, aliveErr error, now time.Time) triggerFactObservation {
	unknown := func(detail string) triggerFactObservation {
		return triggerFactObservation{result: triggerlogic.FactUnknown, detail: detail, observed: now}
	}
	if sessionsErr != nil || aliveErr != nil {
		return unknown("live session observation is unavailable")
	}
	var current *db.SessionRow
	for _, row := range rows {
		if _, ok := alive[row.TmuxSession]; !ok || row.TmuxSession == "" {
			continue
		}
		if current == nil || row.UpdatedAt.After(current.UpdatedAt) {
			current = row
		}
	}
	if current == nil {
		return unknown("agent has no live observed session")
	}
	knownHarness, ok := harness.Get(current.Harness)
	if !ok {
		obs := unknown("agent harness is unknown to this tclaude build")
		obs.harness = current.Harness
		return obs
	}
	observation := triggerFactObservation{result: triggerlogic.FactFalse, harness: knownHarness.Name, observed: now}
	switch source {
	case db.TriggerSourceAgentIdle:
		observation.detail = "agent.idle uses only the live session idle status and last harness hook; group messages and pane keystrokes are excluded"
		if current.Status == "" || current.LastHook.IsZero() {
			observation.result = triggerlogic.FactUnknown
			observation.detail = "agent.idle has no usable harness-hook observation"
		} else if current.Status == session.StatusIdle {
			observation.result = triggerlogic.FactTrue
			observation.since = current.LastHook.UTC()
		}
	case db.TriggerSourceAgentAwaitingInput:
		observation.detail = "agent.awaiting_input is exact harness question-waiting state; awaiting_permission is explicitly excluded"
		if !knownHarness.SupportsAwaitingInputObservation() {
			observation.result = triggerlogic.FactUnknown
			observation.detail = "harness does not expose awaiting-input observation; awaiting_permission is explicitly excluded"
		} else {
			if knownHarness.Name == harness.CodexName {
				runtime, err := db.GetCodexAppServerRuntimeByConvID(current.ConvID)
				if err != nil || runtime == nil || runtime.State != db.CodexAppServerReady {
					observation.result = triggerlogic.FactUnknown
					observation.detail = "Codex awaiting-input observation requires a ready managed app-server; awaiting_permission is explicitly excluded"
					break
				}
			}
			switch current.Status {
			case "":
				observation.result = triggerlogic.FactUnknown
			case session.StatusAwaitingInput:
				observation.result = triggerlogic.FactTrue
			}
		}
	}
	_ = agent
	return observation
}

// RunTriggerTickForTest executes one complete reconciliation synchronously.
func RunTriggerTickForTest(now time.Time) { runTriggerTick(now) }

func processTriggerPREvent(event db.TriggerPREvent, now time.Time) {
	rules, err := db.ListTriggerRules()
	if err != nil {
		slog.Warn("triggers: list rules", "error", err)
		return
	}
	allTerminal := true
	for _, candidate := range rules {
		if db.IsTriggerStateSource(event.Source) && candidate.ID != event.OriginRuleID {
			continue
		}
		last, err := db.LatestCompletedTriggerFiring(candidate.ID)
		if err != nil {
			slog.Warn("triggers: read cooldown", "rule", candidate.ID, "error", err)
			return
		}
		var lastAt time.Time
		if last != nil {
			lastAt = last.FinishedAt
		}
		selectedAgent := event.PRAuthorAgent
		if event.AgentID != "" {
			selectedAgent = event.AgentID
		}
		spawnedByRule, err := db.RuleSpawnedAgent(candidate.ID, selectedAgent)
		if err != nil {
			slog.Warn("triggers: read loop provenance", "rule", candidate.ID, "error", err)
			return
		}
		decision := triggerlogic.Evaluate(candidate, event, now, lastAt, spawnedByRule)
		if decision.Outcome == triggerlogic.OutcomeDeferredDebounce {
			allTerminal = false
			continue
		}
		if decision.Fire {
			if !fireTriggerRule(candidate.ID, candidate.Revision, event, now) {
				allTerminal = false
			}
			continue
		}
		// Durable suppressions are firing-history facts too. Out-of-scope and
		// disabled decisions are intentionally omitted to keep the ledger useful.
		switch decision.Outcome {
		case triggerlogic.OutcomeSuppressedCooldown, triggerlogic.OutcomeSuppressedLoop,
			triggerlogic.OutcomeDraftFiltered:
			if id, inserted, insertErr := db.InsertTriggerFiring(candidate.ID, candidate.Revision,
				event.ID, event.EventRef, decision.Outcome, decision.Detail, now); insertErr != nil {
				allTerminal = false
			} else if inserted {
				if finishErr := db.FinishTriggerFiring(id, decision.Outcome, decision.Detail, now); finishErr != nil {
					allTerminal = false
				}
			}
		}
	}
	if allTerminal {
		if err := db.MarkTriggerPREventProcessed(event.ID, now); err != nil {
			slog.Warn("triggers: mark PR event processed", "event", event.ID, "error", err)
		}
	}
}

func fireTriggerRule(ruleID, expectedRevision int64, event db.TriggerPREvent, now time.Time) bool {
	triggerAuthorityMu.Lock()
	defer triggerAuthorityMu.Unlock()
	rule, err := db.GetTriggerRule(ruleID)
	if err != nil || rule == nil {
		return false
	}
	if !rule.Enabled || rule.Revision != expectedRevision {
		return false
	}
	if !rule.OperatorAuthored {
		conv, err := db.CurrentConvForAgent(rule.OwnerAgent)
		if err != nil || conv == "" {
			return recordTriggerDenial(rule, event, now, "owner unavailable")
		}
		rule.OwnerAgent = strings.TrimSpace(rule.OwnerAgent)
	}
	firingID, inserted, err := db.InsertTriggerFiring(rule.ID, rule.Revision, event.ID,
		event.EventRef, "running", "", now)
	if err != nil || !inserted {
		return err == nil
	}
	overall := "ok"
	var details []string
	for i, actionSpec := range rule.Actions {
		outcome := executeTriggerAction(rule, firingID, i, actionSpec, event, now)
		if err := db.InsertTriggerActionOutcome(&outcome); err != nil {
			slog.Warn("triggers: record action outcome", "firing", firingID, "action", i, "error", err)
			_ = db.FinishTriggerFiring(firingID, "interrupted", "action completed but its outcome could not be recorded", time.Now().UTC())
			_ = db.MarkTriggerPREventInterrupted(event.ID, time.Now().UTC())
			return true
		}
		if outcome.Outcome != "ok" && outcome.Outcome != "spawned" && outcome.Outcome != "queued" {
			overall = "partial_failure"
			details = append(details, fmt.Sprintf("action %d: %s", i, outcome.Detail))
		}
	}
	return db.FinishTriggerFiring(firingID, overall, strings.Join(details, "; "), time.Now().UTC()) == nil
}

func recordTriggerDenial(rule *db.TriggerRule, event db.TriggerPREvent, now time.Time, detail string) bool {
	id, inserted, err := db.InsertTriggerFiring(rule.ID, rule.Revision, event.ID, event.EventRef,
		"permission_denied", detail, now)
	if err != nil {
		return false
	}
	if !inserted {
		return true
	}
	return db.FinishTriggerFiring(id, "permission_denied", detail, now) == nil
}

func executeTriggerAction(rule *db.TriggerRule, firingID int64, index int, spec db.TriggerAction, event db.TriggerPREvent, now time.Time) db.TriggerActionOutcome {
	o := db.TriggerActionOutcome{FiringID: firingID, ActionIndex: index, ActionType: spec.Type, CreatedAt: time.Now().UTC()}
	switch spec.Type {
	case db.TriggerActionSpawn:
		o.Outcome, o.Detail, o.SpawnedAgent = executeTriggerSpawn(rule, firingID, index, spec.Spawn, event, now)
	case db.TriggerActionMessage:
		o.Outcome, o.Detail, o.MessageID = executeTriggerMessage(rule, spec.Message, event)
	default:
		o.Outcome, o.Detail = "invalid_action", "unknown action type"
	}
	return o
}

func triggerOwnerConv(rule *db.TriggerRule) (string, error) {
	if rule.OperatorAuthored {
		return "", nil
	}
	owner, err := db.GetAgent(rule.OwnerAgent)
	if err != nil {
		return "", err
	}
	if !owner.Active() {
		return "", errors.New("owning agent is retired or unavailable")
	}
	conv, err := db.CurrentConvForAgent(rule.OwnerAgent)
	if err != nil {
		return "", err
	}
	if conv == "" {
		return "", errors.New("owning agent has no current conversation")
	}
	if rule.ScopeKind == db.TriggerScopeGlobal {
		allowed, authErr := durableOwnerAllows("trigger", conv, PermTriggersManage, ActionContext{})
		if authErr != nil {
			return "", authErr
		}
		if !allowed {
			return "", fmt.Errorf("owner lacks %s", PermTriggersManage)
		}
		return conv, nil
	}
	g, err := db.GetAgentGroupByID(rule.GroupID)
	if err != nil {
		return "", err
	}
	if g == nil || g.IsArchived() {
		return "", errors.New("trigger group is unavailable")
	}
	allowed, authErr := durableOwnerAllows("trigger", conv, PermGroupsTriggersManage,
		ActionContext{Group: g.Name, structuralGroup: g.Name})
	if authErr != nil {
		return "", authErr
	}
	if !allowed {
		return "", fmt.Errorf("owner lacks %s for group %s", PermGroupsTriggersManage, g.Name)
	}
	return conv, nil
}

func triggerActionGroup(rule *db.TriggerRule, event db.TriggerPREvent) (*db.AgentGroup, error) {
	groupID := rule.GroupID
	if groupID == 0 && len(event.GroupIDs) > 0 {
		groupID = event.GroupIDs[0]
	}
	if groupID == 0 {
		return nil, errors.New("spawn/group message needs a group in the event scope")
	}
	g, err := db.GetAgentGroupByID(groupID)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, errors.New("target group no longer exists")
	}
	if g.IsArchived() {
		return nil, errors.New("target group is archived")
	}
	return g, nil
}

func executeTriggerSpawn(rule *db.TriggerRule, firingID int64, index int, spec *db.TriggerSpawnAction, event db.TriggerPREvent, now time.Time) (string, string, string) {
	return executeManagedSpawn(rule, index, spec, event, now, managedWorkerSource{
		RuleID: rule.ID, FiringID: firingID, RatePrincipal: fmt.Sprintf("trigger:%d", rule.ID),
		Tag: fmt.Sprintf("trigger:%d", rule.ID),
	})
}

// managedSpawn is a fully authorized, not-yet-launched managed spawn: the
// launch group and parameters every policy check below has already approved.
type managedSpawn struct {
	group  *db.AgentGroup
	params spawnParams
	// promptRefs are the saved prompts the brief opens with (the profile's,
	// else the group's defaults). Kept so a caller that replaces the brief
	// after planning recomposes it rather than dropping them.
	promptRefs []string
	// notes disclose the identity and access decisions a tier other than the
	// rule made, such as a default profile's grant the owner could not confer.
	notes []string
}

// setInitialMessage replaces the planned brief, composing the plan's saved
// prompts in front of it as planManagedSpawn did.
func (m *managedSpawn) setInitialMessage(body string) *spawnFailure {
	if len(m.promptRefs) > 0 {
		composed, fail := composeSavedPrompts(m.promptRefs, body)
		if fail != nil {
			return fail
		}
		body = composed
	}
	m.params.InitialMessage = body
	return nil
}

// planManagedSpawn performs every authorization, guardrail, rate, role, cwd,
// and sandbox check of a daemon-initiated spawn with profileName into g on
// behalf of ownerConv ("" for an operator-authored principal, which is then
// rate-limited as ratePrincipal). It launches nothing: the caller reserves its
// own tracking record and then calls executeSpawn(plan.group, plan.params).
//
// capacity, when set, is the caller's own live-worker bound, evaluated after
// the owner's permission is established and before any rate slot is taken. A
// non-empty status refuses the spawn with that status and detail.
//
// On refusal it returns a nil plan with a machine status and a human detail.
func planManagedSpawn(g *db.AgentGroup, profileName string, roleRefs []string, ownerConv, ratePrincipal,
	name, initialMessage string, capacity func() (status, detail string)) (*managedSpawn, string, string) {
	profile, err := db.ResolveSpawnProfile(profileName)
	if err != nil {
		return nil, "io", err.Error()
	}
	if profile == nil {
		return nil, "profile_not_found", fmt.Sprintf("spawn profile %q does not exist", profileName)
	}
	if fail := managedProfileRefusal(g, profile, ownerConv, false); fail != nil {
		return nil, fail.Kind, fail.Msg
	}
	allowAnyGroup := false
	// A managed worker never selects a sandbox profile of its own, so the
	// sandbox_profile dimension is described by the ambient assignment it will
	// inherit. Without that this whole path is unreachable for an owner whose
	// spawn grant carries a sandbox_profile scope: the dimension would be
	// undescribed and fail closed on every firing.
	sandboxProfileForScope := ambientSandboxProfileName(g)
	sandboxScopePinned := false
	if ownerConv != "" {
		req, _ := http.NewRequest(http.MethodPost, "http://trigger.invalid", nil)
		// Metered only so a limited grant is seen and refused by name below.
		req, _ = withSudoUseMeter(req)
		ctx := ActionContext{Group: g.Name, SpawnProfile: profile.Name,
			SandboxProfile: sandboxProfileForScope, structuralGroup: g.Name}
		allowed, authorizedSlug, _, authErr := spawnPermissionAllowsAction(req, ownerConv, ctx)
		if authErr != nil {
			return nil, "io", authErr.Error()
		}
		if allowed && spawnAuthorityIsCounted(req, ownerConv, authorizedSlug, ctx) {
			return nil, "permission_denied", "a trigger cannot spend a use-limited sudo grant"
		}
		if !allowed {
			return nil, "permission_denied", fmt.Sprintf("owner lacks %s or %s for group %s and spawn profile %s", PermAgentSpawn, PermGroupsMembersSpawn, g.Name, profile.Name)
		}
		allowAnyGroup = authorizedSlug == PermAgentSpawn
		sandboxScopePinned = scopePinsDimension(req, ownerConv, authorizedSlug,
			ctx, ScopeDimSandboxProfile)
	}
	if capacity != nil {
		if status, detail := capacity(); status != "" {
			return nil, status, detail
		}
	}
	// The default tiers too, before a rate slot is spent on a refusal that
	// repeats on every firing.
	if fail := managedProfileRefusal(g, nil, ownerConv, true); fail != nil {
		return nil, fail.Kind, fail.Msg
	}
	if fail := checkSpawnGuardrails(g, ownerConv, allowAnyGroup); fail != nil {
		return nil, "guardrail_denied", fail.Msg
	}
	rateFail := claimSpawnRateSlot(ownerConv)
	if ownerConv == "" {
		rateFail = claimDaemonSpawnRateSlot(ratePrincipal)
	}
	if rateFail != nil {
		return nil, "rate_limited", rateFail.Msg
	}

	// Roles, role label, description, saved prompts, startup context, owner
	// flag and permission overrides resolve over the worker's profile, the
	// group's default and the global default, with the owner's authority
	// checked exactly as a direct spawn checks its caller's
	// (resolveLaunchIdentity). The rule's own roles are the explicit tier.
	var request agent.SpawnRequest
	if len(roleRefs) > 0 {
		request.StateRoleRefs(roleRefs)
	}
	tiers := defaultLaunchTiers(g, profile, profileSource(profile, agent.ProvCLIProfileSource))
	identity, fail := resolveLaunchIdentity(launchIdentityRequest{body: request, group: g,
		harness: harnessOrDefault(profile.Harness), spawner: ownerConv, tiers: tiers})
	if fail != nil {
		return nil, managedIdentityFailureKind(fail.Kind), fail.Msg
	}
	promptRefs := identity.promptRefs
	if len(promptRefs) > 0 {
		composed, fail := composeSavedPrompts(promptRefs, initialMessage)
		if fail != nil {
			return nil, fail.Kind, fail.Msg
		}
		initialMessage = composed
	}
	cwd := strings.TrimSpace(g.DefaultCwd)
	if cwd == "" && ownerConv != "" {
		if s, _ := db.FindSessionByConvID(ownerConv); s != nil {
			cwd = s.Cwd
		}
	}
	if cwd == "" {
		return nil, "invalid_cwd", "target group and owning principal provide no launch directory"
	}
	if info, statErr := os.Stat(cwd); statErr != nil || !info.IsDir() {
		return nil, "invalid_cwd", fmt.Sprintf("launch directory %q is unavailable", cwd)
	}

	p := spawnParams{UsageSpawnProfile: profile, managedProfile: profile, spawnCopyCallerTiers: spawnCopyTiers(spawnCopyTier(db.SpawnCopyTierNamed, profile)), Name: name, Role: identity.role, Descr: identity.descr, InitialMessage: initialMessage,
		ProfileContext: identity.startupContext, Cwd: cwd, ReplyToConv: ownerConv, SpawnedByConv: ownerConv,
		IsOwner: identity.isOwner, PermissionOverrides: identity.overrides, PermissionSources: identity.overrideSources, Async: true,
		LaunchNotes: identity.launchNotes(),
		// The worker's profile is the named tier, above the group's and the
		// global default profile, for every launch field: identity above, the
		// rest in applyDefaultProfile. With no tier speaking, the group context
		// is included, as it always was for these workers.
		IncludeGroupContextDefault: true,
		// The default profiles identity resolved from, so the launch fields
		// resolve from the same versions.
		defaultProfiles: &pinnedDefaultProfiles{group: tiers[1].profile, global: tiers[2].profile}}
	if fail := resolveManagedLaunch(g, &p, ownerConv, false); fail != nil {
		if fail.Kind == "io" || fail.Kind == "permission_denied" || isManagedProfileRefusal(fail.Kind) {
			return nil, fail.Kind, fail.Msg
		}
		// The owner's sandbox refused the worker as it would refuse a child
		// it spawned: a refusal of the owner's authority, in the outcome
		// vocabulary a managed worker's run records.
		if fail.Kind == "sandbox_profile_restricted" {
			return nil, "permission_denied", fail.Msg
		}
		return nil, "spawn_failed", fail.Msg
	}
	// The owner's grant was conditioned on the inherited sandbox profile, so
	// refuse when the spawn profile resolves a launch that would not enforce
	// it. Only a scope-pinned owner is bound: an unscoped grant or group
	// ownership never traded on the profile, and a daemon-owned firing
	// (ownerConv == "") answered to no grant at all.
	if sandboxScopePinned &&
		sandboxProfileTierInert(p.Harness, p.HarnessBuiltinMode, p.SandboxImplementation) {
		return nil, "permission_denied", fmt.Sprintf(
			"this firing is authorized by a grant scoped to sandbox profile %s, and its launch (spawn profile %s over the group's and the global default profiles) resolves a mode that would not enforce it",
			sandboxProfileForScope, profile.Name)
	}
	return &managedSpawn{group: g, params: p, promptRefs: promptRefs, notes: identity.notes}, "", ""
}

// managedIdentityFailureKind maps a refused identity resolution onto the
// outcome kinds a managed worker's run records.
func managedIdentityFailureKind(kind string) string {
	switch kind {
	case "io", "invalid_role":
		return kind
	case "role_scope_conflict":
		return "invalid_role"
	case "forbidden", "scope_not_attenuated":
		return "permission_denied"
	}
	return "spawn_failed"
}

type managedWorkerSource struct {
	RuleID        int64
	FiringID      int64
	CronJobID     int64
	CronRunID     int64
	RatePrincipal string
	Tag           string
	OwnerConv     string
}

func executeManagedSpawn(rule *db.TriggerRule, index int, spec *db.TriggerSpawnAction, event db.TriggerPREvent, now time.Time, source managedWorkerSource) (string, string, string) {
	if spec == nil {
		return "invalid_action", "missing spawn payload", ""
	}
	g, err := triggerActionGroup(rule, event)
	if err != nil {
		return "target_invalid", err.Error(), ""
	}
	ownerConv := source.OwnerConv
	if source.CronJobID == 0 {
		var err error
		ownerConv, err = triggerOwnerConv(rule)
		if err != nil {
			return "permission_denied", err.Error(), ""
		}
	}
	name := triggerlogic.RenderTemplate(spec.NameTemplate, event, g.Name)
	if strings.TrimSpace(name) == "" {
		if event.AgentID != "" {
			name = fmt.Sprintf("trigger-%d-agent", rule.ID)
		} else {
			name = fmt.Sprintf("trigger-%d-pr-%d", rule.ID, event.PRNumber)
		}
	}
	plan, status, detail := planManagedSpawn(g, spec.Profile, spec.RoleRefs, ownerConv, source.RatePrincipal,
		name, triggerlogic.RenderTemplate(spec.InstructionTemplate, event, g.Name), func() (string, string) {
			if n, err := db.CountLiveManagedWorkers(source.RuleID, source.CronJobID, index); err != nil {
				return "io", err.Error()
			} else if n >= spec.MaxLiveWorkers {
				return "max_live_workers", fmt.Sprintf("rule already has %d live workers (max %d)", n, spec.MaxLiveWorkers)
			}
			return "", ""
		})
	if plan == nil {
		announceManagedProfileRefusal(managedRefusalSource{Key: source.Tag,
			Label: managedSourceLabel(rule, source), OwnerConv: ownerConv}, status, detail)
		return status, detail, ""
	}
	profileGroup, p := plan.group, plan.params
	p.AgentID = db.NewAgentID()
	worker := &db.TriggerWorker{RuleID: source.RuleID, FiringID: source.FiringID,
		CronJobID: source.CronJobID, CronRunID: source.CronRunID,
		ActionIndex: index, AgentID: p.AgentID, State: "reserved", CreatedAt: now}
	if spec.WorkerDeadlineSeconds > 0 {
		worker.DeadlineAt = now.Add(time.Duration(spec.WorkerDeadlineSeconds) * time.Second)
	}
	workerID, err := db.InsertTriggerWorker(worker)
	if err != nil {
		return "spawn_failed", "reserve trigger worker: " + err.Error(), ""
	}
	out, fail := executeSpawn(profileGroup, p)
	if fail != nil {
		_ = db.CompleteTriggerWorker(workerID, "failed", fail.Msg, time.Now().UTC())
		return "spawn_failed", fail.Msg, ""
	}
	agentID := out.AgentID
	if agentID == "" && out.ConvID != "" {
		agentID, _ = db.AgentIDForConv(out.ConvID)
	}
	if agentID == "" {
		_ = db.CompleteTriggerWorker(workerID, "failed", "spawn returned no stable agent identity", time.Now().UTC())
		return "spawn_failed", "spawn returned no stable agent identity", ""
	}
	if agentID != p.AgentID {
		_ = db.CompleteTriggerWorker(workerID, "failed", "spawn returned a different stable agent identity", time.Now().UTC())
		return "spawned_tracking_failed", "spawn returned a different stable agent identity", agentID
	}
	if managedWorkerBeforePromotionForTest != nil {
		managedWorkerBeforePromotionForTest(workerID)
	}
	promoted, err := db.MarkTriggerWorkerDispatched(workerID, out.ConvID, out.Label)
	if err != nil {
		return "spawned_tracking_pending", err.Error(), agentID
	}
	if !promoted {
		conv := strings.TrimSpace(out.ConvID)
		if conv == "" {
			conv, _ = db.CurrentConvForAgent(agentID)
		}
		if conv != "" {
			_ = stopOneConv(conv, false)
		}
		return "spawned_tracking_failed", "worker reservation ended while spawn dispatch was in progress; launched worker was stopped best-effort", agentID
	}
	_ = db.AddAgentTags(agentID, source.Tag)
	rearmManagedProfileRefusal(source.Tag)
	return "spawned", strings.Join(plan.notes, "; "), agentID
}

// managedSourceLabel names a managed worker's automation for a notice.
func managedSourceLabel(rule *db.TriggerRule, source managedWorkerSource) string {
	if source.CronJobID != 0 {
		return fmt.Sprintf("schedule %q", rule.Name)
	}
	return fmt.Sprintf("trigger %q", rule.Name)
}

// SetManagedWorkerBeforePromotionForTest installs a deterministic seam after
// launch and before the reserved worker CAS is promoted to pending/live.
func SetManagedWorkerBeforePromotionForTest(fn func(int64)) func() {
	old := managedWorkerBeforePromotionForTest
	managedWorkerBeforePromotionForTest = fn
	return func() { managedWorkerBeforePromotionForTest = old }
}

func executeTriggerMessage(rule *db.TriggerRule, spec *db.TriggerMessageAction, event db.TriggerPREvent) (string, string, int64) {
	if spec == nil {
		return "invalid_action", "missing message payload", 0
	}
	ownerConv, err := triggerOwnerConv(rule)
	if err != nil {
		return "permission_denied", err.Error(), 0
	}
	groupName := ""
	var targetConv string
	var groupID int64
	if spec.Target == "group" {
		g, gErr := triggerActionGroup(rule, event)
		if gErr != nil {
			return "target_invalid", gErr.Error(), 0
		}
		groupName = g.Name
		groupID = g.ID
		// The bounded group message action addresses the selected agent while stamping
		// the rule's group route; later slices may add explicit multicast.
	}
	targetAgent := event.PRAuthorAgent
	if spec.Target == "agent" || targetAgent == "" {
		targetAgent = event.AgentID
	}
	targetConv, err = db.CurrentConvForAgent(targetAgent)
	if err != nil || targetConv == "" {
		return "target_invalid", "selected agent has no current conversation", 0
	}
	if !rule.OperatorAuthored {
		via, _, routeErr := db.CanSenderReachTarget(ownerConv, targetConv)
		if routeErr != nil {
			return "io", routeErr.Error(), 0
		}
		if via != nil {
			groupID = via.ID
			groupName = via.Name
		} else if resolvePermission(ownerConv, PermMessageDirect) != permAllow {
			return "permission_denied", "owner cannot reach selected agent and lacks " + PermMessageDirect, 0
		}
	}
	subject := triggerlogic.RenderTemplate(spec.SubjectTemplate, event, groupName)
	if strings.TrimSpace(subject) == "" {
		subject = "[trigger:" + rule.Name + "] " + event.Source
	}
	body := triggerlogic.RenderTemplate(spec.BodyTemplate, event, groupName)
	id, err := db.InsertAgentMessage(&db.AgentMessage{GroupID: groupID, FromConv: ownerConv, ToConv: targetConv, Subject: subject, Body: body, ToRecipients: []string{targetConv}, OperatorAuthored: rule.OperatorAuthored})
	if err != nil {
		return "queue_failed", err.Error(), 0
	}
	maybeFlushUndelivered(targetConv)
	return "queued", "", id
}

func reconcileTriggerWorkers(now time.Time) {
	workers, err := db.ListActiveTriggerWorkers()
	if err != nil {
		slog.Warn("triggers: list workers", "error", err)
		return
	}
	for _, w := range workers {
		conv, _ := db.CurrentConvForAgent(w.AgentID)
		if conv == "" {
			conv = w.ConvID
		}
		if !w.DeadlineAt.IsZero() && !now.Before(w.DeadlineAt) {
			if conv != "" {
				_ = stopOneConv(conv, false)
			}
			_ = db.CompleteTriggerWorker(w.ID, "deadline_exceeded", "worker deadline elapsed", now)
			if w.CronRunID > 0 {
				_ = db.FinishAgentCronRun(w.CronRunID, "deadline_exceeded", "worker deadline elapsed", w.ID, w.AgentID)
			}
			continue
		}
		sess, _ := db.FindSessionByConvID(conv)
		if sess != nil && (sess.Status == session.StatusExited || sess.Status == session.StatusError) {
			_ = db.CompleteTriggerWorker(w.ID, "exited", sess.Status, now)
			if w.CronRunID > 0 {
				_ = db.FinishAgentCronRun(w.CronRunID, "exited", sess.Status, w.ID, w.AgentID)
			}
		}
	}
}
