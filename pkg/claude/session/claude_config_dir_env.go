package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ApplyClaudeConfigDirEnv pins a Claude Code launch's config directory to the
// harness state root, seeding the config file once from the operator's ambient
// config.
//
// Why this exists: Claude Code's account and onboarding state — oauthAccount,
// hasCompletedOnboarding, per-project trust — lives in ~/.claude.json, a file
// directly in $HOME, while everything else (OAuth tokens, settings, sessions,
// plugins) already lives under the ~/.claude state root. Under a constructed
// filesystem root that split is fatal: the launch contract binds the state
// root read-write but $HOME stays read-only scaffolding, so the top-level file
// is invisible, Claude Code treats the launch as a fresh install, and the
// detached pane parks on the login wizard. Pointing CLAUDE_CONFIG_DIR at the
// state root moves the config file — and the lock, temp and backup siblings
// Claude Code creates next to it — inside the directory every posture keeps
// writable.
//
// It is applied to EVERY tclaude-launched Claude pane rather than only to
// constructed-root ones (settled operator decision): one file for all tclaude
// agents means no state fork between an agent's sandboxed and unsandboxed
// launches, and no dependence on a root posture that launch degradation can
// re-derive after the environment is already composed. Only ambient `claude`
// runs outside tclaude keep reading the legacy top-level file.
//
// CLAUDE_CONFIG_DIR is a reserved profile environment name
// (sandboxpolicy.reservedEnvironmentNames), so no operator profile can
// conflict with this launch-owned assignment.
//
// Both launch paths route through here — session.runNew (spawn and daemon
// resume) and conv.resumeLaunchCmd (watch-mode resume) — mirroring
// ApplyAgentSocketEnv. The daemon-side scribe pre-trust reaches the same seed
// through PretrustClaudeLaunchDir.
func ApplyClaudeConfigDirEnv(harnessName string, env map[string]string) error {
	if harnessName != harness.DefaultName || env == nil {
		return nil
	}
	stateRoot, err := TclaudeLayerHarnessStateRoot(harnessName)
	if err != nil {
		return err
	}
	if err := seedClaudeConfigJSON(stateRoot); err != nil {
		return fmt.Errorf("seed Claude Code config into %s: %w", stateRoot, err)
	}
	env["CLAUDE_CONFIG_DIR"] = stateRoot
	return nil
}

// PretrustClaudeLaunchDir is the daemon-side pre-trust entry for dirs a Claude
// agent is about to be spawned into: it seeds and resolves the same relocated
// config file the launched pane will read, then records the trust entry there.
// Calling harness.EnsureClaudeDirTrusted directly would write the ambient
// ~/.claude.json — a file tclaude-launched panes no longer open.
func PretrustClaudeLaunchDir(projectDir string) error {
	env := map[string]string{}
	if err := ApplyClaudeConfigDirEnv(harness.DefaultName, env); err != nil {
		return err
	}
	return harness.EnsureClaudeDirTrustedForLaunch(
		func(name string) string { return env[name] }, projectDir)
}

// seedClaudeConfigJSON copies the operator's ambient Claude Code config into
// the state root exactly once, so a relocated-config launch starts from the
// already-logged-in ambient state instead of Claude Code's onboarding wizard.
// The ambient source honors an operator's own CLAUDE_CONFIG_DIR; without one
// it is the legacy top-level ~/.claude.json. An existing target is never
// touched — after the first copy the relocated config evolves on its own and
// the source is expected to go stale. A missing source is a clean no-op: the
// machine genuinely has no login to carry over, and the wizard the agent then
// sees is the truth. A symlinked source is followed (dotfile managers commonly
// symlink it) but must resolve to a regular file.
//
// The state root itself is ensured 0700 up front — matching the layer
// launch-state preparation — so a later trust write cannot be the first
// creator of ~/.claude with a wider default mode.
func seedClaudeConfigJSON(stateRoot string) error {
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		return err
	}
	target := filepath.Join(stateRoot, harness.ClaudeConfigJSONName)
	if _, err := os.Lstat(target); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect %s: %w", target, err)
	}
	sourceDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR"))
	if sourceDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("cannot determine home dir: %w", err)
		}
		sourceDir = home
	}
	source := filepath.Join(sourceDir, harness.ClaudeConfigJSONName)
	info, err := os.Stat(source)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", source, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file; refusing to seed from it", source)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	// CreateTemp creates 0600, which is also Claude Code's own mode for this
	// account-adjacent file.
	tmp, err := os.CreateTemp(stateRoot, harness.ClaudeConfigJSONName+".seed-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Link, not rename: two concurrent first launches can both reach this
	// point, and the loser must not replace a target the winner's pane may
	// already be evolving. Link fails with EEXIST instead of overwriting.
	if err := os.Link(tmpPath, target); err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	return nil
}
