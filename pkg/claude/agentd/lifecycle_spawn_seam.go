package agentd

import (
	"net/http"
	"runtime"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/launchundo"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// resolveSpawnLaunchSeam is executeSpawn's launch-seam step: whether the
// agent is enrolled before the fork (a preset conversation) or after it
// connects, and whether its group routes apply to this launch.
//
// Launch-enrollment path (Claude Code, unless reverted via config): the
// conv-id can be PRESET, so enroll the agent and bake its rename + welcome
// into the launch command — no post-connect tmux injection, no conv-id
// poll-wait. We generate the conv-id, enroll (group membership + inbox
// briefing) BEFORE the fork (the welcome must reference the briefing's
// message id), and forward the id/name/welcome as launch args. Harnesses
// that can't preset a conv-id (Codex) keep the inject-after-connect flow.
//
// Resolve (not Get) so a blank p.Harness normalises to the Claude Code
// default — callers like the template instantiator and the pending-spawn
// sweeper leave Harness unset, and those CC spawns must take the same
// launch-enrollment path as the HTTP spawn endpoint. Resolve also tolerates
// an unknown name (returns nil), and SupportsLaunchEnrollment is nil-safe,
// so a bad harness degrades to the legacy path rather than panicking.
//
// It also refuses node-profile workers without enrollment, installs any
// bundled history through installHistory (an import always enrolls before
// launch), refuses route-capable launches the seam cannot carry, and prepends
// an arrival briefing to the initial message.
func resolveSpawnLaunchSeam(g *db.AgentGroup, p *spawnParams, spawnHarness *harness.Harness, layeredLaunch bool,
	groupName string, installHistory func(*harness.Harness) *spawnFailure, importedID *string,
) (bool, bool, *spawnFailure) {
	launchEnroll := spawnHarness.SupportsLaunchEnrollment() && !spawnUsesLegacyInjection()
	if p.AgentID != "" {
		workerDefaults, err := db.GetFederationWorkerDefaults(p.AgentID)
		if err != nil {
			return false, false, &spawnFailure{http.StatusInternalServerError, "worker_defaults", err.Error()}
		}
		if workerDefaults != nil && len(workerDefaults.Permissions) > 0 && !launchEnroll {
			return false, false, &spawnFailure{http.StatusBadRequest, "worker_defaults", "node-profile workers require a harness with enrollment before launch"}
		}
	}

	if fail := installHistory(spawnHarness); fail != nil {
		return false, false, fail
	}
	if *importedID != "" {
		launchEnroll = true
	}
	if p.DarwinRouteCapable && !launchEnroll {
		return false, false, &spawnFailure{http.StatusUnprocessableEntity, "darwin_route_launch",
			"Darwin route-capable launches require the preset-conversation launch seam"}
	}
	routeEnabled := runtime.GOOS == "darwin" && p.DarwinRouteCapable && g != nil
	if runtime.GOOS == "linux" && g != nil {
		var routeErr error
		routeEnabled, routeErr = db.IsAgentGroupRouteEnabled(g.ID, PermRoutesPublish, PermRoutesConsume)
		if routeErr != nil {
			return false, false, &spawnFailure{http.StatusInternalServerError, "route_authority", "could not resolve group route capability: " + routeErr.Error()}
		}
		if routeEnabled && !layeredLaunch {
			return false, false, &spawnFailure{http.StatusUnprocessableEntity, "unsupported_group_route_launch", "Linux group routes require a pane-authoritative launch with tclaude’s sandbox"}
		}
	}
	if p.Arrival != nil {
		if p.AgentID == "" {
			p.AgentID = db.NewAgentID()
		}
		arrival := *p.Arrival
		arrival.Sandbox = p.EffectiveSandbox
		p.InitialMessage = buildArrivalBriefing(arrival, p.AgentID, p.Cwd, groupName, spawnHarness.Name, p.Model) + "\n\n" + p.InitialMessage
		p.Arrival = nil // an async continuation must not add a second briefing
	}
	return launchEnroll, routeEnabled, nil
}

// enrollOrSeedSpawn is executeSpawn's enrollment step. A launch-enrolled spawn
// presets its conversation and enrolls (group membership and inbox briefing)
// before the fork; a seed-needing harness instead gets its first-turn welcome
// as the launch prompt. It returns the preset conversation, its briefing
// message id and whether that briefing rode inline.
func enrollOrSeedSpawn(g *db.AgentGroup, p *spawnParams, spawnArgs *clcommon.SpawnArgs, spawnHarness *harness.Harness,
	groupName, importedID string, openCodeLaunch *openCodeLaunch, undo *launchundo.Undo, launchEnroll bool,
) (string, int64, bool, *spawnFailure) {
	var preConvID string
	var preMsgID int64
	// briefingInlined records whether the launch-enrollment prompt baked the
	// whole briefing inline (short enough to fit) rather than pointing at the
	// inbox copy. When it did, the inbox copy is inserted already delivered and
	// read — the agent has the text, so it must never enter the nudge queue.
	var briefingInlined bool
	if launchEnroll {
		var fail *spawnFailure
		preConvID, preMsgID, briefingInlined, fail = enrollBeforeLaunch(g, p, spawnArgs, spawnHarness,
			groupName, importedID, openCodeLaunch, undo)
		if fail != nil {
			return "", 0, false, fail
		}
	} else if spawnHarness.NeedsSpawnSeed() {
		// Seed-needing harness (Codex): the conv-id can't be preset, so
		// enrollment + the inbox briefing happen post-connect. But the pane still
		// needs a positional first-turn prompt to materialise its conv-id
		// (JOH-205) — and that prompt IS the [system: ...] welcome, replacing the
		// old inert "[tclaude] …" placeholder. A short/empty briefing rides in
		// full (inline brief, or "wait"), so the agent gets a single greeting
		// turn that looks like the Claude Code launch prompt and the post-connect
		// welcome is skipped (finishSpawnEnrollment gates that on the same
		// spawnBriefingFitsLaunch predicate). A long briefing's seed is a
		// stand-by welcome; its inbox-pointer welcome is injected post-connect,
		// once the inbox row + id exist. No conv-id is known here, so the welcome
		// carries no inbox-message id (msgID 0). (CC on the legacy-injection
		// revert reports its id via hook and needs no seed, so it is excluded.)
		spawnContextBody := buildSpawnContextBody(groupName, p.GroupContext, p.ProfileContext, p.InitialMessage, p.Attachments, p.ManagedInstructionsBody)
		spawnArgs.InitialPrompt = buildSpawnSeedPrompt(p.Name, p.Role, p.Descr, groupName,
			p.InitialMessage != "", spawnContextBody, p.WorktreePath, p.WorktreeBranch,
			resolveSpawnerTitle(p.SpawnedByConv, p.SpawnedByAgent), spawnInlineMaxChars())
	}
	return preConvID, preMsgID, briefingInlined, nil
}

// landSpawnWithoutLaunch is executeSpawn's offline landing: a NoLaunch spawn
// keeps its Claude subscription marker (and token), records the offline resume
// profile of a server-authoritative harness, commits the enrollment and lands
// the conversation without forking a pane.
func landSpawnWithoutLaunch(p *spawnParams, spawnHarness *harness.Harness, preConvID string, undo *launchundo.Undo) (*spawnOutcome, *spawnFailure) {
	// Landing offline keeps the subscription: the marker always, and the
	// token when it came, so a later start never uses this node's login.
	if p.ClaudeSubscription.peer != "" {
		agentID := p.AgentID
		if agentID == "" && preConvID != "" {
			agentID, _ = db.AgentIDForConv(preConvID)
		}
		if agentID == "" {
			return nil, &spawnFailure{http.StatusInternalServerError, "claude_subscription", "could not resolve the agent identity to mark for the Claude subscription"}
		}
		mark := func() error { return db.SetAgentClaudeSubscription(agentID, p.ClaudeSubscription.peer) }
		if p.ClaudeSubscription.token != "" {
			mark = func() error { return holdClaudeSubscription(agentID, p.ClaudeSubscription) }
		}
		if err := mark(); err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "claude_subscription", err.Error()}
		}
	}
	if spawnHarness.UsesAuthoritativeServer() {
		if err := recordOfflineResumeProfile(preConvID, spawnHarness.Name, p.Cwd); err != nil {
			return nil, &spawnFailure{http.StatusInternalServerError, "history", err.Error()}
		}
	}
	// The landed conversation keeps its enrollment.
	undo.Commit()
	return landSpawnOffline(*p, spawnHarness, preConvID)
}
