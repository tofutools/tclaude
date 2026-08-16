package agentd

import (
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// codexFastModeAtLaunch resolves the effective state a launch receives while
// preserving the distinction between explicit intent and inherited runtime
// evidence. A nil result means the main config could not be inspected.
func codexFastModeAtLaunch(mode, stateRoot string) *bool {
	var fast bool
	switch strings.TrimSpace(mode) {
	case harness.FastModeOn:
		fast = true
	case harness.FastModeOff:
		fast = false
	default:
		var err error
		fast, err = harness.CodexMainConfigFastMode(stateRoot)
		if err != nil {
			slog.Debug("Codex launch Fast-mode baseline unavailable", "error", err,
				"codex_state_root", stateRoot)
			return nil
		}
	}
	return &fast
}

func persistCodexFastModeAtLaunch(convID string, observed *bool) {
	if err := db.SetAgentFastModeAtLaunchForConv(convID, observed); err != nil {
		slog.Warn("record Codex launch Fast-mode baseline failed", "error", err,
			"conv_id", convID)
	}
}

// codexFastModeFromFollowerNow bypasses the dashboard's one-second presentation
// throttle while reusing the same incremental follower and cursor. It never
// reparses an unchanged prefix: RuntimeTelemetry stats the memoized rollout and
// consumes only complete records appended since the follower's last read.
func codexFastModeFromFollowerNow(sess *db.SessionRow) (fast, known bool, err error) {
	if sess == nil || sess.ID == "" || sess.ConvID == "" || sess.Harness != harness.CodexName {
		return false, false, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return false, false, err
	}

	codexContextRefreshMu.Lock()
	if codexContextRefreshMu.last == nil {
		codexContextRefreshMu.last = map[string]codexReadThroughSnapshot{}
	}
	cached := codexContextRefreshMu.last[sess.ID]
	if cached.follower == nil {
		cached.follower = &harness.CodexTelemetryFollower{}
		codexContextRefreshMu.last[sess.ID] = cached
	}
	follower := cached.follower
	codexContextRefreshMu.Unlock()

	snap, err := follower.RuntimeTelemetry(home, sess.ConvID)
	if err != nil {
		return false, false, err
	}
	fast, known = codexFastModeForSession(snap, sess)

	if !known {
		profile, profileErr := db.AgentRelaunchProfileForConv(sess.ConvID)
		if profileErr != nil {
			return false, false, profileErr
		}
		if profile != nil && profile.FastMode != nil {
			fast, known = *profile.FastMode, true
		} else if profile != nil && profile.FastModeAtLaunch != nil {
			fast, known = *profile.FastModeAtLaunch, true
		}
	}
	if !known {
		// Inherit deliberately records no launch override. Ask Codex's own
		// merged config reader for a best-effort baseline instead; a later live
		// thread_settings_applied event supersedes it.
		environment, permissionProfile, boundaryErr := codexFastModeProbeBoundary(sess)
		if boundaryErr != nil {
			slog.Debug("Codex inherited Fast mode launch boundary unavailable", "error", boundaryErr,
				"conv_id", sess.ConvID)
		}
		inherited, inheritedKnown, inheritedErr := session.CodexEffectiveFastMode(
			sess.Cwd, environment, permissionProfile)
		if inheritedErr != nil {
			slog.Debug("Codex inherited Fast mode probe failed", "error", inheritedErr,
				"conv_id", sess.ConvID, "cwd", sess.Cwd)
		} else if inheritedKnown {
			fast, known = inherited, true
		}

		// config/read may take long enough for the live thread to change while
		// it runs. Re-read the incremental follower after the probe so any
		// current-generation settings event wins before we cache or act.
		latest, latestErr := follower.RuntimeTelemetry(home, sess.ConvID)
		if latestErr != nil {
			return false, false, latestErr
		}
		if liveFast, liveKnown := codexFastModeForSession(latest, sess); liveKnown {
			fast, known = liveFast, true
		}
	}

	// Keep the ordinary snapshot read-through cache coherent, but do not move
	// its refresh timestamp: this focused proof does not perform the context,
	// effort, usage, cost, or checkpoint persistence work of a dashboard tick.
	codexContextRefreshMu.Lock()
	cached = codexContextRefreshMu.last[sess.ID]
	if cached.follower == follower {
		cached.sessionConvID = sess.ConvID
		cached.sessionCreatedAt = sess.CreatedAt
		cached.runtimeFastMode = fast
		cached.runtimeHasFastMode = known
		codexContextRefreshMu.last[sess.ID] = cached
	}
	codexContextRefreshMu.Unlock()
	return fast, known, nil
}

func codexFastModeProbeBoundary(
	sess *db.SessionRow,
) (environment []sandboxpolicy.EnvironmentEntry, permissionProfile string, err error) {
	if sess == nil {
		return nil, "", nil
	}
	if sess.EffectiveSandbox != nil {
		environment = append(environment, sess.EffectiveSandbox.Effective.Environment...)
	}
	relaunch, relaunchErr := db.AgentRelaunchProfileForConv(sess.ConvID)
	if relaunchErr != nil {
		return environment, "", relaunchErr
	}
	if relaunch != nil && relaunch.CodexStateRoot != nil &&
		strings.TrimSpace(*relaunch.CodexStateRoot) != "" {
		// The launch wrapper freezes the resolved Codex state directory even
		// when HOME, rather than CODEX_HOME, originally selected it. Pinning
		// CODEX_HOME to that exact root recreates the same config boundary.
		filtered := environment[:0]
		for _, entry := range environment {
			if entry.Name != "CODEX_HOME" {
				filtered = append(filtered, entry)
			}
		}
		environment = append(filtered, sandboxpolicy.EnvironmentEntry{
			Name: "CODEX_HOME", Value: strings.TrimSpace(*relaunch.CodexStateRoot),
		})
	}
	profileForGeneration := func(generation string) (string, error) {
		profile, profileErr := db.GetCodexNativePermissionProfile(generation)
		if profileErr != nil || profile == nil {
			return "", profileErr
		}
		return profile.ProfileName, nil
	}
	if runtime, runtimeErr := db.GetCodexAppServerRuntimeByConvID(sess.ConvID); runtimeErr != nil {
		return environment, "", runtimeErr
	} else if runtime != nil && runtime.LaunchID == sess.ID {
		profile, profileErr := profileForGeneration(runtime.Generation)
		if profileErr != nil {
			return environment, "", profileErr
		}
		if profile != "" {
			return environment, profile, nil
		}
	}
	identity, identityErr := db.GetSessionExitLaunchIdentity(sess.ID)
	if identityErr != nil {
		return environment, "", identityErr
	}
	if identity.Generation != "" {
		profile, profileErr := profileForGeneration("launch:" + identity.Generation)
		if profileErr != nil {
			return environment, "", profileErr
		}
		if profile != "" {
			return environment, profile, nil
		}
	}
	// User-authored Codex profiles are persisted in the historical sandbox_mode
	// column. Built-in sandbox values are flags rather than profile names.
	switch sess.HarnessBuiltinMode {
	case "", harness.SandboxReadOnly, harness.SandboxWorkspaceWrite, harness.SandboxDangerFull:
		return environment, "", nil
	default:
		return environment, sess.HarnessBuiltinMode, nil
	}
}

// dashboardSetCodexFastModeAgent re-proves live Fast state immediately before
// sending Codex's no-argument toggle. Codex currently exposes no directional
// /fast command, so a manual toggle in the tiny interval between this proof
// and pane delivery can still race; callers explicitly accept that limitation.
func dashboardSetCodexFastModeAgent(w http.ResponseWriter, _ *http.Request, convSelector string, desired bool) {
	resolved, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "resolve agent: "+err.Error())
		return
	}
	if afterResolveCodexFastModeForTest != nil {
		afterResolveCodexFastModeForTest()
	}
	launchLock := resumeLaunchLock(resolved.ConvID)
	launchLock.Lock()
	defer launchLock.Unlock()
	if resolved.AgentID != "" {
		if err := requireCurrentAgentGeneration(resolved.AgentID, resolved.ConvID); err != nil {
			writeError(w, http.StatusConflict, "stale_generation", err.Error())
			return
		}
	}
	sess := aliveSessionForConv(resolved.ConvID)
	if sess == nil {
		writeError(w, http.StatusConflict, "offline", "agent has no live tmux session")
		return
	}
	h, err := harness.Resolve(sess.Harness)
	if err != nil || h.Life == nil || h.Life.FastModeCommand() == "" {
		writeError(w, http.StatusConflict, "unsupported", "agent harness does not support the Fast mode action")
		return
	}
	fast, known, err := codexFastModeFromFollowerNow(sess)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "read live Codex Fast mode: "+err.Error())
		return
	}
	if !known {
		// /fast is only a toggle. When both telemetry and the inherited-config
		// probe are unavailable, honor the operator's requested direction by
		// assuming the opposite current state. Codex's next settings readback
		// repairs the dashboard if that guess was wrong.
		fast = !desired
	}
	if fast == desired {
		state := "off"
		if desired {
			state = "on"
		}
		writeError(w, http.StatusConflict, "already_"+state, "Codex Fast mode is already "+state)
		return
	}
	if !injectCodexFastModeToggle(sess, h.Life.FastModeCommand(), desired) {
		writeError(w, http.StatusConflict, "delivery_failed", "could not deliver the Fast mode toggle to the live Codex pane")
		return
	}

	// Allow the next dashboard poll to inspect the rollout immediately rather
	// than waiting out a presentation-cache interval that began before the
	// command was injected.
	codexContextRefreshMu.Lock()
	cached := codexContextRefreshMu.last[sess.ID]
	cached.at = time.Time{}
	codexContextRefreshMu.last[sess.ID] = cached
	codexContextRefreshMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"conv_id":   resolved.ConvID,
		"requested": map[bool]string{true: "fast_on", false: "fast_off"}[desired],
	})
}

// injectCodexFastModeToggle deliberately uses the pane for both Codex drives.
// Codex 0.147's stable app-server protocol has no settings mutation for an
// already-running thread; /fast remains a TUI command even when app-server
// owns turns. Keep this exact compile-time token path separate from the generic
// lifecycle dispatcher, whose app-server branch must continue to fail closed
// for commands without typed RPC equivalents.
func injectCodexFastModeToggle(sess *db.SessionRow, command string, desired bool) bool {
	target := sess.TmuxSession + ":0.0"
	if err := injectTextAndSubmit(target, command); err != nil {
		slog.Warn("Codex Fast mode inject failed", "error", err, "tmux", sess.TmuxSession,
			"conv_id", sess.ConvID, "desired", desired)
		return false
	}
	slog.Info("Codex Fast mode toggle injected via send-keys", "tmux_session", sess.TmuxSession,
		"conv_id", sess.ConvID, "desired", desired)
	return true
}

// afterResolveCodexFastModeForTest is a deterministic seam for proving that
// stable-agent generation changes are rejected after selector resolution.
var afterResolveCodexFastModeForTest func()
