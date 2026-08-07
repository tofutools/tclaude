package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// GitHub Copilot CLI directory trust (TCL-973).
//
// Measured against the pinned 1.0.77 binary on a REAL pty: a launch with a
// fresh COPILOT_HOME parks on
//
//	Confirm folder trust
//	<cwd>
//	Do you trust the files in this folder?
//	 1. Yes
//	 2. Yes, and remember this folder for future sessions
//	 3. No (Esc)
//
// before it contacts the provider at all — zero provider requests, the process
// never exits. So this is the SAME startup gate Codex and Claude Code have, and
// for a detached tmux pane with no human at its TUI it is a spawn that freezes.
//
// Two properties of the measurement decide the shape of this file, and neither
// is guessable from the docs:
//
//   - The gate is invisible headlessly. `copilot -p …` with stdin a pipe never
//     shows the modal, so a fixture built on the existing headless runner
//     proves nothing about it. The scenario that covers this contract
//     (copilotfixture/dir_trust_smoke_test.go) therefore drives a real pty.
//   - NO launch flag clears it. `--allow-all-tools`, `--allow-all`, `--yolo`,
//     `--allow-all-paths` and `--add-dir` were all measured and all still park
//     on the modal. `COPILOT_ALLOW_ALL=true` does clear it, but it is a blanket
//     promotion of every tool, path and URL approval as well, so it is exactly
//     the kind of silent widening this seeding exists to avoid, and tclaude
//     never sets it. Note what that does and does NOT claim: nothing here
//     strips the variable from a launch that inherited it from the operator's
//     own environment. Refusing or clearing it belongs to the approval wave
//     that owns Copilot's env posture, not to this editor.
//
// What DOES clear it, and the only thing that does, is a pre-launch
// `trustedFolders` array in COPILOT_HOME's config.json containing the launch
// cwd. A flat `trustedFolders` key in settings.json was measured NOT to work,
// and the nested spelling that might is UNVERIFIED — so this editor writes the
// one file whose effect was actually observed, and nothing else.
//
// Which file, and why it is not the canonical one, is measured rather than
// assumed. `trustedFolders` is a CLI-MANAGED key that lives in config.json and
// stays there. Against the pinned binary:
//
//   - config.json {"trustedFolders":[…],"theme":"dark"} → after one launch,
//     `theme` has moved into settings.json (the documented user-settings
//     migration, see CopilotConfigFileName) while `trustedFolders` remains in
//     config.json alongside the CLI's own `firstLaunchAt`.
//   - settings.json {"trustedFolders":[…]} → after one launch, settings.json is
//     `{}`: the CLI DELETES the key from the file it does not own it in, and
//     the folder was never trusted at all (Phase 0's finding that the flat
//     settings.json spelling does not clear the modal).
//
// So config.json is the trust store, full stop, and this editor reads and
// writes only that file. It deliberately does NOT merge a settings.json
// `trustedFolders` into what it writes: those entries are inert where they sit,
// so promoting them into the file that IS honoured would trust directories the
// operator never actually trusted — a silent widening in a contract whose whole
// point is to seed exactly one opted-in directory.
//
// What this does NOT do, and must not be read as doing: it pre-answers a
// human's trust decision for ONE directory. It grants no tool, path or URL
// approval — those gates are separate, still enforced, and measured to remain
// enforced with a trusted folder (see docs/harnesses.md). Like the other two
// harnesses it is reached only through EnsureDirTrusted, which is never a
// default: an explicit --trust-dir / profile / dashboard opt-in, the
// daemon-created scribe workdir, or a tclaude-created default sibling worktree.

// copilotTrustedFoldersKey is the config.json key measured to clear the modal.
// Named once because the editor, its refusals and the fixture scenario all have
// to agree on it.
const copilotTrustedFoldersKey = "trustedFolders"

// EnsureCopilotDirTrusted pre-trusts projectDir for Copilot CLI using the
// AMBIENT environment's COPILOT_HOME (or $HOME/.copilot). projectDir must be
// the absolute launch cwd.
//
// Reached through EnsureDirTrusted (dir_trust.go). A launch whose environment
// relocates COPILOT_HOME must use EnsureCopilotDirTrustedForLaunch instead —
// seeding the ambient home for a launch that reads another one writes a file
// the agent never opens.
func EnsureCopilotDirTrusted(projectDir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("copilot dir-trust: cannot determine home dir: %w", err)
	}
	return EnsureCopilotDirTrustedForLaunch(os.Getenv, home, projectDir)
}

// EnsureCopilotDirTrustedForLaunch is EnsureCopilotDirTrusted with the launch's
// own environment supplied, so a spawn whose profile relocates COPILOT_HOME
// seeds the file that launch will actually read.
//
// getenv/home resolve COPILOT_HOME through the same CopilotStateDirForLaunch
// every other Copilot gate uses, including its refusal of a relative value —
// tclaude and Copilot would otherwise resolve it against different working
// directories and this editor would seed a file nobody reads.
func EnsureCopilotDirTrustedForLaunch(getenv func(string) string, home, projectDir string) error {
	if getenv == nil {
		getenv = os.Getenv
	}
	stateDir, err := CopilotStateDirForLaunch(getenv, home)
	if err != nil {
		return err
	}
	return ensureCopilotDirTrustedInHome(stateDir, projectDir)
}

// ensureCopilotDirTrustedInHome is the filesystem half, with COPILOT_HOME
// already resolved, so tests drive it against a temp directory.
func ensureCopilotDirTrustedInHome(stateDir, projectDir string) error {
	if !filepath.IsAbs(projectDir) {
		return fmt.Errorf("copilot dir-trust: project dir %q is not absolute", projectDir)
	}
	configPath := filepath.Join(stateDir, CopilotConfigFileName)
	dirs := copilotTrustSpellings(projectDir)

	// Read-modify-write under EditCopilotConfigFile's lock, not a bare atomic
	// write. The array is SHARED state: two concurrent spawns seeding different
	// directories both rewrite `trustedFolders`, and a last-writer-wins rename
	// would drop one of them — leaving that pane parked on the modal it was
	// seeded to clear. The plan runs inside the lock and is re-run from fresh
	// bytes if anything changed under it, so the two seeds compose.
	//
	// The default perm is 0600 — COPILOT_HOME sits beside a session store and
	// the CLI's own state, so a file tclaude creates there is private; an
	// existing file's mode is preserved by the editor.
	return EditCopilotConfigFile(configPath, 0o600, func(configData []byte) (bool, []byte, error) {
		return planCopilotDirTrust(configData, dirs)
	})
}

// copilotTrustSpellings returns the path spellings to seed for one launch dir:
// the cleaned absolute path, plus its symlink-resolved form when that differs.
//
// Both are seeded because the two ends of the comparison are spelled by
// different parties. tclaude receives the cwd as the operator's shell spelled
// it (on macOS, /var/folders/… for a $TMPDIR path), while Copilot records and
// compares its own resolved cwd (/private/var/folders/… for the same physical
// directory) — the exact mismatch TCL-987 fixed for the conversation store.
// Which side of that the trust check reads is not measured, and a wrong guess
// costs a frozen pane, so both spellings go in. The extra entry is inert when
// the two are equal (the common Linux case), and it is still a directory the
// operator opted in to trusting — the same physical one, named the other way.
// A dir that does not exist yet resolves to nothing, so only the given
// spelling is seeded. That is a silent degradation rather than an error
// because the launch cwd normally exists by the time this runs, and a refusal
// would fail a seed that is correct on every platform where the two spellings
// are equal.
func copilotTrustSpellings(projectDir string) []string {
	dirs := []string{filepath.Clean(projectDir)}
	if resolved, err := filepath.EvalSymlinks(dirs[0]); err == nil {
		if resolved = filepath.Clean(resolved); resolved != dirs[0] {
			dirs = append(dirs, resolved)
		}
	}
	return dirs
}

// planCopilotDirTrust is the pure core: given config.json's current bytes and
// the dir spellings to trust, it reports whether a write is needed and returns
// the new bytes.
//
//   - every dir already trusted        → (false, nil, nil) no-op
//   - file absent/empty                → create {"trustedFolders":[…]}
//   - file present                     → add/extend the key, preserve the rest
//     (including the CLI's own managed keys, e.g. firstLaunchAt)
//   - `trustedFolders` is not an array
//     of strings                       → refuse rather than overwrite
func planCopilotDirTrust(configData []byte, dirs []string) (bool, []byte, error) {
	config, err := parseCopilotSettingsObject(configData, CopilotConfigFileName)
	if err != nil {
		return false, nil, err
	}
	trusted, err := parseCopilotTrustedFolders(config[copilotTrustedFoldersKey])
	if err != nil {
		return false, nil, err
	}

	// Containment is compared on the CLEANED spelling of each existing entry
	// while the entry itself is carried over verbatim: a `/work/proj/` already
	// in the list is the same directory and must not be seeded twice, but
	// rewriting the operator's entries to tclaude's preferred spelling is not
	// this editor's business.
	existing := make([]string, 0, len(trusted))
	for _, folder := range trusted {
		existing = append(existing, filepath.Clean(strings.TrimSpace(folder)))
	}
	changed := false
	for _, dir := range dirs {
		if slices.Contains(existing, dir) {
			continue
		}
		trusted = append(trusted, dir)
		existing = append(existing, dir)
		changed = true
	}
	if !changed {
		return false, nil, nil
	}

	encoded, err := json.Marshal(trusted)
	if err != nil {
		return false, nil, fmt.Errorf("copilot dir-trust: encode %s: %w", copilotTrustedFoldersKey, err)
	}
	config[copilotTrustedFoldersKey] = json.RawMessage(encoded)

	// Every other key is carried across as its ORIGINAL bytes (RawMessage), so
	// no value in a file tclaude did not author is reformatted, re-escaped or
	// rounded. Key order is not preserved — Go marshals maps sorted — which is
	// immaterial for a JSON object the CLI rewrites on its next migration.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(config); err != nil {
		return false, nil, fmt.Errorf("copilot dir-trust: encode %s: %w", CopilotConfigFileName, err)
	}
	return true, buf.Bytes(), nil
}

// parseCopilotSettingsObject decodes one settings file into its top-level keys,
// preserving each value's raw bytes. Empty/absent is an empty object; anything
// that is not a JSON object is refused, because the alternative is replacing a
// file whose contents tclaude did not understand.
func parseCopilotSettingsObject(data []byte, name string) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	stripped := bytes.TrimSpace(stripCopilotLineComments(data))
	if len(stripped) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(stripped, &out); err != nil {
		return nil, fmt.Errorf("copilot dir-trust: cannot parse Copilot %s as a JSON object: %w; "+
			"fix the file, or clear the folder-trust prompt in the pane once", name, err)
	}
	if out == nil {
		return nil, fmt.Errorf("copilot dir-trust: cannot parse Copilot %s as a JSON object: top-level null is not an object; "+
			"fix the file, or clear the folder-trust prompt in the pane once", name)
	}
	return out, nil
}

// parseCopilotTrustedFolders reads an existing `trustedFolders` value, VERBATIM
// — the caller cleans copies for comparison rather than rewriting the
// operator's own entries. A missing key or a JSON null is an empty list. Any
// other shape is refused: overwriting it would discard folders the operator
// trusted, and tclaude cannot tell what a non-array spelling means to the CLI.
func parseCopilotTrustedFolders(raw json.RawMessage) ([]string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var folders []string
	if err := json.Unmarshal(raw, &folders); err != nil {
		return nil, fmt.Errorf("copilot dir-trust: `%s` in Copilot %s is not an array of strings; "+
			"refusing to edit it", copilotTrustedFoldersKey, CopilotConfigFileName)
	}
	return folders, nil
}
