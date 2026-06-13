package statusbar

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Codex CLI status-line install.
//
// Unlike Claude Code — whose `statusLine` runs a command and pipes session
// JSON to its stdin (which is how tclaude installs its own `status-bar`
// renderer) — Codex CLI has NO command-backed status line. Codex renders its
// TUI footer internally from a fixed enum of built-in item identifiers listed
// in ~/.codex/config.toml under `[tui] status_line`. The command-backed
// variant is still an open feature request (openai/codex#17827), so tclaude
// cannot install its ANSI renderer for Codex.
//
// What tclaude CAN do today is curate a sensible default of Codex's *built-in*
// items so a tclaude-set-up Codex session shows a useful footer mirroring the
// information CC's statusbar surfaces (model, context, git, limits). When the
// command-backed feature ships, a command path can be added that reuses the
// existing `status-bar` renderer.
//
// This is the Codex satisfaction of the future StatuslineInstaller seam
// (JOH-150): one contract, CC = command-renderer, Codex = native-items config.

// codexStatusLineItems is tclaude's curated default Codex status line. Each
// string is one of Codex's canonical kebab-case `StatusLineItem` identifiers
// (the enum uses serialize_all = "kebab_case"). Codex silently ignores an
// unknown identifier, so a typo here would render nothing — TestCodexDefaultItemsAreValid
// pins every entry against the known-valid set. Each item is conditionally
// shown by Codex (git only in a repo, limits only once known, thread-id once a
// session starts), so the footer degrades gracefully when data is absent.
var codexStatusLineItems = []string{
	"model-with-reasoning", // model name + reasoning level (≈ CC's model + 🧠 effort)
	"context-remaining",    // % of context window left (≈ CC's context bar)
	"git-branch",           // current branch (≈ CC's [branch])
	"five-hour-limit",      // primary rate-limit headroom (≈ CC's 5h bar)
	"weekly-limit",         // secondary rate-limit headroom (≈ CC's 7d bar)
	"thread-id",            // session/thread id
}

// codexManagedMarker is the comment tclaude writes immediately above a
// status_line it manages. Its presence means the value is tclaude-owned and
// may be repaired/replaced; its absence means the user owns the value and
// tclaude must never clobber it. Codex ignores TOML comments, so this is
// invisible to Codex itself. Detection matches on codexManagedPrefix so a
// user editing the parenthetical text doesn't lose the "managed" marking.
const (
	codexManagedPrefix = "# tclaude:managed-status-line"
	codexManagedMarker = codexManagedPrefix + " (delete this line to manage the Codex status line yourself)"
)

// CodexInstallOutcome describes what InstallCodex did, so the caller can print
// an accurate message (and so a user-owned value is reported, not clobbered).
type CodexInstallOutcome int

const (
	// CodexAlreadyInstalled: a tclaude-managed status_line matching the
	// current curated default is already present — nothing was written.
	CodexAlreadyInstalled CodexInstallOutcome = iota
	// CodexInstalled: there was no status_line, so the curated default was added.
	CodexInstalled
	// CodexRepaired: a tclaude-managed but stale status_line was replaced with
	// the current curated default.
	CodexRepaired
	// CodexUserManaged: the user has their own (non-tclaude) status_line —
	// tclaude left it untouched.
	CodexUserManaged
)

// CodexConfigPath returns ~/.codex/config.toml, or "" if the home dir is unknown.
func CodexConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "config.toml")
}

// CheckCodexInstalled reports whether the current curated default is installed
// as a tclaude-managed status_line in ~/.codex/config.toml.
func CheckCodexInstalled() bool {
	path := CodexConfigPath()
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	sc := scanCodexConfigData(data)
	return sc.present && sc.managed && sc.current
}

// CodexStatusLineUserManaged reports whether ~/.codex/config.toml has a
// status_line that the user (not tclaude) owns — used by `tclaude setup
// --check` to explain why tclaude isn't managing it.
func CodexStatusLineUserManaged() bool {
	path := CodexConfigPath()
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	sc := scanCodexConfigData(data)
	return sc.present && !sc.managed
}

// InstallCodex writes (or repairs) tclaude's curated status_line into
// ~/.codex/config.toml, mirroring install.go's idempotent + repair semantics:
//   - no status_line               → add the curated default       (CodexInstalled)
//   - tclaude-managed + current    → no-op                          (CodexAlreadyInstalled)
//   - tclaude-managed + stale      → replace with the curated default (CodexRepaired)
//   - user-owned status_line       → leave it untouched             (CodexUserManaged)
//
// The user's other keys and comments are preserved: edits are surgical (splice
// specific lines), never a parse→re-marshal round-trip, which is the only way
// to keep a hand-written TOML file's comments and ordering intact.
func InstallCodex() (CodexInstallOutcome, error) {
	path := CodexConfigPath()
	if path == "" {
		return 0, fmt.Errorf("cannot determine Codex config path (no home dir)")
	}

	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("read Codex config: %w", err)
	}

	outcome, newData := planCodexStatusLine(data)
	if outcome == CodexAlreadyInstalled || outcome == CodexUserManaged {
		return outcome, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, fmt.Errorf("create .codex dir: %w", err)
	}
	if err := os.WriteFile(path, newData, 0o644); err != nil {
		return 0, fmt.Errorf("write Codex config: %w", err)
	}
	return outcome, nil
}

// codexScan is tclaude's view of the status_line key in a Codex config.
type codexScan struct {
	present   bool // an active (non-comment) tui.status_line assignment exists
	managed   bool // that assignment is tclaude-managed (preceded by the marker)
	current   bool // the managed value matches codexStatusLineItems exactly
	keyStart  int  // line index of the status_line key (-1 if absent)
	keyEnd    int  // line index of the array's closing ']' (inclusive)
	tuiHeader int  // line index of the first [tui] table header (-1 if absent)
}

func scanCodexConfigData(data []byte) codexScan {
	lines, _ := toLines(data)
	return scanCodexConfig(lines)
}

// scanCodexConfig walks the config line-by-line, tracking the current table so
// a bare `status_line` is attributed to `tui.status_line` only inside [tui]
// (and the dotted top-level `tui.status_line` form is also recognised). Only
// the first active binding is considered (TOML forbids duplicate keys).
func scanCodexConfig(lines []string) codexScan {
	sc := codexScan{keyStart: -1, keyEnd: -1, tuiHeader: -1}
	currentTable := ""
	prevNonBlank := ""

	for i, raw := range lines {
		t := strings.TrimSpace(raw)
		if t == "" {
			continue // blank lines don't break marker adjacency
		}
		if strings.HasPrefix(t, "#") {
			prevNonBlank = t
			continue
		}
		if strings.HasPrefix(t, "[") {
			currentTable = tomlTableName(t)
			if currentTable == "tui" && sc.tuiHeader == -1 {
				sc.tuiHeader = i
			}
			prevNonBlank = t
			continue
		}

		if sc.keyStart == -1 {
			if lhs, rhs, found := strings.Cut(t, "="); found {
				left := strings.TrimSpace(lhs)
				isTui := (currentTable == "tui" && left == "status_line") ||
					(currentTable == "" && left == "tui.status_line")
				if isTui {
					sc.present = true
					sc.keyStart = i
					sc.managed = strings.HasPrefix(prevNonBlank, codexManagedPrefix)

					// Accumulate the value across lines until the array closes,
					// so a multi-line array is replaced as a whole on repair.
					val := rhs
					j := i
					for !strings.Contains(val, "]") && j+1 < len(lines) {
						j++
						val += " " + strings.TrimSpace(lines[j])
					}
					sc.keyEnd = j
					if sc.managed {
						sc.current = equalStrings(parseTomlStringArray(val), codexStatusLineItems)
					}
				}
			}
		}
		prevNonBlank = t
	}
	return sc
}

// planCodexStatusLine decides the outcome and returns the new file bytes (the
// original bytes when the outcome is a no-op). Pure: no filesystem access, so
// it's exhaustively unit-testable.
func planCodexStatusLine(data []byte) (CodexInstallOutcome, []byte) {
	lines, sep := toLines(data)
	sc := scanCodexConfig(lines)

	switch {
	case sc.present && !sc.managed:
		return CodexUserManaged, data
	case sc.present && sc.managed && sc.current:
		return CodexAlreadyInstalled, data
	}

	keyLine := formatCodexStatusLine(codexStatusLineItems)

	// Repair: replace the managed (stale) key — possibly multi-line — in place.
	// The marker line sits above keyStart and is preserved untouched.
	if sc.present && sc.managed {
		out := append([]string{}, lines[:sc.keyStart]...)
		out = append(out, keyLine)
		out = append(out, lines[sc.keyEnd+1:]...)
		return CodexRepaired, fromLines(out, sep)
	}

	// Install: no status_line present. Prefer inserting into an existing [tui]
	// table (right after its header, before any keys/subtables); otherwise
	// append a fresh [tui] block at EOF.
	if sc.tuiHeader >= 0 {
		out := append([]string{}, lines[:sc.tuiHeader+1]...)
		out = append(out, codexManagedMarker, keyLine)
		out = append(out, lines[sc.tuiHeader+1:]...)
		return CodexInstalled, fromLines(out, sep)
	}

	out := append([]string{}, lines...)
	if len(out) > 0 {
		out = append(out, "") // blank separator before our block
	}
	out = append(out, "[tui]", codexManagedMarker, keyLine)
	return CodexInstalled, fromLines(out, sep)
}

// formatCodexStatusLine renders the single-line `status_line = [...]` TOML key.
// Always single-line so repair is a clean one-line splice.
func formatCodexStatusLine(items []string) string {
	quoted := make([]string, len(items))
	for i, it := range items {
		quoted[i] = fmt.Sprintf("%q", it)
	}
	return "status_line = [" + strings.Join(quoted, ", ") + "]"
}

// toLines splits config bytes into lines (\r stripped), returning the dominant
// line separator so it round-trips. A single trailing newline is dropped here
// and re-added by fromLines, normalising the file to end in exactly one
// newline. Empty input yields no lines.
func toLines(data []byte) (lines []string, sep string) {
	sep = "\n"
	s := string(data)
	if strings.Contains(s, "\r\n") {
		sep = "\r\n"
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil, sep
	}
	return strings.Split(s, "\n"), sep
}

func fromLines(lines []string, sep string) []byte {
	return []byte(strings.Join(lines, sep) + sep)
}

// tomlTableName extracts the table name from a header line, e.g. "[tui]" →
// "tui", "[[hooks.X]]" → "hooks.X". Strips a trailing inline comment.
func tomlTableName(t string) string {
	t, _, _ = strings.Cut(t, "#")
	t = strings.TrimSpace(t)
	t = strings.TrimPrefix(t, "[")
	t = strings.TrimPrefix(t, "[")
	t = strings.TrimSuffix(t, "]")
	t = strings.TrimSuffix(t, "]")
	return strings.TrimSpace(t)
}

// parseTomlStringArray extracts the string elements of a (possibly multi-line)
// TOML array value such as `["model", "git-branch"]`. Good enough for the
// status_line shape (a flat array of identifier strings with no brackets
// inside the quoted values).
func parseTomlStringArray(val string) []string {
	_, rest, found := strings.Cut(val, "[")
	if !found {
		return nil
	}
	if before, _, ok := strings.Cut(rest, "]"); ok {
		rest = before
	}
	var out []string
	for p := range strings.SplitSeq(rest, ",") {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"'`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
