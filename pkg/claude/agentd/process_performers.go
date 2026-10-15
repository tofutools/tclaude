package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Agent performer dispatch (tcl-09xlwr).
//
// The engine plans an agent command exactly like a program command: a durable,
// attempt-qualified outbox entry. What it does NOT decide is who performs it.
// That is this file's job, and its only durable state is one
// process_agent_dispatches row per (run, command):
//
//	pending ──resolve──▶ delivered ──report──▶ reported ──apply──▶ closed
//	   │                    │ performer lost (group/unscoped)
//	   ▼                    ▼
//	needs_performer ◀── pending (re-resolve)
//
// The run checkpoint stays the authority on whether a command is outstanding.
// Every step here is idempotent against it, so a daemon restart, a lost row, a
// report that committed to the row but not the run, or a person settling the
// node in the agent's place (human preemption) is finished by the next sweep.
//
// Delivery is always a message into the performer's agent inbox — never pane
// send-keys (CLAUDE.md injection-sink guardrail). The performer reports through
// the fixed `tclaude process report` CLI, which is authenticated by the agent
// socket identity and bound to the exact run/node/attempt it was given.

// Run scope kinds, as decided in tcl-fboivp.
const (
	processScopeNone  = "none"
	processScopeGroup = "group"
	processScopeAgent = "agent"
)

// Dispatch resolutions recorded on a delivered dispatch: how its performer was
// chosen. They are shown next to the performer's avatar.
const (
	processResolutionAssignee    = "assignee"
	processResolutionRole        = "role"
	processResolutionSpawned     = "spawned"
	processResolutionBound       = "bound"
	processResolutionSameSession = "same_session"
	// processResolutionAnyone is a member picked by an "anyone" selector.
	processResolutionAnyone = "anyone"
	// processResolutionFallbackFailed marks an attempt failed by its
	// fallback because nobody could take it.
	processResolutionFallbackFailed = "fallback_failed"
)

// processRunScope is the slice of a run's persisted scope binding
// (db.ProcessRunScope) that dispatch needs.
type processRunScope struct {
	Kind     string
	GroupID  int64
	AgentID  string
	Owner    string // "agent:<agent_id>" or "human:operator"
	Orphaned bool
	// Paused holds new agent turns: attempts already delivered keep going,
	// pending ones wait for the operator's resume.
	Paused bool
}

// processOwnerOperator is the run owner for runs the operator started.
const processOwnerOperator = "human:operator"

// processRunScopeOf resolves a run's scope. It is a variable only so flow tests
// can bind a scope without going through run creation.
var processRunScopeOf = defaultProcessRunScope

func defaultProcessRunScope(runID string) (processRunScope, error) {
	stored, err := db.GetProcessRunScope(runID)
	if err != nil {
		return processRunScope{Kind: processScopeNone, Owner: processOwnerOperator}, err
	}
	scope := processRunScope{Kind: stored.Kind, GroupID: stored.GroupID, AgentID: stored.AgentID,
		Owner: stored.Owner, Orphaned: stored.Orphaned(), Paused: stored.Paused()}
	if scope.Kind == "" {
		scope.Kind = processScopeNone
	}
	if scope.Owner == "" {
		scope.Owner = processOwnerOperator
	}
	return scope, nil
}

// processDispatchMu serializes performer resolution. Resolution may spawn an
// agent before its dispatch row can be compare-and-swapped, so two concurrent
// resolvers of one attempt must never both get that far.
var processDispatchMu sync.Mutex

// processDispatchWG tracks asynchronous dispatch passes so tests and shutdown
// can wait for them.
var processDispatchWG sync.WaitGroup

// kickProcessAgentDispatch reconciles one run's agent dispatches in the
// background. The run's owner calls it right after committing agent commands;
// it must never run synchronously there, because a pass may route an input
// (a recovered report, a withdrawal) back to that very owner.
func kickProcessAgentDispatch(runID string) {
	processDispatchWG.Go(func() {
		if err := reconcileProcessRunAgentDispatches(runID); err != nil {
			slog.Warn("process performers: dispatch pass failed", "run", runID, "error", err)
		}
	})
}

// sweepProcessAgentDispatches is the periodic recovery pass. It rediscovers
// agent commands whose dispatch row was never written (a crash between the
// planning commit and the row insert) and then reconciles every open dispatch.
// After a daemon restart this is what picks in-flight dispatches back up: a
// delivered dispatch simply stays delivered, so nothing is re-sent.
func sweepProcessAgentDispatches() {
	if !processRoutesEnabled() {
		return
	}
	runs := map[string]struct{}{}
	after := ""
	for {
		ids, next, err := db.ListProcessRunIDsWithAgentCommands(after, db.MaxProcessRunReadPage)
		if err != nil {
			slog.Warn("process performers: rediscovery failed", "error", err)
			break
		}
		for _, id := range ids {
			runs[id] = struct{}{}
		}
		if next == "" {
			break
		}
		after = next
	}
	afterRun, afterCommand := "", ""
	for {
		rows, err := db.ListOpenProcessAgentDispatches(afterRun, afterCommand, db.MaxProcessDispatchReadPage)
		if err != nil {
			slog.Warn("process performers: open dispatch scan failed", "error", err)
			break
		}
		for _, row := range rows {
			runs[row.RunID] = struct{}{}
		}
		if len(rows) < db.MaxProcessDispatchReadPage {
			break
		}
		last := rows[len(rows)-1]
		afterRun, afterCommand = last.RunID, last.CommandID
	}
	ids := make([]string, 0, len(runs))
	for id := range runs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if err := reconcileProcessRunAgentDispatches(id); err != nil {
			slog.Warn("process performers: dispatch pass failed", "run", id, "error", err)
		}
	}
}

// reconcileProcessRunAgentDispatches brings every dispatch of one run in line
// with its durable checkpoint: rows for new commands, delivery for unresolved
// ones, re-resolution for lost performers, recovery of committed-but-unapplied
// reports, withdrawal from a draining run, and closure of settled commands.
//
// The checkpoint is read under processDispatchMu. Rows are only created under
// that lock, so the snapshot always covers every open row: a snapshot read
// before the lock could predate a command another pass has since delivered,
// and would close that live dispatch for good.
func reconcileProcessRunAgentDispatches(runID string) error {
	processDispatchMu.Lock()
	record, err := db.GetProcessRun(runID)
	if err != nil {
		processDispatchMu.Unlock()
		return err
	}
	var checkpoint engine.Checkpoint
	if err := json.Unmarshal(record.CheckpointJSON, &checkpoint); err != nil {
		processDispatchMu.Unlock()
		return fmt.Errorf("decode checkpoint: %w", err)
	}
	outstanding := map[string]engine.Command{}
	for _, command := range checkpoint.Commands {
		if command.Kind == engine.CommandAgent && command.Agent != nil {
			outstanding[command.ID] = command
		}
	}

	var withdraw []engine.Command
	var reapply []db.ProcessAgentDispatch
	for _, command := range sortedCommands(outstanding) {
		if _, err := db.EnsureProcessAgentDispatch(runID, command.ID, command.NodeID, command.Attempt); err != nil {
			processDispatchMu.Unlock()
			return err
		}
	}
	rows, err := db.ListProcessAgentDispatchesForRun(runID)
	if err != nil {
		processDispatchMu.Unlock()
		return err
	}
	draining := checkpoint.Status != engine.RunRunning || engine.Draining(checkpoint)
	for _, row := range rows {
		if !row.Open() {
			continue
		}
		command, live := outstanding[row.CommandID]
		switch {
		case !live:
			// The command left the outbox: its report was applied, a person
			// settled it, or the run ended. Nothing here is still owed.
			reason := ""
			if row.State != db.ProcessDispatchReported {
				reason = "settled without this performer's report"
				notifyProcessPerformerReleased(row, "This process node no longer needs your report: it was settled without it.")
			}
			if err := db.CloseProcessAgentDispatch(row.RunID, row.CommandID, reason); err != nil {
				slog.Warn("process performers: close failed", "run", runID, "command", row.CommandID, "error", err)
			}
		case draining:
			withdraw = append(withdraw, command)
		case row.State == db.ProcessDispatchReported:
			reapply = append(reapply, row)
		case row.State == db.ProcessDispatchDelivered:
			if failed := resolveLostProcessPerformer(row, command); failed != nil {
				reapply = append(reapply, *failed)
			}
		default:
			if failed := resolveAndDeliverProcessDispatch(row, command); failed != nil {
				reapply = append(reapply, *failed)
			}
		}
	}
	processDispatchMu.Unlock()

	// Run inputs go through the run's single state owner, and never while
	// holding the dispatch lock: the owner's own dispatch pass may need it.
	for _, row := range reapply {
		if err := applyRecordedProcessReport(row); err != nil && !errors.Is(err, engine.ErrStaleObservation) {
			slog.Warn("process performers: recorded report not yet applied", "run", runID,
				"node", row.NodeID, "error", err)
		}
	}
	for _, command := range withdraw {
		withdrawProcessAgentCommand(runID, command)
	}
	return nil
}

func sortedCommands(commands map[string]engine.Command) []engine.Command {
	out := make([]engine.Command, 0, len(commands))
	for _, command := range commands {
		out = append(out, command)
	}
	slices.SortFunc(out, func(a, b engine.Command) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// resolveLostProcessPerformer re-resolves a delivered dispatch whose performer
// can no longer do the work. An agent-scoped run is bound to its agent: losing
// that agent orphans the whole run (tcl-mrhhuc), so its dispatch just waits.
func resolveLostProcessPerformer(row db.ProcessAgentDispatch, command engine.Command) *db.ProcessAgentDispatch {
	scope, err := processRunScopeOf(row.RunID)
	if err != nil || scope.Kind == processScopeAgent {
		return nil
	}
	reason := processPerformerLostReason(scope, row.AgentID)
	if reason == "" {
		return nil
	}
	if err := db.ReleaseProcessAgentDispatch(row.RunID, row.CommandID, row.AgentID, reason); err != nil {
		return nil
	}
	row.State, row.AgentID = db.ProcessDispatchPending, ""
	return resolveAndDeliverProcessDispatch(row, command)
}

// processPerformerLostReason names why a delivered performer can no longer
// perform, or "" when it still can. An offline performer is NOT lost: its
// inbox keeps the prompt and the node simply waits for it.
func processPerformerLostReason(scope processRunScope, agentID string) string {
	performer, err := db.GetAgent(agentID)
	if err != nil {
		return ""
	}
	if performer == nil {
		// A spawned performer whose launch is still pending has no agent row
		// yet. It is starting, not lost; only a spawn that died is.
		if pending, err := db.GetPendingSpawnByAgentID(agentID); err != nil || pending != nil {
			return ""
		}
		return "performer " + agentID + " never started"
	}
	if !performer.Active() {
		return "performer " + agentID + " was retired"
	}
	if scope.Kind == processScopeGroup {
		conv, _ := db.CurrentConvForAgent(agentID)
		if member, _ := db.FindMemberInGroup(scope.GroupID, conv); member == nil {
			return "performer " + agentID + " left the group"
		}
	}
	return ""
}

// processPerformerCandidate is one agent resolution may choose.
type processPerformerCandidate struct {
	AgentID string
	ConvID  string
	Name    string
	Role    string
	// rank orders preference: idle, then online, then offline.
	rank int
}

// processPerformerChoice is the outcome of resolution: an existing agent, a
// spawn plan, or the reason nobody can take the node.
type processPerformerChoice struct {
	candidate  *processPerformerCandidate
	spawn      *managedSpawn
	resolution string
	waitReason string
	// failReason, when set, fails the attempt: the node's fallback says to
	// fail rather than wait when nobody can take it.
	failReason string
	// feedback is the previous attempt's failure, handed back to a
	// feedback-same-session performer.
	feedback string
}

// resolveAndDeliverProcessDispatch resolves a performer for one pending or
// needs-performer dispatch and delivers the node prompt to it. The caller holds
// processDispatchMu. When the node's fallback fails the attempt instead, it
// returns the dispatch row carrying that failure report, for the caller to
// apply to the run once it has released the lock.
func resolveAndDeliverProcessDispatch(row db.ProcessAgentDispatch, command engine.Command) *db.ProcessAgentDispatch {
	scope, err := processRunScopeOf(row.RunID)
	if err != nil {
		slog.Warn("process performers: scope lookup failed", "run", row.RunID, "error", err)
		return nil
	}
	if scope.Paused {
		// A paused run starts no new agent turn. The attempt stays as it is —
		// not needs_performer, which would ask a human for a performer — and
		// resume's dispatch pass delivers it.
		return nil
	}
	var choice processPerformerChoice
	if scope.Orphaned {
		choice.waitReason = "the run is orphaned: its scope target is gone"
	} else {
		previous := previousProcessDispatch(row)
		loop := processLoopFeedback(row.RunID, command)
		body := processDispatchMessage(row.RunID, command, "", loop)
		choice = resolveProcessPerformer(row.RunID, scope, command, previous, body)
		if choice.feedback != "" {
			body = processDispatchMessage(row.RunID, command, choice.feedback, loop)
		}
		switch {
		case choice.candidate != nil:
			messageID, err := queueAgentMessage(&db.AgentMessage{
				GroupID: scope.GroupID, ToConv: choice.candidate.ConvID,
				Subject: processDispatchSubject(row.RunID, command), Body: body,
			})
			if err != nil {
				slog.Warn("process performers: delivery failed", "run", row.RunID, "node", row.NodeID, "error", err)
				return nil
			}
			if err := db.MarkProcessAgentDispatchDelivered(row.RunID, row.CommandID, row.State,
				choice.candidate.AgentID, choice.resolution, messageID); err != nil {
				slog.Warn("process performers: delivery not recorded", "run", row.RunID, "node", row.NodeID, "error", err)
			}
			return nil
		case choice.spawn != nil:
			choice.spawn.params.InitialMessage = body
			choice.spawn.params.AgentID = db.NewAgentID()
			out, fail := executeSpawn(choice.spawn.group, choice.spawn.params)
			if fail != nil {
				choice.waitReason = "spawning a performer failed: " + fail.Msg
				break
			}
			agentID := out.AgentID
			if agentID == "" && out.ConvID != "" {
				agentID, _ = db.AgentIDForConv(out.ConvID)
			}
			if agentID == "" {
				choice.waitReason = "spawning a performer returned no stable agent identity"
				break
			}
			if err := db.MarkProcessAgentDispatchDelivered(row.RunID, row.CommandID, row.State,
				agentID, processResolutionSpawned, 0); err != nil {
				slog.Warn("process performers: spawned delivery not recorded", "run", row.RunID, "node", row.NodeID, "error", err)
			}
			return nil
		case choice.failReason != "":
			if err := db.FailProcessAgentDispatch(row.RunID, row.CommandID, row.State, processResolutionFallbackFailed,
				string(engine.ProgramFailed), choice.failReason); err != nil {
				slog.Warn("process performers: fallback failure not recorded", "run", row.RunID, "node", row.NodeID, "error", err)
				return nil
			}
			failed, err := db.GetProcessAgentDispatch(row.RunID, row.CommandID)
			if err != nil || failed == nil {
				return nil
			}
			return failed
		}
	}
	if row.State == db.ProcessDispatchNeedsPerformer && row.WaitReason == choice.waitReason {
		return nil
	}
	if err := db.MarkProcessAgentDispatchNeedsPerformer(row.RunID, row.CommandID, choice.waitReason); err != nil {
		slog.Warn("process performers: needs-performer not recorded", "run", row.RunID, "node", row.NodeID, "error", err)
	}
	return nil
}

// previousProcessDispatch returns the dispatch of the attempt immediately
// before row's, if that attempt was dispatched to an agent.
func previousProcessDispatch(row db.ProcessAgentDispatch) *db.ProcessAgentDispatch {
	if row.Attempt <= 1 {
		return nil
	}
	rows, err := db.ListProcessAgentDispatchesForNode(row.RunID, row.NodeID)
	if err != nil {
		return nil
	}
	for i := range rows {
		if rows[i].Attempt == row.Attempt-1 && rows[i].AgentID != "" {
			return &rows[i]
		}
	}
	return nil
}

// resolveProcessPerformer applies the scope decision (tcl-fboivp):
//
//   - agent scope: the bound agent performs every agentic node;
//   - group scope: the explicit assignee if it names a current member, then a
//     role-matching member (idle first), then a spawn with the node's profile
//     when the template allows it and the run owner could spawn into the
//     group, and otherwise a visible needs-performer wait;
//   - unscoped: the same rules with no members — only an explicit assignee.
//
// Retries layer on top. feedback-same-session hands the next attempt back to
// the previous performer, with its failure, when that performer is still
// eligible. fresh-attempt prefers anyone else, spawning when allowed.
func resolveProcessPerformer(runID string, scope processRunScope, command engine.Command,
	previous *db.ProcessAgentDispatch, body string) processPerformerChoice {
	spec := command.Agent
	// A new loop round goes back to the performer of the round before, which
	// is handed the decider's feedback in its own session (processLoopFeedback).
	loopReentry := processLoopReentry(command, previous)
	feedbackMode := (spec.RetryMode == model.RetryModeFeedbackSameSession && previous != nil) || loopReentry
	feedback := ""
	if feedbackMode && !loopReentry {
		feedback = previous.ReportNote
		if feedback == "" {
			feedback = "the previous attempt reported failure without a note"
		}
	}

	switch scope.Kind {
	case processScopeAgent:
		candidate, reason := processBoundCandidate(scope.AgentID)
		if candidate == nil {
			return processPerformerChoice{waitReason: reason}
		}
		if spec.Assignee != "" && !processCandidateMatches(*candidate, spec.Assignee) {
			return processPerformerChoice{waitReason: fmt.Sprintf(
				"assignee %q is not the agent this run is bound to", spec.Assignee)}
		}
		resolution := processResolutionBound
		if feedbackMode && previous.AgentID == candidate.AgentID {
			resolution = processResolutionSameSession
		}
		return processPerformerChoice{candidate: candidate, resolution: resolution, feedback: feedback}

	case processScopeGroup:
		group, err := db.GetAgentGroupByID(scope.GroupID)
		if err != nil || group == nil || group.IsArchived() {
			return processPerformerChoice{waitReason: "the run's group is unavailable"}
		}
		members := processGroupCandidates(group.ID)
		if feedbackMode {
			if candidate := findProcessCandidate(members, previous.AgentID); candidate != nil {
				return processPerformerChoice{candidate: candidate, resolution: processResolutionSameSession, feedback: feedback}
			}
		}
		if spec.Assignee != "" {
			for i := range members {
				if processCandidateMatches(members[i], spec.Assignee) {
					return processPerformerChoice{candidate: &members[i], resolution: processResolutionAssignee}
				}
			}
		}
		matching, resolution := processMatchingMembers(members, spec)
		// A fresh attempt is a fresh performer whenever one is available: the
		// previous performer is only a last resort, behind a spawn.
		var reuse *processPerformerCandidate
		if previous != nil && !feedbackMode {
			if index := slices.IndexFunc(matching, func(c processPerformerCandidate) bool { return c.AgentID == previous.AgentID }); index >= 0 {
				reused := matching[index]
				reuse = &reused
				matching = slices.Delete(matching, index, index+1)
			}
		}
		if len(matching) > 0 {
			sortProcessCandidates(matching)
			return processPerformerChoice{candidate: &matching[0], resolution: resolution}
		}
		action, profile := spec.Fallback()
		waitReason := ""
		if action == model.FallbackSpawn {
			if profile == "" {
				profile = group.DefaultProfile
			}
			if profile == "" {
				waitReason = processNoMemberReason(spec) + "; the fallback spawns the group's default spawn profile, and the group has none"
			} else {
				plan, status, detail := planProcessPerformerSpawn(runID, scope, group, command, profile, body)
				if plan != nil {
					return processPerformerChoice{spawn: plan}
				}
				waitReason = fmt.Sprintf("no member can take the node and spawning a %q performer was refused (%s): %s",
					profile, status, detail)
			}
		}
		if reuse != nil {
			return processPerformerChoice{candidate: reuse, resolution: resolution}
		}
		if action == model.FallbackFail {
			return processPerformerChoice{failReason: processNoMemberReason(spec) + "; the step's fallback fails it"}
		}
		if waitReason == "" {
			waitReason = processNoMemberReason(spec)
		}
		return processPerformerChoice{waitReason: waitReason}

	default:
		if feedbackMode {
			if candidate, _ := processBoundCandidate(previous.AgentID); candidate != nil {
				return processPerformerChoice{candidate: candidate, resolution: processResolutionSameSession, feedback: feedback}
			}
		}
		if spec.Assignee != "" {
			candidate := processAgentBySelector(spec.Assignee)
			if candidate == nil {
				return processPerformerChoice{waitReason: fmt.Sprintf("assignee %q is not an active agent", spec.Assignee)}
			}
			if !processOwnerMayTask(scope.Owner, *candidate) {
				return processPerformerChoice{waitReason: fmt.Sprintf(
					"assignee %q shares no group with the run owner", spec.Assignee)}
			}
			return processPerformerChoice{candidate: candidate, resolution: processResolutionAssignee}
		}
		reason := "an unscoped run has no members to perform this node: " +
			"name an assignee, or start the run scoped to a group or an agent"
		if action, _ := spec.Fallback(); action == model.FallbackFail {
			return processPerformerChoice{failReason: reason}
		}
		return processPerformerChoice{waitReason: reason}
	}
}

// processMatchingMembers returns the members a node's selector admits, and
// the resolution that names the choice: everyone for "anyone", else the
// members holding a wanted role.
func processMatchingMembers(members []processPerformerCandidate, spec *engine.AgentCommand) ([]processPerformerCandidate, string) {
	if spec.Anyone {
		return slices.Clone(members), processResolutionAnyone
	}
	roles := processWantedRoles(spec)
	var matching []processPerformerCandidate
	for _, member := range members {
		if slices.ContainsFunc(roles, func(role string) bool { return strings.EqualFold(role, member.Role) }) {
			matching = append(matching, member)
		}
	}
	return matching, processResolutionRole
}

// sortProcessCandidates orders candidates by preference: idle, then online,
// then offline, and within each the one holding the fewest open process
// attempts.
func sortProcessCandidates(candidates []processPerformerCandidate) {
	busy := make(map[string]int, len(candidates))
	for _, candidate := range candidates {
		if open, err := db.ListProcessAgentDispatchesForAgent(candidate.AgentID, true, 100); err == nil {
			busy[candidate.AgentID] = len(open)
		}
	}
	slices.SortStableFunc(candidates, func(a, b processPerformerCandidate) int {
		if a.rank != b.rank {
			return a.rank - b.rank
		}
		return busy[a.AgentID] - busy[b.AgentID]
	})
}

func processNoMemberReason(spec *engine.AgentCommand) string {
	action, _ := spec.Fallback()
	switch {
	case spec.Anyone:
		return "the group has no active member to take this node"
	case spec.Role != "":
		if action == model.FallbackWait {
			return fmt.Sprintf("no group member has role %q; the step waits for one", spec.Role)
		}
		return fmt.Sprintf("no group member has role %q", spec.Role)
	case spec.Profile == "":
		return "no group member can take this node: it names no role (profile) and no assignee"
	case action == model.FallbackWait:
		return fmt.Sprintf("no group member has role %q; the template does not allow spawning one", spec.Profile)
	}
	return fmt.Sprintf("no group member has role %q", spec.Profile)
}

// planProcessPerformerSpawn authorizes a spawn of the node's profile into the
// run's group on behalf of the run owner. It reuses the managed-worker policy
// path, so a process can never spawn anything its owner could not.
func planProcessPerformerSpawn(runID string, scope processRunScope, group *db.AgentGroup,
	command engine.Command, profile, body string) (*managedSpawn, string, string) {
	ownerConv := ""
	if agentID, ok := strings.CutPrefix(scope.Owner, "agent:"); ok {
		owner, err := db.GetAgent(agentID)
		if err != nil || !owner.Active() {
			return nil, "permission_denied", "the run owner is retired or unavailable"
		}
		ownerConv, _ = db.CurrentConvForAgent(agentID)
		if ownerConv == "" {
			return nil, "permission_denied", "the run owner has no current conversation"
		}
	}
	name := processPerformerSpawnName(command)
	return planManagedSpawn(group, profile, nil, ownerConv, "process:"+runID, name, body, nil)
}

// processPerformerSpawnName derives a spawn-name-charset-safe agent name from
// the node id and attempt.
func processPerformerSpawnName(command engine.Command) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, command.NodeID)
	name = fmt.Sprintf("proc-%s-%d", name, command.Attempt)
	if len(name) > agent.MaxSpawnNameLen {
		name = name[:agent.MaxSpawnNameLen]
	}
	return name
}

// processWantedRoles is the set of member roles that may take a node: its
// role when it names one, else (the legacy shape) the profile name itself plus
// the role label and role refs the spawn profile of that name declares.
func processWantedRoles(spec *engine.AgentCommand) []string {
	if spec.Role != "" {
		return []string{spec.Role}
	}
	profile := spec.Profile
	if profile == "" {
		return nil
	}
	roles := []string{profile}
	if resolved, err := db.ResolveSpawnProfile(profile); err == nil && resolved != nil {
		if resolved.Role != "" {
			roles = append(roles, resolved.Role)
		}
		roles = append(roles, resolved.RoleRefs...)
	}
	return roles
}

func processGroupCandidates(groupID int64) []processPerformerCandidate {
	members, err := db.ListAgentGroupMembers(groupID)
	if err != nil {
		return nil
	}
	alive, _ := session.LiveTmuxSessions()
	var out []processPerformerCandidate
	for _, member := range members {
		agentID := peerAgentID(member.ConvID)
		performer, err := db.GetAgent(agentID)
		if err != nil || !performer.Active() {
			continue
		}
		candidate := processCandidateFor(agentID, member.ConvID, alive)
		candidate.Role = member.Role
		out = append(out, candidate)
	}
	return out
}

func processCandidateFor(agentID, convID string, alive map[string]struct{}) processPerformerCandidate {
	online, status := convLiveStatus(convID, alive)
	var rank int
	switch normalizeMemberStatus(online, status) {
	case "idle":
		rank = 0
	case "offline":
		rank = 2
	default:
		rank = 1
	}
	return processPerformerCandidate{AgentID: agentID, ConvID: convID, Name: agent.FreshTitle(convID), rank: rank}
}

func findProcessCandidate(candidates []processPerformerCandidate, agentID string) *processPerformerCandidate {
	for i := range candidates {
		if candidates[i].AgentID == agentID {
			return &candidates[i]
		}
	}
	return nil
}

// processBoundCandidate resolves one specific active agent.
func processBoundCandidate(agentID string) (*processPerformerCandidate, string) {
	performer, err := db.GetAgent(agentID)
	if err != nil || performer == nil {
		return nil, "the bound agent is unknown"
	}
	if !performer.Active() {
		return nil, "the bound agent is retired"
	}
	conv, _ := db.CurrentConvForAgent(agentID)
	if conv == "" {
		return nil, "the bound agent has no current conversation"
	}
	alive, _ := session.LiveTmuxSessions()
	candidate := processCandidateFor(agentID, conv, alive)
	return &candidate, ""
}

// processAgentBySelector resolves an assignee (name, agent id, or conv id) to
// an active agent anywhere.
func processAgentBySelector(selector string) *processPerformerCandidate {
	resolved, _, err := agent.ResolveSelector(selector)
	if err != nil || resolved.AgentID == "" {
		return nil
	}
	candidate, _ := processBoundCandidate(resolved.AgentID)
	return candidate
}

// processOwnerMayTask reports whether an unscoped run's owner may hand a node
// to this agent. The daemon delivers on the owner's behalf, so an agent owner
// reaches only itself and agents it shares a group with — the same reach its
// own messages have. The operator reaches every agent.
func processOwnerMayTask(owner string, candidate processPerformerCandidate) bool {
	ownerAgent, ok := strings.CutPrefix(owner, "agent:")
	if !ok {
		return owner == processOwnerOperator
	}
	if ownerAgent == candidate.AgentID {
		return true
	}
	ownerConv, _ := db.CurrentConvForAgent(ownerAgent)
	if ownerConv == "" {
		return false
	}
	shared, err := db.SharedGroupsForConvs(ownerConv, candidate.ConvID)
	return err == nil && len(shared) > 0
}

// processCandidateMatches reports whether an assignee names this agent: by
// stable agent id, conv id, or (case-insensitive) display name.
func processCandidateMatches(candidate processPerformerCandidate, assignee string) bool {
	assignee = strings.TrimSpace(assignee)
	return assignee != "" && (assignee == candidate.AgentID || assignee == candidate.ConvID ||
		strings.EqualFold(assignee, candidate.Name))
}

func processDispatchSubject(runID string, command engine.Command) string {
	return fmt.Sprintf("[process] %s · %s (attempt %d)", runID, command.NodeID, command.Attempt)
}

// processDispatchMessage is the inbox message that hands one node attempt to
// its performer. The identity block and the report commands are fixed text;
// the prompt is the template's bound instruction, with control characters
// other than line breaks and tabs removed.
//
// loop is the round feedback of a node in a loop body (processLoopFeedback).
func processDispatchMessage(runID string, command engine.Command, feedback, loop string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the performer of a process node.\n\n")
	fmt.Fprintf(&b, "Run:     %s\nNode:    %s\nAttempt: %d\n", runID, command.NodeID, command.Attempt)
	if command.Round > 0 {
		fmt.Fprintf(&b, "Round:   %d of at most %d\n", command.Round, command.Rounds)
	}
	if loop != "" {
		fmt.Fprintf(&b, "\n%s\n", sanitizeProcessText(loop))
	}
	if feedback != "" {
		fmt.Fprintf(&b, "\nYour previous attempt (%d) failed. Its report said:\n%s\n", command.Attempt-1,
			sanitizeProcessText(feedback))
	}
	fmt.Fprintf(&b, "\n## Task\n\n%s\n", sanitizeProcessText(command.Agent.Prompt))
	report := fmt.Sprintf("tclaude process report %%s --run %s --node %s --attempt %d", runID, command.NodeID, command.Attempt)
	if len(command.Verdicts) > 0 {
		// An agent decider: its report is the decision.
		fmt.Fprintf(&b, "\n## Your decision\n\nYou decide this step. Choose exactly one verdict: %s.\n", strings.Join(command.Verdicts, ", "))
		fmt.Fprintf(&b, "Your note is your reasoning; if your verdict sends the work back, it is what the next round's performer is told.\n")
		fmt.Fprintf(&b, "\nReport exactly once:\n\n")
		fmt.Fprintf(&b, "    "+report+" --verdict <verdict> --note \"<your reasoning>\"\n", engine.ProgramSucceeded)
		fmt.Fprintf(&b, "\nIf you cannot decide at all, report failure instead; that fails the step:\n\n")
		fmt.Fprintf(&b, "    "+report+" --note \"<why you cannot decide>\"\n", engine.ProgramFailed)
		fmt.Fprintf(&b, "\nOnly you can report this attempt, and only once.\n")
		return b.String()
	}
	fmt.Fprintf(&b, "\n## When you are done\n\nReport exactly once, with the outcome of the work:\n\n")
	fmt.Fprintf(&b, "    "+report+" --note \"<short summary>\"\n", engine.ProgramSucceeded)
	fmt.Fprintf(&b, "    "+report+" --note \"<why it failed>\"\n", engine.ProgramFailed)
	fmt.Fprintf(&b, "\nOnly you can report this attempt, and only once.\n")
	return b.String()
}

func sanitizeProcessText(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
}

// notifyProcessPerformerReleased tells a performer that an attempt it was
// given no longer needs it. Best effort: the report path refuses a stale
// report either way.
func notifyProcessPerformerReleased(row db.ProcessAgentDispatch, text string) {
	if row.State != db.ProcessDispatchDelivered || row.AgentID == "" {
		return
	}
	conv, _ := db.CurrentConvForAgent(row.AgentID)
	if conv == "" {
		return
	}
	body := fmt.Sprintf("%s\n\nRun:     %s\nNode:    %s\nAttempt: %d\n", text, row.RunID, row.NodeID, row.Attempt)
	if _, err := queueAgentMessage(&db.AgentMessage{
		ToConv:  conv,
		Subject: fmt.Sprintf("[process] %s · %s (attempt %d) withdrawn", row.RunID, row.NodeID, row.Attempt),
		Body:    body,
	}); err != nil {
		slog.Warn("process performers: release notice failed", "run", row.RunID, "node", row.NodeID, "error", err)
	}
}

// withdrawProcessAgentCommand settles an agent command of a run that is
// already doomed. The run cannot finish while the command is outstanding, and
// no performer should keep working for it, so the engine records the attempt
// as failed — the same disposition a draining program gets when it is killed.
func withdrawProcessAgentCommand(runID string, command engine.Command) {
	report := processWithdrawalReport(command)
	if _, err := processRuns.submit(runID, executor.EngineActor, processRunInput{agentReport: &report}); err != nil &&
		!errors.Is(err, engine.ErrStaleObservation) {
		slog.Warn("process performers: withdrawal not applied", "run", runID, "node", command.NodeID, "error", err)
		return
	}
	closeProcessDispatchAfterWithdrawal(runID, command)
}

func processWithdrawalReport(command engine.Command) executor.AgentReport {
	return executor.AgentReport{
		CommandID: command.ID, NodeID: command.NodeID, Attempt: command.Attempt,
		Outcome: engine.ProgramFailed, Note: "withdrawn: the run is ending",
	}
}

func closeProcessDispatchAfterWithdrawal(runID string, command engine.Command) {
	// Under the dispatch lock, so a delivery racing this withdrawal either lands
	// first (and its performer is told) or finds the row closed.
	processDispatchMu.Lock()
	defer processDispatchMu.Unlock()
	row, err := db.GetProcessAgentDispatch(runID, command.ID)
	if err != nil || row == nil || !row.Open() {
		return
	}
	notifyProcessPerformerReleased(*row, "Stop working on this process node: its run is ending, so the attempt was withdrawn.")
	_ = db.CloseProcessAgentDispatch(runID, command.ID, "withdrawn: the run is ending")
}

// withdrawDrainingAgentCommands is the owner-side withdrawal: the run's own
// driver calls it after a commit left the run doomed, applying the withdrawals
// directly instead of routing them back to itself.
func withdrawDrainingAgentCommands(runID string, run *executor.Run) {
	if !run.Draining() {
		return
	}
	for _, command := range run.AgentCommands() {
		if err := executor.RecordAgentReport(run, executor.EngineActor, processWithdrawalReport(command)); err != nil {
			slog.Warn("process performers: withdrawal not applied", "run", runID, "node", command.NodeID, "error", err)
			continue
		}
		closeProcessDispatchAfterWithdrawal(runID, command)
	}
}

// applyRecordedProcessReport applies a report that is durable on its dispatch
// row but not yet on the run.
func applyRecordedProcessReport(row db.ProcessAgentDispatch) error {
	report := executor.AgentReport{
		CommandID: row.CommandID, NodeID: row.NodeID, Attempt: row.Attempt,
		Outcome: engine.ProgramOutcome(row.ReportOutcome), Note: row.ReportNote, Verdict: row.ReportVerdict,
		Performer: row.AgentID,
	}
	actor := "agent:" + row.AgentID
	if row.AgentID == "" {
		// A fallback failure has no performer: the engine records it.
		actor = executor.EngineActor
	}
	_, err := processRuns.submit(row.RunID, actor, processRunInput{agentReport: &report})
	if err == nil || errors.Is(err, engine.ErrStaleObservation) {
		// Applied now, or already settled some other way: either way the
		// dispatch has nothing left to do.
		_ = db.CloseProcessAgentDispatch(row.RunID, row.CommandID, "")
	}
	return err
}

// processReportRequest is the body of POST /v1/process/report. Run, node, and
// attempt are optional only as a convenience: when the caller has exactly one
// open dispatch, an empty selector means that one.
type processReportRequest struct {
	RunID   string                `json:"runId,omitempty"`
	NodeID  string                `json:"nodeId,omitempty"`
	Attempt int                   `json:"attempt,omitempty"`
	Outcome engine.ProgramOutcome `json:"outcome"`
	Note    string                `json:"note,omitempty"`
	// Verdict is an agent decider's decision: one of its node's outcomes.
	Verdict string `json:"verdict,omitempty"`
}

type processReportResponse struct {
	Status  string                `json:"status"`
	RunID   string                `json:"runId"`
	NodeID  string                `json:"nodeId"`
	Attempt int                   `json:"attempt"`
	Outcome engine.ProgramOutcome `json:"outcome"`
}

// handleProcessReport accepts one agent performer's report. The caller is
// authenticated by its agent socket identity; the report is authorized only by
// matching the exact dispatch that was delivered to that agent. A wrong agent,
// an older attempt, or a node a person already settled is refused, and a
// repeated identical report is acknowledged without being applied twice.
func handleProcessReport(w http.ResponseWriter, r *http.Request) {
	conv, ok := requireAgent(w, r)
	if !ok {
		return
	}
	agentID := peerAgentID(conv)
	if agentID == "" {
		writeError(w, http.StatusForbidden, "process_report_identity", "caller has no stable agent identity")
		return
	}
	var request processReportRequest
	if err := decodeProcessRuntimeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "process_report_request", err.Error())
		return
	}
	if request.Outcome != engine.ProgramSucceeded && request.Outcome != engine.ProgramFailed {
		writeError(w, http.StatusUnprocessableEntity, "process_report_outcome", "outcome must be succeeded or failed")
		return
	}
	request.Note = strings.TrimSpace(request.Note)
	request.Verdict = strings.TrimSpace(request.Verdict)
	// Validate before the report is recorded: a recorded report the executor
	// then refuses could never be applied or corrected.
	if err := executor.ValidateAgentReportNote(request.Note); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "process_report_note", err.Error())
		return
	}
	setAuditDetail(r, "run: "+stripControl(request.RunID)+", node: "+stripControl(request.NodeID)+
		", attempt: "+strconv.Itoa(request.Attempt)+", outcome: "+stripControl(string(request.Outcome)))

	row, status, code, message := selectProcessReportDispatch(agentID, request)
	if row == nil {
		writeError(w, status, code, message)
		return
	}
	response := processReportResponse{RunID: row.RunID, NodeID: row.NodeID, Attempt: row.Attempt, Outcome: request.Outcome}

	switch row.State {
	case db.ProcessDispatchDelivered:
		// A stored report is applied as it was sent, so one the run would
		// refuse — a decider's verdict that is not an outcome, or that goes
		// back past its loop's last round — is refused before it is stored.
		if status, code, message := checkProcessReport(*row, request); status != 0 {
			writeError(w, status, code, message)
			return
		}
		if err := db.RecordProcessAgentDispatchReport(row.RunID, row.CommandID, agentID,
			string(request.Outcome), request.Note, request.Verdict); err != nil {
			writeError(w, http.StatusConflict, "process_report_conflict", "the dispatch changed while reporting; retry")
			return
		}
		row.State, row.ReportOutcome, row.ReportNote = db.ProcessDispatchReported, string(request.Outcome), request.Note
		row.ReportVerdict = request.Verdict
		response.Status = "accepted"
	case db.ProcessDispatchReported, db.ProcessDispatchClosed:
		if row.ReportOutcome == "" {
			writeError(w, http.StatusConflict, "process_report_stale",
				"this attempt was settled without your report; it no longer needs one")
			return
		}
		if row.ReportOutcome != string(request.Outcome) || row.ReportVerdict != request.Verdict {
			reported := row.ReportOutcome
			if row.ReportVerdict != "" {
				reported += " with verdict " + row.ReportVerdict
			}
			writeError(w, http.StatusConflict, "process_report_conflict",
				fmt.Sprintf("this attempt was already reported as %s", reported))
			return
		}
		response.Status = "duplicate"
		if row.State == db.ProcessDispatchClosed {
			writeJSON(w, http.StatusOK, response)
			return
		}
	default:
		writeError(w, http.StatusForbidden, "process_report_wrong_performer",
			"this process node attempt is not currently delivered to you")
		return
	}

	err := applyRecordedProcessReport(*row)
	switch {
	case err == nil:
	case errors.Is(err, engine.ErrStaleObservation):
		if response.Status == "accepted" {
			// A person settled the node between delivery and this report.
			writeError(w, http.StatusConflict, "process_report_stale",
				"this attempt was settled while you were reporting; your report was not applied")
			return
		}
	default:
		// The report is durable on the dispatch; the recovery sweep applies it.
		slog.Warn("process performers: report recorded but not yet applied", "run", row.RunID,
			"node", row.NodeID, "error", err)
		response.Status = "recorded"
		writeJSON(w, http.StatusAccepted, response)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// checkProcessReport asks the run whether it would accept a report, without
// recording it. It returns a zero status when it would.
func checkProcessReport(row db.ProcessAgentDispatch, request processReportRequest) (int, string, string) {
	run, err := executor.LoadRun(row.RunID)
	if err != nil {
		return http.StatusInternalServerError, "process_report", err.Error()
	}
	err = executor.CheckAgentReport(run, executor.AgentReport{
		CommandID: row.CommandID, NodeID: row.NodeID, Attempt: row.Attempt,
		Outcome: request.Outcome, Note: request.Note, Verdict: request.Verdict, Performer: row.AgentID,
	})
	switch {
	case err == nil:
		return 0, "", ""
	case errors.Is(err, engine.ErrStaleObservation):
		return http.StatusConflict, "process_report_stale", "this attempt was settled while you were reporting; it no longer needs your report"
	case errors.Is(err, executor.ErrInvalidAgentReport), errors.Is(err, engine.ErrInvalidDecisionVerdict):
		return http.StatusUnprocessableEntity, "process_report_verdict", err.Error()
	default:
		return http.StatusInternalServerError, "process_report", err.Error()
	}
}

// selectProcessReportDispatch finds the dispatch a report means and checks it
// belongs to the caller and to the current attempt. On refusal it returns a nil
// row with the HTTP status, error code, and message to write.
func selectProcessReportDispatch(agentID string, request processReportRequest) (*db.ProcessAgentDispatch, int, string, string) {
	if request.RunID == "" {
		if request.NodeID != "" || request.Attempt != 0 {
			return nil, http.StatusBadRequest, "process_report_request", "--node and --attempt require --run"
		}
		open, err := db.ListProcessAgentDispatchesForAgent(agentID, true, 2)
		if err != nil {
			return nil, http.StatusInternalServerError, "process_report", err.Error()
		}
		switch len(open) {
		case 0:
			return nil, http.StatusNotFound, "process_report_not_found", "no process node attempt is waiting on your report"
		case 1:
			return &open[0], 0, "", ""
		default:
			return nil, http.StatusConflict, "process_report_ambiguous",
				"you have more than one process node attempt open; name it with --run, --node, and --attempt"
		}
	}
	if request.NodeID == "" {
		return nil, http.StatusBadRequest, "process_report_request", "--run requires --node"
	}
	rows, err := db.ListProcessAgentDispatchesForNode(request.RunID, request.NodeID)
	if err != nil {
		return nil, http.StatusInternalServerError, "process_report", err.Error()
	}
	if len(rows) == 0 {
		return nil, http.StatusNotFound, "process_report_not_found", "no agent attempt was dispatched for that run and node"
	}
	latest := rows[len(rows)-1]
	target := latest
	if request.Attempt != 0 {
		index := slices.IndexFunc(rows, func(row db.ProcessAgentDispatch) bool { return row.Attempt == request.Attempt })
		if index < 0 {
			return nil, http.StatusNotFound, "process_report_not_found", "no agent attempt with that number was dispatched"
		}
		target = rows[index]
	}
	if target.AgentID != agentID {
		return nil, http.StatusForbidden, "process_report_wrong_performer",
			"this process node attempt was not delivered to you"
	}
	if target.Attempt != latest.Attempt {
		return nil, http.StatusConflict, "process_report_stale",
			fmt.Sprintf("attempt %d is stale: the node is on attempt %d", target.Attempt, latest.Attempt)
	}
	return &target, 0, "", ""
}

// processRunPerformerView is the read model of one node attempt's performer:
// who, how they were chosen, where the dispatch stands, and what they
// reported. It is what the UI draws avatars on nodes from.
type processRunPerformerView struct {
	NodeID     string `json:"nodeId"`
	Attempt    int    `json:"attempt"`
	CommandID  string `json:"commandId"`
	State      string `json:"state"`
	AgentID    string `json:"agentId,omitempty"`
	Name       string `json:"name,omitempty"`
	Status     string `json:"status,omitempty"`
	Resolution string `json:"resolution,omitempty"`
	WaitReason string `json:"waitReason,omitempty"`
	Outcome    string `json:"outcome,omitempty"`
	Note       string `json:"note,omitempty"`
	// DeliveredAt is when the node prompt reached the performer's inbox: the
	// turn injected into its conversation.
	DeliveredAt *time.Time `json:"deliveredAt,omitempty"`
}

// processLiveness lists the live tmux sessions, at most once per value: a
// read that projects many runs shares one, so a context poll forks tmux once
// rather than once per run.
type processLiveness func() map[string]struct{}

func newProcessLiveness() processLiveness {
	var once sync.Once
	var alive map[string]struct{}
	return func() map[string]struct{} {
		once.Do(func() { alive, _ = session.LiveTmuxSessions() })
		return alive
	}
}

// processRunPerformerViews projects a run's dispatch history.
func processRunPerformerViews(runID string, live processLiveness) (map[string]processRunPerformerView, []processRunPerformerView, error) {
	rows, err := db.ListProcessAgentDispatchesForRun(runID)
	if err != nil || len(rows) == 0 {
		return map[string]processRunPerformerView{}, []processRunPerformerView{}, err
	}
	byCommand := make(map[string]processRunPerformerView, len(rows))
	views := make([]processRunPerformerView, 0, len(rows))
	for _, row := range rows {
		view := processRunPerformerView{
			NodeID: row.NodeID, Attempt: row.Attempt, CommandID: row.CommandID, State: row.State,
			AgentID: row.AgentID, Resolution: row.Resolution, WaitReason: row.WaitReason,
			Outcome: row.ReportOutcome, Note: row.ReportNote,
		}
		if !row.DeliveredAt.IsZero() {
			delivered := row.DeliveredAt
			view.DeliveredAt = &delivered
		}
		if row.AgentID != "" {
			view.Status = "retired"
			if performer, _ := db.GetAgent(row.AgentID); performer.Active() {
				conv, _ := db.CurrentConvForAgent(row.AgentID)
				online, status := convLiveStatus(conv, live())
				view.Status = normalizeMemberStatus(online, status)
				view.Name = agent.FreshTitle(conv)
			}
		}
		byCommand[row.CommandID] = view
		views = append(views, view)
	}
	return byCommand, views, nil
}

// processLoopReentry reports whether an attempt opens a new round of its
// loop: its node finished the round before successfully, so this attempt is
// not a retry of a failure.
func processLoopReentry(command engine.Command, previous *db.ProcessAgentDispatch) bool {
	return command.Round > 1 && previous != nil && previous.ReportOutcome == string(engine.ProgramSucceeded)
}

// processLoopFeedback is what a performer in a later round of a loop is told
// about why the work came back: the round, and the feedback the decider sent
// it back with (the loop_iterated evidence). Empty in a first round.
func processLoopFeedback(runID string, command engine.Command) string {
	if command.Round < 2 {
		return ""
	}
	events, err := db.ListLatestProcessRunNodeEvents(runID, "loop_iterated")
	if err != nil {
		slog.Warn("process performers: loop feedback not read", "run", runID, "node", command.NodeID, "error", err)
		return ""
	}
	for _, event := range events {
		// The executor's loopEvidencePayload, field for field: the payload
		// decode is strict.
		var payload struct {
			From          string `json:"from"`
			Outcome       string `json:"outcome"`
			To            string `json:"to"`
			Round         int    `json:"round"`
			MaxIterations int    `json:"maxIterations"`
			OnExhausted   string `json:"onExhausted,omitempty"`
			Feedback      string `json:"feedback,omitempty"`
		}
		// Only the command's own innermost loop: with nested loops another
		// loop's row can carry the same round number.
		if event.DecodePayload(&payload) != nil || payload.Round != command.Round ||
			payload.From != command.LoopFrom || payload.Outcome != command.LoopOutcome {
			continue
		}
		text := fmt.Sprintf("## Sent back for round %d of %d\n\n%q decided %q, so this work runs again.",
			command.Round, command.Rounds, payload.From, payload.Outcome)
		if payload.Feedback != "" {
			text += " Its feedback:\n\n" + payload.Feedback
		} else {
			text += " It left no feedback."
		}
		return text
	}
	return ""
}
