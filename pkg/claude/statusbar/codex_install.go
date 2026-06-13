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
	// CodexTuiConflict: the config defines `tui` as an inline table, scalar, or
	// array-of-tables (e.g. `tui = { theme = "dark" }` or `[[tui]]`), so tclaude
	// cannot add a `[tui]` table or a `tui.status_line` key without producing a
	// duplicate-key TOML error. Left untouched; the user is told to convert
	// `tui` to a `[tui]` table.
	CodexTuiConflict
	// CodexNeedsManualFix: a tclaude-managed status_line was hand-edited into an
	// unterminated array (the file is already invalid TOML). tclaude won't guess
	// where the array ends — left untouched; the user fixes or deletes it.
	CodexNeedsManualFix
)

// CodexConfigState is the read-only classification of the Codex config's
// status-line state. Centralised so `tclaude setup` and `--check` share one
// source of truth (no scattered re-classification).
type CodexConfigState int

const (
	CodexNotInstalled CodexConfigState = iota // no status_line; tclaude can add one
	CodexInstalledState                       // tclaude-managed and current
	CodexNeedsRepair                          // tclaude-managed but stale
	CodexUserManagedState                     // user owns the status_line
	CodexTuiConflictState                     // tui is an inline table/scalar/array; unsafe to edit
	CodexNeedsManualFixState                  // managed value hand-edited into an unterminated (broken) array
)

// CodexConfigPath returns ~/.codex/config.toml, or "" if the home dir is unknown.
func CodexConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "config.toml")
}

// CodexStatusLineState reads ~/.codex/config.toml and classifies the status
// line. A missing/unreadable file reads as CodexNotInstalled.
func CodexStatusLineState() CodexConfigState {
	path := CodexConfigPath()
	if path == "" {
		return CodexNotInstalled
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return CodexNotInstalled
	}
	return classifyCodex(scanCodexConfigData(data))
}

// CheckCodexInstalled reports whether the current curated default is installed
// as a tclaude-managed status_line in ~/.codex/config.toml.
func CheckCodexInstalled() bool {
	return CodexStatusLineState() == CodexInstalledState
}

// InstallCodex writes (or repairs) tclaude's curated status_line into
// ~/.codex/config.toml, mirroring install.go's idempotent + repair semantics:
//   - no status_line               → add the curated default       (CodexInstalled)
//   - tclaude-managed + current    → no-op                          (CodexAlreadyInstalled)
//   - tclaude-managed + stale      → replace with the curated default (CodexRepaired)
//   - user-owned status_line       → leave it untouched             (CodexUserManaged)
//   - tui is an inline table/scalar → leave it untouched            (CodexTuiConflict)
//
// The user's other keys and comments are preserved: edits are surgical (splice
// specific lines), never a parse→re-marshal round-trip, which is the only way
// to keep a hand-written TOML file's comments and ordering intact. The
// insertion form is chosen so the result is always valid TOML (it never
// duplicates the `tui` table).
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
	switch outcome {
	case CodexAlreadyInstalled, CodexUserManaged, CodexTuiConflict, CodexNeedsManualFix:
		return outcome, nil // no write
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return 0, fmt.Errorf("create .codex dir: %w", err)
	}
	if err := os.WriteFile(path, newData, 0o644); err != nil {
		return 0, fmt.Errorf("write Codex config: %w", err)
	}
	return outcome, nil
}

// codexScan is tclaude's structural view of a Codex config, computed by a
// single line-by-line pass.
type codexScan struct {
	present         bool // an active (non-comment) tui.status_line assignment exists
	managed         bool // that assignment is tclaude-managed (preceded by the marker)
	current         bool // the managed value matches codexStatusLineItems exactly
	arrayTerminated bool // the value array closed its ']' (false = unterminated, already-broken TOML)
	keyStart        int  // line index of the status_line key (-1 if absent)
	keyEnd          int  // line index of the array's closing ']' (inclusive)

	tuiHeader        int  // line index of the first [tui] table header (-1 if absent)
	firstTableHeader int  // line index of the first table header of any kind (-1 if absent)
	tuiBoundAsValue  bool // a top-level `tui = ...` binding (inline table/scalar) exists
	tuiNamespaced    bool // tui exists as a table via dotted `tui.x` keys or `[tui.sub]` subtables
}

func scanCodexConfigData(data []byte) codexScan {
	lines, _ := toLines(data)
	return scanCodexConfig(lines)
}

// scanCodexConfig walks the config line-by-line, tracking the current table so
// a bare `status_line` is attributed to `tui.status_line` only inside [tui]
// (and the dotted top-level `tui.status_line` form is also recognised). Only
// the first active binding is considered (TOML forbids duplicate keys). It also
// records how the `tui` namespace is used, so the installer can pick an
// insertion form that never produces invalid TOML.
func scanCodexConfig(lines []string) codexScan {
	sc := codexScan{keyStart: -1, keyEnd: -1, tuiHeader: -1, firstTableHeader: -1, arrayTerminated: true}
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
			if sc.firstTableHeader == -1 {
				sc.firstTableHeader = i
			}
			arrayTable := strings.HasPrefix(t, "[[")
			currentTable = tomlTableName(t)
			switch {
			case currentTable == "tui" && arrayTable:
				// [[tui]] is an array-of-tables: `tui` is an array, so neither a
				// [tui] table nor a tui.status_line key can be added without a
				// duplicate-key conflict. Mark it so install refuses rather than
				// inserting into an array element Codex won't read.
				sc.tuiBoundAsValue = true
			case currentTable == "tui":
				if sc.tuiHeader == -1 {
					sc.tuiHeader = i
				}
			case strings.HasPrefix(currentTable, "tui."):
				sc.tuiNamespaced = true
			}
			prevNonBlank = t
			continue
		}

		if lhs, rhs, found := strings.Cut(t, "="); found {
			left := strings.TrimSpace(lhs)

			// Record how `tui` is used at the top level, to guide insertion.
			if currentTable == "" {
				switch {
				case left == "tui":
					sc.tuiBoundAsValue = true
				case strings.HasPrefix(left, "tui."):
					sc.tuiNamespaced = true
				}
			}

			if sc.keyStart == -1 {
				isTui := (currentTable == "tui" && left == "status_line") ||
					(currentTable == "" && left == "tui.status_line")
				if isTui {
					sc.present = true
					sc.keyStart = i
					sc.managed = strings.HasPrefix(prevNonBlank, codexManagedPrefix)
					endIdx, items, ok := scanStatusLineArray(lines, i, rhs)
					sc.keyEnd = endIdx
					sc.arrayTerminated = ok
					if sc.managed && ok {
						sc.current = equalStrings(items, codexStatusLineItems)
					}
				}
			}
		}
		prevNonBlank = t
	}
	return sc
}

// classifyCodex maps a scan to the read-only state enum.
func classifyCodex(sc codexScan) CodexConfigState {
	switch {
	case sc.present && !sc.managed:
		return CodexUserManagedState
	case sc.present && sc.managed && !sc.arrayTerminated:
		return CodexNeedsManualFixState
	case sc.present && sc.managed && sc.current:
		return CodexInstalledState
	case sc.present && sc.managed:
		return CodexNeedsRepair
	case sc.tuiBoundAsValue:
		return CodexTuiConflictState
	default:
		return CodexNotInstalled
	}
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
	case sc.present && sc.managed && !sc.arrayTerminated:
		// Our managed value was hand-edited into an unterminated array, so the
		// file is already invalid TOML. Don't guess where the array ends (that
		// risks deleting the user's trailing content) — leave it untouched.
		return CodexNeedsManualFix, data
	case sc.present && sc.managed && sc.current:
		return CodexAlreadyInstalled, data
	case !sc.present && sc.tuiBoundAsValue:
		// `tui = {...}` / `tui = scalar` / `[[tui]]`: a [tui] table or
		// tui.status_line key would duplicate `tui` and break the parse. Refuse
		// rather than corrupt.
		return CodexTuiConflict, data
	}

	// Repair: replace the managed (stale) key — possibly multi-line — in place,
	// preserving its original left-hand side (`status_line` vs `tui.status_line`)
	// and indentation. The marker line above keyStart is left untouched.
	if sc.present && sc.managed {
		lhs, _, _ := strings.Cut(lines[sc.keyStart], "=")
		out := append([]string{}, lines[:sc.keyStart]...)
		out = append(out, strings.TrimRight(lhs, " \t")+" = "+formatStatusLineArray(codexStatusLineItems))
		out = append(out, lines[sc.keyEnd+1:]...)
		return CodexRepaired, fromLines(out, sep)
	}

	// Install (no status_line present). Pick an insertion form that is always
	// valid TOML for the current `tui` usage:
	switch {
	case sc.tuiHeader >= 0:
		// An existing [tui] table: add `status_line` right after its header.
		keyLine := "status_line = " + formatStatusLineArray(codexStatusLineItems)
		out := append([]string{}, lines[:sc.tuiHeader+1]...)
		out = append(out, codexManagedMarker, keyLine)
		out = append(out, lines[sc.tuiHeader+1:]...)
		return CodexInstalled, fromLines(out, sep)

	case sc.tuiNamespaced:
		// tui is already a table (via dotted keys or [tui.sub] subtables) but
		// has no [tui] header. A dotted `tui.status_line` in top-level scope is
		// compatible; a new [tui] header would conflict / be out-of-order.
		keyLine := "tui.status_line = " + formatStatusLineArray(codexStatusLineItems)
		insertAt := sc.firstTableHeader // top-level scope = before the first table
		if insertAt < 0 {
			insertAt = len(lines)
		}
		out := append([]string{}, lines[:insertAt]...)
		out = append(out, codexManagedMarker, keyLine)
		out = append(out, lines[insertAt:]...)
		return CodexInstalled, fromLines(out, sep)

	default:
		// No `tui` usage anywhere: a fresh [tui] block at EOF is clean and
		// conventional (matches what Codex's own /statusline writes).
		keyLine := "status_line = " + formatStatusLineArray(codexStatusLineItems)
		out := append([]string{}, lines...)
		if len(out) > 0 {
			out = append(out, "") // blank separator before our block
		}
		out = append(out, "[tui]", codexManagedMarker, keyLine)
		return CodexInstalled, fromLines(out, sep)
	}
}

// formatStatusLineArray renders the `[...]` array of the status_line value
// (without the key) on a single line, so repair is a clean one-line splice.
func formatStatusLineArray(items []string) string {
	quoted := make([]string, len(items))
	for i, it := range items {
		quoted[i] = fmt.Sprintf("%q", it)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// formatCodexStatusLine renders the full `status_line = [...]` line (the form
// written inside a [tui] table). Used by tests and the table-insert path.
func formatCodexStatusLine(items []string) string {
	return "status_line = " + formatStatusLineArray(items)
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

// scanStatusLineArray reads a (possibly multi-line) TOML array value starting
// at line startIdx, whose text after '=' is rhs. It returns the line index of
// the closing ']' (inclusive) and the array's string elements. Quote- and
// comment-aware: a ']', ',' or '#' inside a quoted string or an inline comment
// does not end the array — so a hand-edited multi-line managed value is bounded
// correctly (avoids orphaning lines on repair). ok is false if the array never
// closes within the file.
func scanStatusLineArray(lines []string, startIdx int, rhs string) (endIdx int, items []string, ok bool) {
	depth := 0
	inString := false
	var quote byte
	escaped := false
	var elem strings.Builder
	hasElem := false

	flush := func() {
		if hasElem {
			items = append(items, elem.String())
			elem.Reset()
			hasElem = false
		}
	}

	// process one physical line's worth of text; returns true once the array
	// closes (depth back to 0).
	process := func(s string) bool {
		for i := 0; i < len(s); i++ {
			c := s[i]
			if inString {
				if escaped {
					escaped = false
					elem.WriteByte(c)
					continue
				}
				if c == '\\' && quote == '"' {
					escaped = true
					continue
				}
				if c == quote {
					inString = false
					continue
				}
				elem.WriteByte(c)
				continue
			}
			switch c {
			case '#':
				return false // comment to end of this physical line
			case '"', '\'':
				inString = true
				quote = c
				hasElem = true
			case '[':
				depth++
			case ']':
				depth--
				if depth == 0 {
					flush()
					return true
				}
			case ',':
				flush()
			}
		}
		return false
	}

	if process(rhs) {
		return startIdx, items, true
	}
	for j := startIdx + 1; j < len(lines); j++ {
		if process(lines[j]) {
			return j, items, true
		}
	}
	return startIdx, items, false
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
