package agentd

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// Cross-harness morph. The agent keeps its identity, inbox, groups and
// permissions, but its conversation is translated into a new native
// conversation of the destination harness (harness.TranslateHistory), the
// agent is rebound onto that conversation as its next generation, and the
// ordinary resume path launches it there. Launch posture does not map across
// harnesses, so the destination form is explicit: approval, model and effort
// come from a destination spawn profile or explicit fields, the drive starts
// at the destination's defaults, and only tclaude's own sandbox layer is
// carried as-is.

// crossMorph is a validated cross-harness morph of one agent generation.
type crossMorph struct {
	src, dst *harness.Harness
	cwd      string
	// target is the complete destination form, as recorded for a pending
	// morph and shown as "after".
	target db.AgentMorphForm
	// previous is the current form, recorded for `morph --back`.
	previous     db.AgentMorphForm
	conversation db.ConversationResumeProfile
	profile      db.AgentRelaunchProfile
	opts         harness.TranslateOptions
	// sandboxBefore and sandboxAfter are the harness-native modes, for display.
	sandboxBefore, sandboxAfter string
}

func cloneRelaunchProfile(p *db.AgentRelaunchProfile) (db.AgentRelaunchProfile, error) {
	var out db.AgentRelaunchProfile
	raw, err := json.Marshal(p)
	if err == nil {
		err = json.Unmarshal(raw, &out)
	}
	return out, err
}

func crossMorphFailure(status int, kind, msg string) *spawnFailure {
	return &spawnFailure{Status: status, Kind: kind, Msg: msg}
}

// planCrossHarnessMorph validates target for convID's agent and builds the
// successor's profiles in memory. caller is the requesting agent ("" for the
// operator); an agent caller gets the sandbox and approval lineage checks.
// It writes nothing, so it runs for a dry run and again under the launch lock
// before an apply.
func planCrossHarnessMorph(convID string, dst *harness.Harness, target db.AgentMorphForm, caller string) (*crossMorph, *spawnFailure) {
	src := harnessForConv(convID)
	if src == nil {
		return nil, crossMorphFailure(http.StatusConflict, "relaunch_profile", "the agent's harness cannot be resolved")
	}
	switch {
	case !src.SupportsTranscript():
		return nil, crossMorphFailure(http.StatusConflict, "cross_harness",
			"harness "+src.Name+" cannot export a translatable history")
	case !dst.SupportsTranscript():
		return nil, crossMorphFailure(http.StatusConflict, "cross_harness",
			"harness "+dst.Name+" cannot install a translated history")
	}
	current, err := durableRelaunchConfigForConv(convID)
	if err != nil {
		return nil, crossMorphFailure(http.StatusConflict, "relaunch_profile", err.Error())
	}
	if current.TemporaryHarnessBuiltinMode {
		return nil, crossMorphFailure(http.StatusConflict, "sandbox_override_active",
			"the agent runs under a temporary sandbox unlock; restore its sandbox before a cross-harness morph")
	}
	if current.ModelProxy != "" && current.ModelProxy != "off" {
		return nil, crossMorphFailure(http.StatusConflict, "model_proxy",
			"the agent's model proxy route is harness-specific and cannot be carried across harnesses")
	}
	conversation, err := db.ConversationResumeProfileForConv(convID)
	if err != nil || conversation == nil {
		return nil, crossMorphFailure(http.StatusConflict, "relaunch_profile", "the agent has no durable conversation profile")
	}
	agentProfile, err := db.AgentRelaunchProfileForConv(convID)
	if err != nil || agentProfile == nil {
		return nil, crossMorphFailure(http.StatusConflict, "relaunch_profile", "the agent has no durable launch profile")
	}
	base, err := cloneRelaunchProfile(db.ComposeAgentRelaunchProfile(conversation.FallbackRelaunch, agentProfile))
	if err != nil {
		return nil, crossMorphFailure(http.StatusInternalServerError, "io", err.Error())
	}

	if target.Approval == nil || strings.TrimSpace(*target.Approval) == "" {
		return nil, crossMorphFailure(http.StatusBadRequest, "invalid_form",
			"approval: a cross-harness morph needs an approval posture for "+dst.Name+
				" (from the spawn profile or --ask-for-approval); approval values do not map across harnesses")
	}
	model := ""
	if target.Model != nil {
		model = *target.Model
	}
	if dst.Name == harness.OpenCodeName && !strings.Contains(model, "/") {
		return nil, crossMorphFailure(http.StatusBadRequest, "invalid_form",
			"model: an OpenCode morph needs a provider/model, because OpenCode resumes with the model its history names")
	}

	// Sandbox. The implementation never changes. Under tclaude's own layer
	// the harness's inner sandbox is stood down and the outer wall carries
	// over unchanged; a harness-native posture has no counterpart in another
	// harness, so only the operator may pick one, from the destination form.
	implementation, err := sandboxpolicy.NormalizeImplementation(current.SandboxImplementation)
	if err != nil {
		return nil, crossMorphFailure(http.StatusConflict, "relaunch_profile", err.Error())
	}
	if caller != "" && !implementation.UsesTclaudeLayer() {
		return nil, crossMorphFailure(http.StatusForbidden, "sandbox_restricted",
			"the agent's sandbox is "+src.Name+"'s own ("+string(implementation)+
				"), which has no counterpart in another harness; only the operator can morph it across harnesses")
	}
	requestedMode := ""
	if !implementation.UsesTclaudeLayer() && target.HarnessBuiltinMode != nil {
		requestedMode = *target.HarnessBuiltinMode
	}
	if !implementation.OmitsOSConfinement() && !implementation.UsesTclaudeLayer() {
		if requestedMode, err = harness.ResolveHarnessBuiltinMode(dst, requestedMode); err != nil {
			return nil, crossMorphFailure(http.StatusBadRequest, "invalid_sandbox", err.Error())
		}
	}
	dstMode, fail := resolveLaunchHarnessBuiltinMode(dst, requestedMode, string(implementation))
	if fail != nil {
		return nil, fail
	}
	if fail := sandboxImplementationPostureFailure(dst.Name, string(implementation)); fail != nil {
		return nil, fail
	}
	if caller != "" {
		from := spawnLineageSandbox{Harness: src.Name, HarnessBuiltinMode: current.Sandbox, Implementation: implementation}
		to := spawnLineageSandbox{Harness: dst.Name, HarnessBuiltinMode: dstMode, Implementation: implementation}
		if !spawnSandboxLineageAllowed(from, to) {
			return nil, crossMorphFailure(http.StatusForbidden, "sandbox_restricted", fmt.Sprintf(
				"a morph from %s sandbox %q to %s sandbox %q (%s) would widen the agent's sandbox",
				src.Name, current.Sandbox, dst.Name, dstMode, implementation))
		}
		if fail := spawnSandboxLineageFailure(caller, dst.Name, dstMode, string(implementation)); fail != nil {
			fail.Msg = strings.Replace(fail.Msg, "may not spawn a", "may not morph an agent into a", 1)
			return nil, fail
		}
		if fail := spawnApprovalLineageFailure(caller, dst.Name, *target.Approval, false); fail != nil {
			fail.Msg = strings.Replace(fail.Msg, "may not spawn a", "may not morph an agent into a", 1)
			return nil, fail
		}
	}

	previous := db.MorphFormOf(&base, agentProfile.MorphProfile)
	previous.Harness = src.Name
	previous.HarnessBuiltinMode = &current.NormalSandbox

	p := base
	p.Version = db.RelaunchProfileVersion
	implementationValue := string(implementation)
	p.SandboxImplementation = &implementationValue
	p.HarnessBuiltinMode = &dstMode
	if !implementation.UsesTclaudeLayer() {
		source := "morph"
		if target.Profile != "" {
			source = "morph profile " + target.Profile
		}
		source = harness.SanitizeSandboxChosenBy(source)
		p.HarnessBuiltinModeSource = &source
	}
	p.TemporaryHarnessBuiltinMode = nil
	p.ApprovalPolicy = stringPtrValue(*target.Approval)
	autoReview := false
	p.ApprovalAutoReview = &autoReview
	p.ModelProxy = stringPtrValue("")
	p.ModelID = stringPtrValue(model)
	window := int64(0)
	if target.ContextWindowSize != nil {
		window = *target.ContextWindowSize
	}
	p.ContextWindowSize = &window
	p.Effort = stringPtrValue(derefString(target.Effort))
	p.ToolGovernance = stringPtrValue(derefString(target.Tools))
	// Launch fields that mean the same thing everywhere carry over; ones the
	// destination cannot honour fall back to its default, never to an error.
	p.AskUserQuestionTimeout = stringPtrValue(carryLaunchField(target.AskTimeout, base.AskUserQuestionTimeout,
		func(v string) (string, error) { return harness.ResolveAskTimeoutMode(dst, v) }))
	p.AutoCompactWindow = stringPtrValue(carryLaunchField(target.AutoCompactWindow, base.AutoCompactWindow,
		func(v string) (string, error) { return harness.ResolveAutoCompactWindow(dst, v) }))
	if ssh, err := harness.ResolveSSHWorkaround(dst, base.SSHWorkaround); err == nil {
		p.SSHWorkaround = &ssh
	} else {
		off := false
		p.SSHWorkaround = &off
	}
	// The drive starts at the destination's defaults: send-keys, and for
	// Codex the state root its launch environment selects.
	p.ConfiguredContextWindowMax, p.CopilotAPI, p.CopilotAPISource = nil, nil, nil
	p.CodexAppServer, p.CodexAppServerSource, p.CodexStateRoot, p.CodexStateRootSource = nil, nil, nil, nil
	p.FastMode, p.FastModeAtLaunch = nil, nil
	switch dst.Name {
	case harness.CopilotName:
		var zero int64
		api, source := false, ""
		p.ConfiguredContextWindowMax, p.CopilotAPI, p.CopilotAPISource = &zero, &api, &source
	case harness.CodexName:
		snapshot, err := db.AgentEffectiveSandboxConfigForConv(convID)
		if err != nil {
			return nil, crossMorphFailure(http.StatusInternalServerError, "io", err.Error())
		}
		root, rootSource, err := codexStateRootForLaunch(dst.Name, snapshot)
		if err != nil {
			return nil, crossMorphFailure(http.StatusUnprocessableEntity, "codex_state_root", err.Error())
		}
		// The translated rollout is installed under the daemon's Codex
		// home; an agent whose launch selects another one would not find it.
		if ambient, err := harness.CodexConfigDir(); err != nil || filepath.Clean(ambient) != filepath.Clean(root) {
			return nil, crossMorphFailure(http.StatusConflict, "codex_state_root",
				"the agent's Codex state root "+root+" is not the daemon's own, so a translated history cannot be installed for it")
		}
		appServer, appServerSource := false, ""
		p.CodexAppServer, p.CodexAppServerSource = &appServer, &appServerSource
		p.CodexStateRoot, p.CodexStateRootSource = &root, &rootSource
	}
	p.MorphProfile = target.Profile
	p.PreviousMorphForm = nil
	p.PendingMorph = nil

	fallback, err := cloneRelaunchProfile(&p)
	if err != nil {
		return nil, crossMorphFailure(http.StatusInternalServerError, "io", err.Error())
	}
	fallback.MorphProfile = ""
	successor := db.ConversationResumeProfile{
		Version: db.RelaunchProfileVersion, Harness: dst.Name, Cwd: current.Cwd,
		ResumeProvenance: conversation.ResumeProvenance, FallbackRelaunch: &fallback,
	}
	check, err := cloneRelaunchProfile(&p)
	if err != nil {
		return nil, crossMorphFailure(http.StatusInternalServerError, "io", err.Error())
	}
	if _, err := durableRelaunchConfigFromProfiles(convID, &successor, &check); err != nil {
		return nil, crossMorphFailure(http.StatusConflict, "relaunch_profile", "the new form cannot be launched: "+err.Error())
	}
	p.PreviousMorphForm = &previous

	complete := db.MorphFormOf(&p, target.Profile)
	complete.Harness = dst.Name
	complete.HarnessBuiltinMode = &dstMode

	opts := harness.MorphTranslateOptions()
	opts.Policy.TargetModel = model
	if window > 0 {
		opts.Policy.ContextWindowTokens = window
	}
	return &crossMorph{
		src: src, dst: dst, cwd: current.Cwd, target: complete, previous: previous,
		conversation: successor, profile: p, opts: opts,
		sandboxBefore: current.Sandbox, sandboxAfter: dstMode,
	}, nil
}

func stringPtrValue(v string) *string { return &v }

func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(*v)
}

// carryLaunchField returns the explicit value, else the current one when the
// destination accepts it, else "" (the destination's default).
func carryLaunchField(explicit, current *string, resolve func(string) (string, error)) string {
	if explicit != nil {
		return *explicit
	}
	if current == nil {
		return ""
	}
	if v, err := resolve(*current); err == nil {
		return v
	}
	return ""
}

// preview is the dry run of the history leg: what would be carried, what is
// lost, and which rung delivers it. Nothing is written.
func (cm *crossMorph) preview(convID string) (*transcript.LossReport, error) {
	return harness.PreviewTranslation(cm.src, cm.dst, convID, cm.cwd, cm.opts)
}

// morphArtifactDir is where a cross-harness morph keeps the full neutral
// transcript, briefing, loss report and any externalized media. The agent is
// pointed at these files, so they live in its own agent-readable directory
// under ~/.tclaude/api/morph: every later tclaude-layer launch of the agent
// reopens that directory (and only it) the way spawn attachments are, and
// tclaude's private data directory is walled off from a sandboxed agent.
func morphArtifactDir(agentID string) (string, error) {
	root, err := tclcommon.PrepareMorphArtifactsDir(agentID)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, time.Now().UTC().Format("20060102T150405.000Z")), nil
}

// installCrossHarnessHistory runs the fallback ladder: the translated native
// history, else a native conversation that points at the history saved as
// files. The source conversation is only read.
func (cm *crossMorph) installHistory(agentID, convID string) (*harness.TranslateResult, error) {
	opts := cm.opts
	dir, err := morphArtifactDir(agentID)
	if err != nil {
		return nil, fmt.Errorf("prepare the morph artifact directory: %w", err)
	}
	opts.ArtifactDir = dir
	res, err := harness.TranslateHistory(cm.src, cm.dst, convID, cm.cwd, opts)
	if err == nil {
		return res, nil
	}
	if res == nil || res.Plan == nil {
		return nil, err
	}
	slog.Warn("morph: native history install failed; using the context-file fallback",
		"conv", short8(convID), "harness", cm.dst.Name, "error", err)
	fallback, fallbackErr := harness.InstallContextFile(cm.dst, res.Plan, cm.cwd, opts.ArtifactDir)
	if fallbackErr != nil {
		return nil, fmt.Errorf("%v; the context-file fallback also failed: %w", err, fallbackErr)
	}
	return fallback, nil
}

// applyCrossHarnessMorphUnderLaunchLock is the cross-harness counterpart of
// applyMorphUnderLaunchLock: stop, translate into a new conversation, rebind
// the agent onto it with the destination profiles in one transaction, then
// resume it. Until the rebind commits, a failure resumes the source
// conversation unchanged.
func applyCrossHarnessMorphUnderLaunchLock(agentID, convID string, pending db.AgentPendingMorph, live *db.SessionRow) morphOutcome {
	dst, err := harness.Resolve(pending.Target.Harness)
	if err != nil {
		return morphOutcome{status: http.StatusBadRequest, code: "invalid_form", detail: err.Error()}
	}
	cm, fail := planCrossHarnessMorph(convID, dst, pending.Target, pending.ActorConv)
	if fail != nil {
		return morphOutcome{status: fail.Status, code: fail.Kind, detail: fail.Msg}
	}
	var clientHandoff *agentRestartTmuxHandoff
	restoreSource := func() string { return "" }
	if live != nil {
		clientHandoff = beginAgentRestartTmuxHandoff(live.TmuxSession)
		defer clientHandoff.finishForConv(convID)
		stopped := escalateShutdownUnderLaunchLock(convID, shutdownGrace)
		if stopped.Outcome == shutdownFailed {
			return morphOutcome{status: http.StatusInternalServerError, code: "shutdown_failed",
				detail: "could not stop the agent to morph it: " + stopped.Detail}
		}
		restoreSource = func() string {
			resume := resumeOneConvUnderLaunchLock(convID, false, nil)
			clientHandoff.finishForConv(convID)
			if resume.Action != "resumed" {
				return "; the agent could not be resumed in its old form either (" + resume.Detail + "); use the normal wake action"
			}
			return "; the agent was resumed in its old form"
		}
		if err := requireCurrentAgentGeneration(agentID, convID); err != nil {
			return morphOutcome{status: http.StatusConflict, code: "stale_generation",
				detail: "agent generation changed while stopping; it was not morphed: " + err.Error()}
		}
	}
	res, err := cm.installHistory(agentID, convID)
	if err != nil {
		return morphOutcome{status: http.StatusInternalServerError, code: "history_install",
			detail: "the history could not be installed for " + dst.Name + ": " + err.Error() + restoreSource()}
	}
	newConv := res.ConvID
	if err := db.ApplyCrossHarnessMorph(convID, newConv, cm.conversation, cm.profile); err != nil {
		res.Cleanup()
		return morphOutcome{status: http.StatusInternalServerError, code: "io",
			detail: "the agent could not be rebound to its translated conversation: " + err.Error() + restoreSource()}
	}
	// The source is now a past generation of the agent, like a reincarnated
	// predecessor: hidden from the default conversation listing.
	if err := db.SetConvIndexArchived(convID, true); err != nil {
		slog.Debug("morph: archive source conversation", "conv", short8(convID), "error", err)
	}
	out := morphOutcome{result: "morphed", status: http.StatusOK, convID: newConv, history: res.Report}
	if live == nil {
		out.detail = "the agent is offline; it resumes as " + dst.Name + " when it is next woken"
	} else {
		resume := resumeOneConvUnderLaunchLock(newConv, false, nil)
		clientHandoff.finishForConv(newConv)
		if resume.Action != "resumed" {
			return morphOutcome{status: http.StatusInternalServerError, code: "restart_failed", convID: newConv, history: res.Report,
				detail: "the agent was morphed to " + dst.Name + " but could not be resumed: " + resume.Detail +
					"; use the normal wake action to retry"}
		}
	}
	deliverMorphNote(newConv, pending, cm.previous, res.Report)
	return out
}

// isCrossHarnessTarget reports whether a recorded form is for another harness
// than convID's.
func isCrossHarnessTarget(convID string, f db.AgentMorphForm) bool {
	if f.Harness == "" {
		return false
	}
	h := harnessForConv(convID)
	return h == nil || h.Name != harnessOrDefault(f.Harness)
}
