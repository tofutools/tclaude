package harness

import (
	"fmt"
	"os"
	"strings"
)

// Harness-agnostic directory trust (JOH-205 inc4 Part B for Codex, JOH-369 for
// Claude Code).
//
// Both spawnable coding harnesses block a first launch in a directory they do
// not yet trust behind a modal dialog, and a tclaude-spawned agent runs
// detached in a tmux pane with no human at its TUI — so that dialog is a
// startup gate that can freeze a spawn. Each harness records trust in its own
// config file, in an unrelated shape:
//
//   - Codex       ~/.codex/config.toml   [projects."<dir>"] trust_level = "trusted"
//   - Claude Code ~/.claude.json         projects.<dir>.hasTrustDialogAccepted = true
//   - Copilot     $COPILOT_HOME/config.json   trustedFolders: ["<dir>", …]
//
// The per-harness editors live in codex_dir_trust.go / claude_dir_trust.go /
// copilot_dir_trust.go and
// share the same conservative contract (atomic, idempotent, fail-safe, refusing
// a shape they cannot edit rather than corrupting it). This file holds the two
// harness-agnostic entry points every caller should use: ResolveTrustDir to
// gate the opt-in, and EnsureDirTrusted to apply it.
//
// Neither is ever a default. Pre-trusting edits a config file tclaude does not
// own, so it happens only on an explicit opt-in — a dashboard checkbox, a spawn
// profile's trust_dir, `tclaude session new --trust-dir` — or on tclaude's own
// verified default sibling worktrees (see IsDefaultSiblingWorktree), which
// tclaude created and can therefore vouch for.

// ResolveTrustDir gates the opt-in pre-trust request against the chosen
// harness: it means something only for a harness with a directory-trust dialog
// tclaude can pre-seed (SupportsDirTrust — Claude Code and Codex), and it edits
// that harness's own config file, so requesting it for any other harness is an
// error rather than a silently dropped flag. Mirrors ResolveAutoReview: an
// unset request (false) always passes, returning false. Every caller — the
// direct `tclaude session new --trust-dir` path, the daemon spawn boundary,
// spawn-profile save and group-template validation — goes through this.
func ResolveTrustDir(h *Harness, requested bool) (bool, error) {
	if !requested {
		return false, nil
	}
	if !h.SupportsDirTrust() {
		name := "the selected harness"
		if h != nil && h.Name != "" {
			name = h.Name
		}
		return false, fmt.Errorf("--trust-dir applies only to a harness with a directory-trust dialog "+
			"(claude, codex, copilot); %s has no directory-trust prompt", name)
	}
	return true, nil
}

// DirTrustStore names the config file EnsureDirTrusted would edit for h, in
// the ~-relative spelling a human recognises. "" for a harness with no dir
// trust. It exists so UI copy ("edits ~/.claude.json") is derived from the
// harness rather than hardcoded per call site, and it deliberately shares
// EnsureDirTrusted's switch shape so adding a harness to one without the other
// is visible in a single file.
func DirTrustStore(h *Harness) string {
	if !h.SupportsDirTrust() {
		return ""
	}
	switch h.Name {
	case CodexName:
		return "~/.codex/config.toml"
	case DefaultName:
		return "~/.claude.json"
	case CopilotName:
		// Named relative to COPILOT_HOME rather than as ~/.copilot/config.json,
		// because a launch that relocates COPILOT_HOME really does move the
		// file this edits, and consent copy that named the default would be
		// wrong for exactly the operator who moved it.
		return "$COPILOT_HOME/config.json"
	default:
		return ""
	}
}

// EnsureDirTrusted pre-trusts projectDir for h by seeding that harness's own
// trust record, so a detached pane never stops on the trust dialog. projectDir
// must be the ABSOLUTE launch cwd — the path the harness keys its record on —
// or the entry won't match. Idempotent (already-trusted → no write) and atomic
// (temp + rename) for both harnesses.
//
// Callers must have cleared ResolveTrustDir first; a harness without dir trust
// is a no-op here rather than an error, so a caller that skipped the gate
// cannot accidentally write a config file for a harness that has none.
//
// Every caller treats a returned error as NON-FATAL: pre-trust is an
// optimisation over the dashboard focus button (a human can still clear the
// dialog on the pending pane), so a refusal degrades to one manual click
// instead of failing the spawn.
func EnsureDirTrusted(h *Harness, projectDir string) error {
	return EnsureDirTrustedForLaunch(h, projectDir, nil, "")
}

// EnsureDirTrustedForLaunch is EnsureDirTrusted with the LAUNCH's environment
// supplied, for the one harness whose trust store moves with it.
//
// Codex and Claude Code key their trust records on a fixed path under the
// operator's home, so their editors ignore both arguments. Copilot's store
// lives under COPILOT_HOME, which a spawn profile can relocate — and seeding
// the ambient location for a launch that reads another one writes a file the
// agent never opens, leaving the pane parked on the modal it was supposed to
// clear. getenv nil (and home "") means "the ambient environment", which is
// what every caller outside the profile-aware launch path wants.
func EnsureDirTrustedForLaunch(
	h *Harness,
	projectDir string,
	getenv func(string) string,
	home string,
) error {
	if !h.SupportsDirTrust() {
		return nil
	}
	switch h.Name {
	case CodexName:
		return EnsureCodexDirTrusted(projectDir)
	case DefaultName:
		return EnsureClaudeDirTrusted(projectDir)
	case CopilotName:
		if getenv == nil && strings.TrimSpace(home) == "" {
			return EnsureCopilotDirTrusted(projectDir)
		}
		if strings.TrimSpace(home) == "" {
			resolved, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("copilot dir-trust: cannot determine home dir: %w", err)
			}
			home = resolved
		}
		return EnsureCopilotDirTrustedForLaunch(getenv, home, projectDir)
	default:
		// A harness declared DirTrust but has no editor wired here. Refuse
		// rather than silently pretend it was trusted, so the gap surfaces at
		// the point it was introduced.
		return fmt.Errorf("dir-trust: no trust-store editor for harness %q", h.Name)
	}
}
