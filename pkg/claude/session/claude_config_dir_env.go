package session

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ApplyClaudeConfigDirEnv pins a constructed-root Claude Code launch's config
// directory to the harness state root, seeding the config file once from the
// legacy top-level ~/.claude.json.
//
// Why this exists: a constructed filesystem root binds the state root
// (~/.claude) read-write, but $HOME itself is read-only scaffolding. Claude
// Code's account and onboarding state — oauthAccount, hasCompletedOnboarding,
// per-project trust — lives in ~/.claude.json, a file directly in $HOME (the
// OAuth tokens in ~/.claude/.credentials.json are already inside the state
// root). With that file invisible, Claude Code treats the launch as a fresh
// install and parks the detached pane on the login wizard; and even a
// completed login could not persist into a read-only $HOME. Pointing
// CLAUDE_CONFIG_DIR at the state root moves the config file — and its lock,
// temp and backup siblings, which Claude Code creates next to it — inside the
// directory the launch contract already keeps writable.
//
// CLAUDE_CONFIG_DIR is a reserved profile environment name
// (sandboxpolicy.reservedEnvironmentNames), so no operator profile can
// conflict with this launch-owned assignment.
//
// Both launch paths route through here — session.runNew (spawn and daemon
// resume) and conv.resumeLaunchCmd (watch-mode resume) — mirroring
// ApplyAgentSocketEnv. Deliberately gated on the CONSTRUCTED root posture
// only: a host-inherited root sees the real ~/.claude.json, and relocating the
// config for launches that never lost it would fork their state for no gain.
func ApplyClaudeConfigDirEnv(
	harnessName string,
	constructedRoot bool,
	env map[string]string,
) error {
	if harnessName != harness.DefaultName || !constructedRoot || env == nil {
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

// seedClaudeConfigJSON copies the operator's legacy top-level ~/.claude.json
// into the state root exactly once, so a relocated-config launch starts from
// the already-logged-in ambient state instead of Claude Code's onboarding
// wizard. An existing target is never touched — after the first copy the
// relocated config evolves on its own and the two files are expected to
// diverge. A missing legacy file is a clean no-op: the machine genuinely has
// no login to carry over, and the wizard the agent then sees is the truth. A
// symlinked legacy config is refused rather than resolved, mirroring the
// OpenCode credential copy's contract.
func seedClaudeConfigJSON(stateRoot string) error {
	target := filepath.Join(stateRoot, harness.ClaudeConfigJSONName)
	if _, err := os.Lstat(target); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect %s: %w", target, err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot determine home dir: %w", err)
	}
	legacy := filepath.Join(home, harness.ClaudeConfigJSONName)
	info, err := os.Lstat(legacy)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", legacy, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file; refusing to seed from it", legacy)
	}
	data, err := os.ReadFile(legacy)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
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
