package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Claude Code directory trust (JOH-369; generalised to the trust-dir opt-in).
//
// On first interactive launch in a directory it does not yet trust, Claude
// Code shows a "Do you trust the files in this folder?" dialog and blocks
// until the human answers. A tclaude-spawned agent runs detached in a tmux
// pane, so — like Codex's dir-trust modal (codex_dir_trust.go) — that dialog
// is a startup gate that makes the human approve a dir before the agent can
// act.
//
// Callers reach this editor through the harness-agnostic EnsureDirTrusted
// (dir_trust.go). Three paths do so, all of them narrow: the scribe-summon
// path (which spawns into a stable, daemon-created ~/.tclaude/scribe workdir),
// the explicit trust-dir opt-in (dashboard checkbox / profile trust_dir /
// `tclaude session new --trust-dir`), and tclaude's own verified default
// sibling worktrees. It is never a blanket default for an arbitrary cwd.
//
// Where Codex records dir trust in ~/.codex/config.toml, Claude Code records
// it in ~/.claude.json as a per-path project entry:
//
//	{
//	  "projects": {
//	    "/abs/path": { "hasTrustDialogAccepted": true, ... }
//	  }
//	}
//
// Claude Code is believed to trust a cwd when that dir OR AN ANCESTOR carries
// hasTrustDialogAccepted=true, rather than only on an exact entry. Treat that
// as UNCONFIRMED: it was inferred to explain why ordinary project dirs sit at
// false yet do not re-prompt, and the worked example originally recorded here
// (a hand-accepted ~/git covering every ~/git/* worktree) does not hold — real
// configs have been observed carrying no ~/git entry at all while worktrees
// beneath it still launched clean. Whatever the exact rule, what matters for
// this file is the direction it does NOT change:
//
//   - Seeding a dir's OWN entry trusts it under either rule (an exact match
//     satisfies an exact-match rule and terminates an ancestor walk), so this
//     editor is correct regardless.
//   - Merely MOVING to a different dir dodges nothing — a fresh dir with no
//     trusted ancestor is as untrusted as the old one. So for the scribe
//     (~/.tclaude/scribe, whose ancestors are not project entries) seeding is
//     the load-bearing step, not the relocation. Relocation still earns its
//     keep for the other reasons the ticket cites: out of $HOME's broad reach,
//     a stable cwd, a minimal surface.
//
// The consequence of the rule being ancestor-walking is only that some seeds
// are redundant no-ops (a dir an ancestor already covers), which the idempotent
// path handles for free. Do not tighten this editor on the strength of the
// ancestor claim without confirming it against a real Claude Code build.
//
// The seed is best-effort only, as a DEGRADATION strategy: a failure
// (unreadable / malformed / wrong-shape config) logs and the spawn proceeds,
// worst case a single one-time dialog the human clears via the pane's focus
// button.
//
// Unlike the surgical line-splice the Codex TOML editor uses, ~/.claude.json
// is a large JSON state file Claude Code owns and rewrites wholesale on nearly
// every turn, so a byte-preserving edit buys nothing (Claude Code reorders it
// on its next write regardless). This editor therefore does a full
// parse→modify→marshal round-trip, but conservatively:
//
//   - Precise: decoded with UseNumber so large integer state (epoch-ms
//     timestamps, token counters) round-trips EXACTLY, never lossily rewritten
//     as floats.
//   - Additive: every other key/value is preserved; only the one project
//     entry's hasTrustDialogAccepted is set. (Key ORDER is not preserved — Go
//     marshals maps sorted — which is immaterial for an order-independent JSON
//     file Claude Code re-serialises anyway.)
//   - Idempotent: a dir already trusted is a clean no-op — parsed but not
//     rewritten.
//   - Atomic: temp file in the same dir, fsync'd, renamed over the original
//     (shared atomicWriteFile), so a reader (or a crash mid-write) never sees
//     a partial config. On the rare summon that actually WRITES, the edit is
//     last-writer-wins against any concurrent Claude Code write in the
//     read→marshal→rename window: our rename would revert whatever CC wrote in
//     that window (a tip flag, a history entry — CC-owned churn, never our
//     trust bit). Bounded and accepted: the idempotent no-op means a dir stays
//     write-free after its first-ever seed, so this is a one-time event on a
//     rare human action, and CC exposes no lock to coordinate on. Inherent to
//     any external editor of this Claude-owned file.
//   - Fail-safe: a config whose `projects` (or the target entry) is bound to a
//     non-object is refused rather than corrupted.

// claudeConfigJSONPath returns ~/.claude.json, the global Claude Code config /
// state file that carries the per-project trust flags.
func claudeConfigJSONPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home dir: %w", err)
	}
	return filepath.Join(home, ".claude.json"), nil
}

// EnsureClaudeDirTrusted pre-trusts projectDir for Claude Code by ensuring
// ~/.claude.json carries projects[projectDir].hasTrustDialogAccepted = true.
// projectDir must be the ABSOLUTE launch cwd — the same path Claude Code keys
// its project entry on — or the entry won't match. Idempotent (already-trusted
// → no write) and atomic (temp + rename).
//
// Reached through EnsureDirTrusted (dir_trust.go), which gates on the harness.
// Callers are limited to the scribe-summon path, the explicit trust-dir opt-in
// and tclaude's verified default sibling worktrees; it is never a default for
// an arbitrary cwd.
func EnsureClaudeDirTrusted(projectDir string) error {
	path, err := claudeConfigJSONPath()
	if err != nil {
		return err
	}
	return ensureClaudeDirTrustedInFile(path, projectDir)
}

// ensureClaudeDirTrustedInFile is EnsureClaudeDirTrusted with the config path
// injected, so tests drive it against a temp file. A missing config is treated
// as empty — a minimal {"projects":{...}} is created (Claude Code fills the
// rest of its defaults on first run), matching EnsureCodexDirTrusted's
// missing-config handling.
func ensureClaudeDirTrustedInFile(configPath, projectDir string) error {
	if !filepath.IsAbs(projectDir) {
		return fmt.Errorf("claude dir-trust: project dir %q is not absolute", projectDir)
	}
	data, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read Claude config: %w", err)
	}

	changed, out, err := planClaudeDirTrust(data, projectDir)
	if err != nil {
		return err
	}
	if !changed {
		return nil // already trusted — clean no-op (idempotent)
	}

	// Preserve the existing file's mode — ~/.claude.json holds account-adjacent
	// state and is typically 0600; a user who tightened it must not have it
	// silently widened. Fall back to 0600 (not 0644) when creating it fresh,
	// matching Claude Code's own conservative default for this file.
	perm := os.FileMode(0o600)
	if fi, statErr := os.Stat(configPath); statErr == nil {
		perm = fi.Mode().Perm()
	}
	return atomicWriteFile(configPath, out, perm)
}

// planClaudeDirTrust is the pure core: given the current ~/.claude.json bytes
// and an absolute project dir, it returns whether a change is needed and the
// new bytes. No filesystem access, so it is exhaustively unit-testable.
//
//   - dir already trusted                          → (false, data, nil) no-op
//   - config empty/absent                          → create {"projects":{dir:{trust}}}
//   - projects / dir entry absent                  → add the entry, preserve the rest
//   - dir entry present, trust false/other         → set hasTrustDialogAccepted=true
//   - `projects` or the dir entry bound to a
//     non-object (would corrupt on edit)           → (false, data, err) refuse
func planClaudeDirTrust(data []byte, projectDir string) (bool, []byte, error) {
	root := map[string]any{}
	if len(bytes.TrimSpace(data)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber() // keep big ints exact across the round-trip
		if err := dec.Decode(&root); err != nil {
			return false, nil, fmt.Errorf("parse Claude config: %w", err)
		}
	}

	// `projects` must be a JSON object; absent → create it.
	var projects map[string]any
	switch p := root["projects"].(type) {
	case nil:
		projects = map[string]any{}
		root["projects"] = projects
	case map[string]any:
		projects = p
	default:
		return false, nil, fmt.Errorf("claude dir-trust: `projects` in Claude config is not an object; refusing to edit")
	}

	// The per-dir entry must be an object; absent → create it.
	var entry map[string]any
	switch e := projects[projectDir].(type) {
	case nil:
		entry = map[string]any{}
		projects[projectDir] = entry
	case map[string]any:
		entry = e
	default:
		return false, nil, fmt.Errorf("claude dir-trust: Claude config project entry %q is not an object; refusing to edit", projectDir)
	}

	// Idempotent: already trusted → no rewrite.
	if b, ok := entry["hasTrustDialogAccepted"].(bool); ok && b {
		return false, data, nil
	}
	entry["hasTrustDialogAccepted"] = true

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, nil, fmt.Errorf("encode Claude config: %w", err)
	}
	out = append(out, '\n')
	return true, out, nil
}
