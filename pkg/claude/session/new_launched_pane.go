package session

import (
	"fmt"
	"log/slog"
	"runtime"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/resumeprovenance"
)

// wrapLaunchInResourceCgroup wraps the pane command in the launch's resource
// cgroup when the implementation or the profile's limits ask for one, and
// returns the command with the cleanup that removes the cgroup. runNew owns
// running that cleanup until the pane takes it over. An unenforceable request
// is refused or disclosed per ResourceCgroupFailureAction.
func wrapLaunchInResourceCgroup(
	params *NewParams,
	sessionID string,
	sandboxImplementation sandboxpolicy.Implementation,
	launchSandbox, effectiveSandbox *sandboxpolicy.Snapshot,
	harnessCmd string,
) (string, func(), error) {
	resourceCgroupCleanup := func() {}
	// resource-only and tclaude-layer request the cgroup through the
	// implementation alone, so this seam must not require a launch snapshot to
	// find the request: a direct `tclaude session new --sandbox-impl
	// resource-only` and a CLI resume both arrive with no snapshot at all.
	var launchResourceLimits sandboxpolicy.ResourceLimits
	var launchResourceNotices []sandboxpolicy.AccessNotice
	if launchSandbox != nil {
		launchResourceLimits = launchSandbox.Effective.ResourceLimits
		launchResourceNotices = launchSandbox.Effective.AccessNotices
	}
	recordResourceNotice := func(notice sandboxpolicy.AccessNotice) {
		if launchSandbox != nil {
			launchSandbox.Effective.AccessNotices = append(launchSandbox.Effective.AccessNotices, notice)
		}
		if effectiveSandbox != nil {
			effectiveSandbox.Effective.AccessNotices = append(effectiveSandbox.Effective.AccessNotices, notice)
		}
	}
	if sandboxpolicy.ResourceCgroupRequested(launchResourceLimits, sandboxImplementation, runtime.GOOS) &&
		!resourceLimitsAlreadyOverridden(launchResourceNotices) {
		if err := sandboxpolicy.ValidateResourceLimitTarget(
			launchResourceLimits, sandboxImplementation, runtime.GOOS,
		); err != nil {
			if !params.AllowUnenforcedSandbox {
				return "", nil, fmt.Errorf("unsupported_sandbox_profile_resource_limits: %w", err)
			}
			recordResourceNotice(resourceLimitOverrideNotice(err))
		} else {
			var wrapped string
			var cleanup func()
			var resourceErr error
			if strings.TrimSpace(params.ResourceCgroupDir) != "" {
				// A daemon-prepared dir means a managed server owns this boundary
				// and is already running inside it; the pane merely joins for
				// accounting. Shared: the wrapper must not reap or remove the
				// boundary when the attach client exits.
				wrapped = wrapPreparedResourceCgroupCommand(
					sessionID, params.ResourceCgroupDir, harnessCmd, params.AllowUnenforcedSandbox, true, false,
					!sandboxpolicy.ResourceCgroupRequired(launchResourceLimits, sandboxImplementation))
				cleanup = func() {}
			} else {
				wrapped, cleanup, resourceErr = wrapResourceLimitedCommand(
					sessionID, launchResourceLimits, sandboxImplementation, harnessCmd,
					params.AllowUnenforcedSandbox,
				)
			}
			if resourceErr == nil {
				resourceCgroupCleanup = cleanup
				harnessCmd = wrapped
			} else {
				continuation := launchIsSandboxContinuation(params)
				switch ResourceCgroupFailureAction(
					launchResourceLimits, sandboxImplementation,
					continuation, params.AllowUnenforcedSandbox) {
				case DiscloseMissingResourceAccounting:
					slog.Warn("resource cgroup unavailable; launching without per-agent accounting",
						"session_id", sessionID, "continuation", continuation, "error", resourceErr)
					recordResourceNotice(ResourceCgroupUnavailableNotice(resourceErr))
				case DiscloseUnenforcedResourceOverride:
					recordResourceNotice(resourceLimitOverrideNotice(resourceErr))
				case RefuseResourceCgroupFailure:
					return "", nil, resourceErr
				default:
					// A policy value this seam does not know must not silently drop the
					// boundary the launch asked for.
					return "", nil, resourceErr
				}
			}
		}
	}
	return harnessCmd, resourceCgroupCleanup, nil
}

// captureLaunchProvenance records the physical identity of the directory the
// launched pane is actually in, which a later unattended resume checks. A
// managed launch must have it; a direct session warns and launches without.
func captureLaunchProvenance(tmuxSession, sessionID string, managed bool) (string, error) {
	resumeProvenance := ""
	physicalCwd, provenanceErr := clcommon.LivePaneCwd(tmuxSession)
	if provenanceErr == nil {
		var captured resumeprovenance.Provenance
		captured, provenanceErr = resumeprovenance.Capture(physicalCwd)
		if provenanceErr == nil {
			resumeProvenance, provenanceErr = resumeprovenance.Encode(captured)
		}
	}
	if provenanceErr != nil {
		if managed {
			return "", fmt.Errorf("capture managed launch provenance metadata: %w", provenanceErr)
		}
		slog.Warn("could not capture launch-directory metadata for direct session",
			"session", sessionID, "error", provenanceErr)
	}
	return resumeProvenance, nil
}

// configureLaunchedPane applies tclaude's per-session tmux settings to the
// launched pane: its window title, terminal passthrough, harness-dependent
// scrollback and the navigation keybindings.
func configureLaunchedPane(tmuxSession, sessionID string, h *harness.Harness) {
	applyTmuxWindowTitle(tmuxSession, sessionID)

	// All harness windows allow terminal integration sequences (such as
	// clipboard OSC 52) through tmux without changing the server-wide policy.
	ConfigureTmuxPassthrough(tmuxSession)

	// Enable tmux mouse-wheel scrollback for this session when the harness
	// relies on tmux for history (Codex CLI). Scoped to this session only so
	// Claude Code panes — which own their scrollback — are untouched (JOH-213).
	ConfigureTmuxScrollback(tmuxSession, h)

	// Configure keybindings for session navigation (idempotent)
	ConfigureTmuxKeybindings()
}

// recordLaunchPostureAndSeeds records the postures this launch ran with, for
// relaunch, and seeds the model and effort a harness with no startup callback
// cannot report itself.
func recordLaunchPostureAndSeeds(
	sessionID string,
	h *harness.Harness,
	params *NewParams,
	model, effort string,
	autoMemory, remoteControl bool,
	contextFeatures map[string]string,
	autoCompactWindow string,
) {
	// Record the postures this launch actually ran with so a later relaunch
	// reproduces them instead of falling back to defaults. Out-of-band like
	// SetSessionResumeProvenance, since SaveSession's UPSERT does not own these
	// columns. On a --resume the values came from the conversation's own record
	// via applyRecordedLaunchPosture, so this write keeps them alive rather than
	// overwriting them. See RecordLaunchPosture.
	// The authoring surface for both pointer fields, and the only one: this launch
	// resolved --context-window-max and --copilot-api (an explicit flag, the
	// profile chain, or the conversation's own record via
	// applyRecordedLaunchPosture) and rendered the launch from them, so it may
	// assert either value including a zero. A surface that cannot resolve them
	// passes nil instead — see LaunchPosture.
	//
	// The one exception is copilotAPIPostureToRecord's, and it is a launch that DID
	// resolve the drive.
	RecordLaunchPosture(sessionID, h, LaunchPosture{
		AutoMemory:        autoMemory,
		PeerMessaging:     params.PeerMessaging,
		ContextFeatures:   contextFeatures,
		AutoCompactWindow: autoCompactWindow,
		RemoteControl:     remoteControl,
		FastMode:          params.FastMode,
		ContextWindowMax:  &params.ContextWindowMax,
		CopilotAPI:        copilotAPIPostureToRecord(params),
		CodexAppServer:    &params.CodexAppServer,
	})
	// Claude reports its live model and effort through the statusline hook.
	// Codex and OpenCode have no equivalent startup callback, so seed the
	// validated launch choices immediately; their telemetry paths can replace
	// these values after an in-session model/effort change.
	if h.Name == harness.CodexName || h.Name == harness.OpenCodeName {
		if _, err := opSeedModel.call(launchString(sessionID, model)); err != nil {
			slog.Warn("failed to seed session model", "harness", h.Name, "session_id", sessionID, "error", err)
		}
		if _, err := opSeedModelID.call(launchString(sessionID, model)); err != nil {
			slog.Warn("failed to seed session model id", "harness", h.Name, "session_id", sessionID, "error", err)
		}
		if _, err := opSeedEffort.call(launchString(sessionID, effort)); err != nil {
			slog.Warn("failed to seed session effort", "harness", h.Name, "session_id", sessionID, "error", err)
		}
	}
	// Gemini's session file names each call's model, but only once the first
	// call lands. Until then show the launch choice; the usage refresh replaces
	// it with the observed model. Display only: the relaunch model is unchanged.
	if h.Name == harness.GeminiName && model != "" {
		if _, err := opSeedModel.call(launchString(sessionID, model)); err != nil {
			slog.Warn("failed to seed session model", "harness", h.Name, "session_id", sessionID, "error", err)
		}
	}
}
