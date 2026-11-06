package session

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// refuseStateDirCwdUnderWorkspaceWrite refuses a launch whose writable
// sandbox would make tclaude's, Codex's or Claude's state dirs writable.
func refuseStateDirCwdUnderWorkspaceWrite(h *harness.Harness, params *NewParams, harnessBuiltinMode, cwd string) error {
	// Sandbox cwd-safety guard: a writable sandbox (Codex workspace-write)
	// confines writes to the cwd subtree, so a cwd at/above $HOME would make
	// ~/.tclaude / ~/.codex / ~/.claude writable and defeat the protection.
	// Refuse that rather than spawn an agent with a false sense of
	// containment. No-op for harnesses/modes that don't write outside cwd.
	// The managed tclaude-agent profile extends :workspace (same cwd-subtree
	// writability), so guard it exactly as workspace-write.
	guardMode := harnessBuiltinMode
	if params.PermissionProfile == harness.CodexAgentProfile {
		guardMode = harness.SandboxWorkspaceWrite
	}
	if home, herr := os.UserHomeDir(); herr == nil && harness.CodexSandboxCwdConflict(guardMode, cwd, home) {
		return fmt.Errorf("refusing to launch a %s agent in %q under workspace-write containment (%s): "+
			"that cwd contains your tclaude/Codex/Claude state dirs, which the sandbox would make writable "+
			"(defeating it). Run the agent from a project subdirectory, or use sandbox %s to opt out of the sandbox",
			h.Name, cwd, sandboxDescr(harnessBuiltinMode, params.PermissionProfile), harness.SandboxDangerFull)
	}
	return nil
}

// discloseSpawnPosture prints, on stderr, what the harness's own sandbox
// does and does not confine for this launch.
func discloseSpawnPosture(h *harness.Harness, approvalPolicy, harnessBuiltinMode, cwd string, outerLayer bool) {
	// Spawn-posture warnings: the Claude unsandboxed-autonomy pairing (TCL-586)
	// and OpenCode's look-like-a-sandbox access-control mode. Unlike the daemon
	// spawn path this one applies no approval default, so the Claude case fires
	// only on a posture the human asked for by name OR one a --resume replayed
	// from the conversation's own record — never on a tclaude-invented default.
	// A human who typed `--permission-mode auto`, and equally one who typed a
	// bare `-r` on a conversation recorded under it, deserves to know tclaude
	// found nothing that confines it; applyRecordedLaunchPosture separately
	// echoes which postures the resume carried. It stays a warning on stderr,
	// never a refusal: the human is the trust root here.
	if !outerLayer || h.Name == harness.OpenCodeName {
		for _, info := range harness.SpawnSandboxInfo(h, harnessBuiltinMode) {
			fmt.Fprintf(cliStderr, "ℹ %s\n", info)
		}
		for _, warning := range harness.SpawnSandboxWarnings(h, approvalPolicy, harnessBuiltinMode, cwd, outerLayer) {
			fmt.Fprintf(cliStderr, "%s\n", warning)
		}
	}
}

// prepareLaunchDirTrust enforces the Copilot API drive's folder-trust gate
// and, when the operator opted in with --trust-dir, pre-trusts the launch
// directory in the harness's own trust store before the pane starts.
func prepareLaunchDirTrust(h *harness.Harness, params *NewParams, launchSandbox *sandboxpolicy.Snapshot, additionalEnv map[string]string, cwd string) error {
	// Pre-trust the launch dir when the operator opted in (--trust-dir), BEFORE
	// the pane starts: each harness reads its trust store at startup, so the
	// entry must already be there or the agent freezes on the trust-folder
	// dialog (JOH-205 for Codex, JOH-369 for Claude Code, TCL-973 for Copilot,
	// whose modal blocks before the CLI contacts the provider at all). Opt-in
	// only (the early gate guarantees the harness has a trust store);
	// EnsureDirTrustedForLaunch dispatches to the right editor and is atomic +
	// idempotent.
	//
	// The LAUNCH's environment is handed over rather than the ambient one for
	// the same reason the Copilot sandbox gate above reads it: Copilot's trust
	// store lives under COPILOT_HOME, so a profile that relocates it moves the
	// file that has to carry the entry, and seeding tclaude's own home would
	// leave the pane parked on the modal. The two harnesses whose stores sit at
	// a fixed path under the operator's home ignore it.
	//
	// Best-effort: pre-trust is an optimisation over the focus-button fallback
	// — if it fails (an FS error, or a config shape the editor refuses to touch
	// rather than corrupt), the agent still launches and the operator can clear
	// the trust dialog on the pending pane via the dashboard focus button
	// (Part A). So warn and continue rather than fail the spawn.
	// The API drive's own folder-trust gate, and the backstop for every launch
	// path: agentd refuses earlier so the operator gets a named spawn failure,
	// but relaunch, clone and the direct `tclaude session new --copilot-api`
	// call all arrive here, so this is the one place the invariant is total.
	//
	// Placed immediately BEFORE the seeding block rather than after it, because
	// the two answer the same question and the seed is what makes the check
	// pass: a --trust-dir launch is admitted on its promise to seed, and the
	// seed below is that promise being kept.
	{
		var launchEnvironment []sandboxpolicy.EnvironmentEntry
		if launchSandbox != nil {
			launchEnvironment = sandboxpolicy.EnvironmentForLaunch(launchSandbox)
		}
		if err := ValidateCopilotAPIFolderTrust(
			h, params.CopilotAPI, params.TrustDir,
			strings.TrimSpace(params.Resume) != "", cwd, launchEnvironment,
		); err != nil {
			return err
		}
	}

	if params.TrustDir {
		var launchEnvironment []sandboxpolicy.EnvironmentEntry
		if launchSandbox != nil {
			launchEnvironment = sandboxpolicy.EnvironmentForLaunch(launchSandbox)
		}
		trustEnv := launchModelEnvironment(launchEnvironment)
		if configDir, ok := additionalEnv["CLAUDE_CONFIG_DIR"]; ok {
			// Launch-owned, never profile-authored (the name is reserved): a
			// constructed-root Claude launch relocates its config file into the
			// state root, and the trust entry must land in the file that launch
			// will actually read. See ApplyClaudeConfigDirEnv.
			trustEnv["CLAUDE_CONFIG_DIR"] = configDir
		}
		if err := harness.EnsureDirTrustedForLaunch(h, cwd,
			func(name string) string { return trustEnv[name] },
			strings.TrimSpace(trustEnv["HOME"]),
		); err != nil {
			slog.Warn("could not pre-trust the launch dir; the trust-folder dialog may appear — clear it via the dashboard focus button",
				"harness", h.Name, "cwd", cwd, "err", err)
		}
	}
	return nil
}
