package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
// Callers reach this editor through the harness-agnostic EnsureDirTrustedForLaunch
// (dir_trust.go). Three paths do so, all of them narrow: the scribe-summon
// path (which spawns into a stable, daemon-created ~/.tclaude/scribe workdir),
// the explicit trust-dir opt-in (dashboard checkbox / profile trust_dir /
// `tclaude session new --trust-dir`), and tclaude's own verified default
// sibling worktrees. It is never a blanket default for an arbitrary cwd.
//
// Where Codex records dir trust in ~/.codex/config.toml, Claude Code records
// it in .claude.json as a per-path project entry. For a tclaude-launched pane
// that file is <state root>/.claude.json (CLAUDE_CONFIG_DIR, see
// session.ApplyClaudeConfigDirEnv), never the top-level ~/.claude.json:
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
// Unlike the surgical line-splice the Codex TOML editor uses, .claude.json
// is a large JSON state file Claude Code owns and rewrites wholesale on nearly
// every turn, so a byte-preserving edit buys nothing (Claude Code reorders it
// on its next write regardless). This editor therefore does a full
// parse→modify→marshal round-trip, but conservatively:
//
//   - Precise: decoded with UseNumber so large integer state (epoch-ms
//     timestamps, token counters) round-trips EXACTLY, never lossily rewritten
//     as floats. Strings have no equivalent knob — an unpaired surrogate escape
//     would decode to U+FFFD — so a config carrying one is refused outright
//     (errOnLoneSurrogateEscape) rather than silently mangled. Encoding uses
//     SetEscapeHTML(false), so <, > and & are left as written instead of being
//     rewritten across a file tclaude did not author.
//   - Additive: every other key/value is preserved; only the one project
//     entry's hasTrustDialogAccepted is set. (Key ORDER is not preserved — Go
//     marshals maps sorted — which is immaterial for an order-independent JSON
//     file Claude Code re-serialises anyway.)
//   - Idempotent: a dir already trusted is a clean no-op — parsed but not
//     rewritten.
//   - Atomic and serialized: the write goes through editHarnessConfigFile,
//     the editor every other harness's dir-trust seed uses. A temp file in the
//     same dir is fsync'd and renamed over the original, so a reader (or a
//     crash mid-write) never sees a partial config. A symlinked .claude.json
//     (a dotfile manager's) is written through, not replaced. An advisory lock
//     makes concurrent tclaude spawns seeding DIFFERENT dirs compose instead of
//     dropping one entry. Claude Code does not take that lock, so after the
//     temp file is staged the editor re-reads the config and, if Claude Code
//     wrote in the meantime, re-plans on top of the new bytes. What remains is
//     the one local rename between that recheck and the commit: a Claude Code
//     write landing exactly there is still reverted. CC exposes no lock to
//     coordinate on, so that residue is inherent to any external editor of a
//     Claude-owned file; per-dir idempotence (a dir is written at most once)
//     keeps it a one-time event per directory.
//   - Fail-safe: a config whose `projects` (or the target entry) is bound to a
//     non-object is refused rather than corrupted.

// ClaudeConfigJSONName is the basename of Claude Code's global config / state
// file — the file that carries oauthAccount, onboarding state and the
// per-project trust flags. Claude Code reads it from $CLAUDE_CONFIG_DIR when
// that variable is set and from $HOME otherwise; the basename is identical in
// both locations.
const ClaudeConfigJSONName = ".claude.json"

// claudeConfigJSONPathForLaunch resolves the config file the LAUNCH will
// actually read: $CLAUDE_CONFIG_DIR/.claude.json. Every tclaude-launched
// Claude pane has CLAUDE_CONFIG_DIR pinned to its state root (see
// session.ApplyClaudeConfigDirEnv), which is also where sandboxing expects the
// file. A launch environment without it is a caller bug, and it is refused
// rather than resolved to the top-level ~/.claude.json: that file is one
// tclaude panes never open, so a trust entry written there would leave the
// pane parked on the dialog it was meant to clear — and tclaude must not edit
// the operator's own Claude config.
func claudeConfigJSONPathForLaunch(getenv func(string) string) (string, error) {
	var dir string
	if getenv != nil {
		dir = strings.TrimSpace(getenv("CLAUDE_CONFIG_DIR"))
	}
	if dir == "" {
		return "", fmt.Errorf("claude dir-trust: the launch environment sets no CLAUDE_CONFIG_DIR; " +
			"refusing to write the top-level ~/.claude.json, which tclaude-launched panes do not read")
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("claude dir-trust: CLAUDE_CONFIG_DIR %q is not absolute", dir)
	}
	return filepath.Join(filepath.Clean(dir), ClaudeConfigJSONName), nil
}

// EnsureClaudeDirTrustedForLaunch pre-trusts projectDir for Claude Code by
// ensuring the launch's config file ($CLAUDE_CONFIG_DIR/.claude.json, see
// claudeConfigJSONPathForLaunch) carries
// projects[projectDir].hasTrustDialogAccepted = true. projectDir must be the
// ABSOLUTE launch cwd — the same path Claude Code keys its project entry on —
// or the entry won't match. Idempotent (already-trusted → no write) and atomic.
//
// Callers are limited to the scribe-summon path, the explicit trust-dir opt-in
// and tclaude's verified default sibling worktrees; it is never a default for
// an arbitrary cwd.
func EnsureClaudeDirTrustedForLaunch(getenv func(string) string, projectDir string) error {
	path, err := claudeConfigJSONPathForLaunch(getenv)
	if err != nil {
		return err
	}
	return ensureClaudeDirTrustedInFile(path, projectDir)
}

// ensureClaudeDirTrustedInFile is EnsureClaudeDirTrustedForLaunch with the config path
// injected, so tests drive it against a temp file. A missing config is treated
// as empty — a minimal {"projects":{...}} is created (Claude Code fills the
// rest of its defaults on first run), matching EnsureCodexDirTrusted's
// missing-config handling.
func ensureClaudeDirTrustedInFile(configPath, projectDir string) error {
	if !filepath.IsAbs(projectDir) {
		return fmt.Errorf("claude dir-trust: project dir %q is not absolute", projectDir)
	}
	return editClaudeConfigFile(configPath, func(data []byte) (bool, []byte, error) {
		return planClaudeDirTrust(data, projectDir)
	}, prepareAtomicWriteFile)
}

// editClaudeConfigFile is the read-modify-write of the launch's .claude.json
// (<state root>/.claude.json), through the
// same editor every other harness's dir-trust seed uses (Codex, Copilot,
// Gemini, pi): an advisory lock against concurrent tclaude spawns, a symlinked
// config written THROUGH rather than replaced by a plain file, the existing
// file mode preserved, and a stale-read recheck after the temp file is staged
// so a concurrent Claude Code write is re-planned on top of instead of reverted.
//
// The default mode for a fresh file is 0600 (not 0644): .claude.json holds
// account-adjacent state, and Claude Code itself creates it 0600.
//
// prepare is injected so a test can land a concurrent write inside the
// staging window; production passes prepareAtomicWriteFile. The lock
// sidecar (.claude.json.tclaude.lock) sits beside the resolved file, inside
// the state root, where a sandboxed writer can take it too.
func editClaudeConfigFile(
	configPath string,
	plan func([]byte) (bool, []byte, error),
	prepare func(string, []byte, os.FileMode) (*atomicFileReplacement, error),
) error {
	return editHarnessConfigFile("Claude config", configPath, 0o600, plan, prepare)
}

// planClaudeDirTrust is the pure core: given the current .claude.json bytes
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
		// A lone surrogate escape does NOT survive decode→marshal: Go replaces
		// it with U+FFFD, irreversibly. UseNumber protects integers but there is
		// no equivalent knob for strings, so the only safe move on a config
		// carrying one is to leave the file alone — same fail-safe posture as a
		// wrong-shape `projects`. (JSON.stringify emits these only for a split
		// surrogate pair, so a real config is very unlikely to trip this.)
		if err := errOnLoneSurrogateEscape(data); err != nil {
			return false, nil, err
		}
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

	// json.Marshal escapes <, > and & as </>/& by default, which
	// would rewrite those characters throughout a file tclaude did not author
	// (harmless but gratuitous churn in a diff the operator may well read).
	// Encoder + SetEscapeHTML(false) emits them verbatim; it also appends the
	// trailing newline MarshalIndent does not.
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return false, nil, fmt.Errorf("encode Claude config: %w", err)
	}
	return true, buf.Bytes(), nil
}

// errOnLoneSurrogateEscape reports an error when data contains a \uXXXX escape
// that is an UNPAIRED surrogate — a high surrogate (D800-DBFF) not immediately
// followed by a low one, or a low surrogate (DC00-DFFF) not immediately
// preceded by a high one. Such an escape cannot survive Go's decode→encode
// round-trip (it becomes U+FFFD), so the caller refuses the edit rather than
// silently corrupting the operator's config.
//
// Properly PAIRED surrogate escapes round-trip fine and are deliberately
// allowed. Scanning tracks string context so a `\u` inside a comment-free JSON
// document is only read where it can actually be an escape, and consumes
// backslash pairs so a literal `\\u0041` is not mistaken for an escape.
func errOnLoneSurrogateEscape(data []byte) error {
	isHigh := func(r rune) bool { return r >= 0xD800 && r <= 0xDBFF }
	isLow := func(r rune) bool { return r >= 0xDC00 && r <= 0xDFFF }

	inString := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			esc, width, ok := parseUnicodeEscape(data, i)
			if !ok {
				i++ // an ordinary two-byte escape (\" \\ \n …); skip its payload
				continue
			}
			if isHigh(esc) {
				// A valid pair is \uD8xx immediately followed by \uDCxx.
				if next, nextWidth, nextOK := parseUnicodeEscape(data, i+width); nextOK && isLow(next) {
					i += width + nextWidth - 1
					continue
				}
				return fmt.Errorf("claude dir-trust: Claude config contains an unpaired high-surrogate escape (\\u%04X) that would be corrupted by a rewrite; refusing to edit", esc)
			}
			if isLow(esc) {
				return fmt.Errorf("claude dir-trust: Claude config contains an unpaired low-surrogate escape (\\u%04X) that would be corrupted by a rewrite; refusing to edit", esc)
			}
			i += width - 1
		}
	}
	return nil
}

// parseUnicodeEscape decodes a `\uXXXX` escape starting at data[i] (which must
// be the backslash), returning the code unit and the escape's byte width.
// ok=false when data[i:] is not a well-formed \u escape — including an
// ordinary escape like \n, which the caller handles separately.
func parseUnicodeEscape(data []byte, i int) (esc rune, width int, ok bool) {
	if i+5 >= len(data) || data[i] != '\\' || data[i+1] != 'u' {
		return 0, 0, false
	}
	var v rune
	for _, b := range data[i+2 : i+6] {
		switch {
		case b >= '0' && b <= '9':
			v = v<<4 | rune(b-'0')
		case b >= 'a' && b <= 'f':
			v = v<<4 | rune(b-'a'+10)
		case b >= 'A' && b <= 'F':
			v = v<<4 | rune(b-'A'+10)
		default:
			return 0, 0, false
		}
	}
	return v, 6, true
}
