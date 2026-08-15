package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
	triggerlogic "github.com/tofutools/tclaude/pkg/claude/triggers"
)

var triggerTickInterval = 2 * time.Second

// triggerAuthorityMu gives rule mutation/retirement and firing the same
// ordering guarantee cron uses: every side effect re-reads the rule and live
// principal while holding this lock.
var triggerAuthorityMu sync.Mutex

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

func runTriggerTick(now time.Time) {
	if !triggerRoutesEnabled() {
		return
	}
	if _, err := db.InterruptRunningTriggerFirings(now); err != nil {
		slog.Warn("triggers: close interrupted firings", "error", err)
		return
	}
	if _, err := db.ReconcileTriggerPREvents(); err != nil {
		slog.Warn("triggers: reconcile PR events", "error", err)
		return
	}
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
		last, err := db.LatestCompletedTriggerFiring(candidate.ID)
		if err != nil {
			slog.Warn("triggers: read cooldown", "rule", candidate.ID, "error", err)
			return
		}
		var lastAt time.Time
		if last != nil {
			lastAt = last.FinishedAt
		}
		spawnedByRule, err := db.RuleSpawnedAgent(candidate.ID, event.PRAuthorAgent)
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
	req, _ := http.NewRequest(http.MethodPost, "http://trigger.invalid", nil)
	if rule.ScopeKind == db.TriggerScopeGlobal {
		allowed, _, authErr := permissionAllowsAction(req, conv, PermTriggersManage, ActionContext{})
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
	ctx := ActionContext{Group: g.Name, structuralGroup: g.Name}
	allowed, _, authErr := permissionAllowsAction(req, conv, PermGroupsTriggersManage, ctx)
	if authErr != nil {
		return "", authErr
	}
	if !allowed && resolvePermissionVerdictForRequest(req, conv, PermGroupsTriggersManage).Resolution != permDeny {
		allowed, _ = structuralPermissionMatch(conv, PermGroupsTriggersManage, ctx)
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
	if spec == nil {
		return "invalid_action", "missing spawn payload", ""
	}
	g, err := triggerActionGroup(rule, event)
	if err != nil {
		return "target_invalid", err.Error(), ""
	}
	ownerConv, err := triggerOwnerConv(rule)
	if err != nil {
		return "permission_denied", err.Error(), ""
	}
	profile, err := db.ResolveSpawnProfile(spec.Profile)
	if err != nil {
		return "io", err.Error(), ""
	}
	if profile == nil {
		return "profile_not_found", fmt.Sprintf("spawn profile %q does not exist", spec.Profile), ""
	}
	if fail := profileSpawnFailure(profile, ownerConv); fail != nil {
		return "permission_denied", fail.Msg, ""
	}
	if ownerConv != "" {
		req, _ := http.NewRequest(http.MethodPost, "http://trigger.invalid", nil)
		ctx := ActionContext{Group: g.Name, SpawnProfile: profile.Name, structuralGroup: g.Name}
		allowed, _, authErr := permissionAllowsAction(req, ownerConv, PermGroupsMembersSpawn, ctx)
		if authErr != nil {
			return "io", authErr.Error(), ""
		}
		if !allowed && resolvePermissionVerdictForRequest(req, ownerConv, PermGroupsMembersSpawn).Resolution != permDeny {
			allowed, _ = structuralPermissionMatch(ownerConv, PermGroupsMembersSpawn, ctx)
		}
		if !allowed {
			return "permission_denied", fmt.Sprintf("owner lacks %s for group %s and spawn profile %s", PermGroupsMembersSpawn, g.Name, profile.Name), ""
		}
	}
	if n, err := db.CountLiveTriggerWorkers(rule.ID, index); err != nil {
		return "io", err.Error(), ""
	} else if n >= spec.MaxLiveWorkers {
		return "max_live_workers", fmt.Sprintf("rule already has %d live workers (max %d)", n, spec.MaxLiveWorkers), ""
	}
	recorder := httptest.NewRecorder()
	if !checkSpawnGuardrails(recorder, g, ownerConv) {
		return "guardrail_denied", strings.TrimSpace(recorder.Body.String()), ""
	}
	claimed := claimSpawnRateSlot(recorder, ownerConv)
	if ownerConv == "" {
		claimed = claimDaemonSpawnRateSlot(recorder, fmt.Sprintf("trigger:%d", rule.ID))
	}
	if !claimed {
		return "rate_limited", strings.TrimSpace(recorder.Body.String()), ""
	}

	roles := spec.RoleRefs
	if len(roles) == 0 {
		roles = profile.RoleRefs
	}
	profileContext := profile.StartupContext
	overrides := map[string]db.PermissionOverride{}
	for slug, v := range profile.PermissionOverrides {
		overrides[slug] = v
	}
	roleLabel := profile.Role
	for _, ref := range roles {
		role, roleErr := db.GetRole(ref)
		if roleErr != nil {
			return "io", roleErr.Error(), ""
		}
		if role == nil {
			return "invalid_role", fmt.Sprintf("role %q does not exist", ref), ""
		}
		if roleLabel == "" {
			roleLabel = role.Name
		}
		profileContext = appendRoleBlock(profileContext, role.Brief)
		for _, grant := range role.Permissions {
			candidate := db.PermissionOverride{Effect: db.PermEffectGrant, Scope: grant.Scope}
			if prior, ok := overrides[grant.Slug]; ok && prior != candidate {
				return "invalid_role", "roles grant incompatible scopes for " + grant.Slug, ""
			}
			overrides[grant.Slug] = candidate
		}
	}
	if ownerConv != "" && len(overrides) > 0 {
		if resolvePermission(ownerConv, PermPermissionsGrant) != permAllow {
			return "permission_denied", "spawn profile/roles confer permissions but owner lacks " + PermPermissionsGrant, ""
		}
		if err := checkGrantAttenuation(ownerConv, grantConferee{descendantByConstruction: true}, conferredGrantsFromOverrides(overrides)); err != nil {
			return "permission_denied", err.Error(), ""
		}
	}
	isOwner := profile.IsOwner != nil && *profile.IsOwner
	if ownerConv != "" && isOwner {
		req, _ := http.NewRequest(http.MethodPost, "http://trigger.invalid", nil)
		ctx := ActionContext{Group: g.Name, structuralGroup: g.Name}
		allowed, _, _ := permissionAllowsAction(req, ownerConv, PermGroupsOwnersManage, ctx)
		if !allowed && resolvePermissionVerdictForRequest(req, ownerConv, PermGroupsOwnersManage).Resolution != permDeny {
			allowed, _ = structuralPermissionMatch(ownerConv, PermGroupsOwnersManage, ctx)
		}
		if !allowed {
			return "permission_denied", "spawn profile confers group ownership but owner lacks " + PermGroupsOwnersManage, ""
		}
	}
	cwd := strings.TrimSpace(g.DefaultCwd)
	if cwd == "" && ownerConv != "" {
		if s, _ := db.FindSessionByConvID(ownerConv); s != nil {
			cwd = s.Cwd
		}
	}
	if cwd == "" {
		return "invalid_cwd", "target group and owning principal provide no launch directory", ""
	}
	if info, statErr := os.Stat(cwd); statErr != nil || !info.IsDir() {
		return "invalid_cwd", fmt.Sprintf("launch directory %q is unavailable", cwd), ""
	}

	name := triggerlogic.RenderTemplate(spec.NameTemplate, event, g.Name)
	if strings.TrimSpace(name) == "" {
		name = fmt.Sprintf("trigger-%d-pr-%d", rule.ID, event.PRNumber)
	}
	p := spawnParams{Name: name, Role: roleLabel, Descr: profile.Descr, InitialMessage: triggerlogic.RenderTemplate(spec.InstructionTemplate, event, g.Name),
		ProfileContext: profileContext, Cwd: cwd, ReplyToConv: ownerConv, SpawnedByConv: ownerConv, IsOwner: isOwner, PermissionOverrides: overrides, Async: true}
	includeGroup := profile.IncludeGroupDefaultContext == nil || *profile.IncludeGroupDefaultContext
	if includeGroup {
		p.GroupContext = g.DefaultContext
	}
	profileGroup := *g
	profileGroup.DefaultProfile = profile.Name
	if fail := applyDefaultProfile(&profileGroup, &p); fail != nil {
		return "spawn_failed", fail.Msg, ""
	}
	snapshot := sandboxpolicy.OmittedProfilesSnapshot()
	if !sandboxProfilesDisabled(p.Harness, p.HarnessBuiltinMode, p.SandboxImplementation) {
		snapshot, err = db.ResolveEffectiveSandboxSnapshot(g.ID, "")
		if err != nil {
			return "spawn_failed", err.Error(), ""
		}
	}
	if ownerConv != "" {
		parent, readErr := db.AgentEffectiveSandboxConfigForConv(ownerConv)
		if readErr != nil {
			return "io", readErr.Error(), ""
		}
		if parent == nil && sandboxpolicy.HasCapabilities(snapshot) {
			return "permission_denied", "owning principal predates effective sandbox snapshots", ""
		}
		if parent != nil {
			validated, valErr := ensureAgentDirectoriesForRelaunch(*parent)
			if valErr != nil {
				return "spawn_failed", valErr.Error(), ""
			}
			if containErr := sandboxpolicy.RequireContained(validated, snapshot); containErr != nil {
				return "permission_denied", containErr.Error(), ""
			}
		}
	}
	p.EffectiveSandbox = &snapshot
	p.AgentID = db.NewAgentID()
	worker := &db.TriggerWorker{RuleID: rule.ID, FiringID: firingID, ActionIndex: index, AgentID: p.AgentID, State: "reserved", CreatedAt: now}
	if spec.WorkerDeadlineSeconds > 0 {
		worker.DeadlineAt = now.Add(time.Duration(spec.WorkerDeadlineSeconds) * time.Second)
	}
	workerID, err := db.InsertTriggerWorker(worker)
	if err != nil {
		return "spawn_failed", "reserve trigger worker: " + err.Error(), ""
	}
	out, fail := executeSpawn(&profileGroup, p)
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
	if err := db.MarkTriggerWorkerDispatched(workerID, out.ConvID, out.Label); err != nil {
		return "spawned_tracking_pending", err.Error(), agentID
	}
	_ = db.AddAgentTags(agentID, fmt.Sprintf("trigger:%d", rule.ID))
	return "spawned", "", agentID
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
		// The bounded v1 message action addresses the PR author while stamping
		// the rule's group route; later slices may add explicit multicast.
	}
	targetConv, err = db.CurrentConvForAgent(event.PRAuthorAgent)
	if err != nil || targetConv == "" {
		return "target_invalid", "PR author has no current conversation", 0
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
			return "permission_denied", "owner cannot reach PR author and lacks " + PermMessageDirect, 0
		}
	}
	subject := triggerlogic.RenderTemplate(spec.SubjectTemplate, event, groupName)
	if strings.TrimSpace(subject) == "" {
		subject = "[trigger:" + rule.Name + "] pr.opened"
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
			continue
		}
		sess, _ := db.FindSessionByConvID(conv)
		if sess != nil && (sess.Status == session.StatusExited || sess.Status == session.StatusError) {
			_ = db.CompleteTriggerWorker(w.ID, "exited", sess.Status, now)
		}
	}
}
