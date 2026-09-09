package codex

import (
	"fmt"
	"github.com/tofutools/tclaude/internal/backend/host"
	"path/filepath"
	"strings"
)

// These pure native-config edits preserve the v1 directory-trust contract.
// File access and opt-in admission are deliberately separate from parsing.
func planCodexDirTrust(data []byte, projectDir string) (bool, []byte, error) {
	wantTable := "projects." + tomlQuote(projectDir) // projects."/abs/path"
	header := "[" + wantTable + "]"
	const trustLine = `trust_level = "trusted"`

	lines, sep := splitConfigLines(data)

	hdrIdx := -1
	for i, raw := range lines {
		if name, ok := tomlTableHeader(raw); ok && name == wantTable {
			hdrIdx = i
			break
		}
	}

	if hdrIdx == -1 {
		// No [projects."dir"] table header for this dir. Before appending one,
		// refuse (rather than corrupt) two cases where the append would
		// produce invalid TOML — a duplicate key:
		//   1. `projects` bound in a conflicting form (inline table / scalar /
		//      [[projects]] array-of-tables): a [projects."dir"] subtable then
		//      redefines `projects`.
		//   2. THIS dir already keyed under `projects` in a NON-header form —
		//      a top-level dotted `projects."dir"…`, or a `"dir"…` key inside a
		//      plain [projects] table. Codex always writes the header form we
		//      match above, so this only arises in a hand-edited config; the
		//      dir may well already be trusted there, so appending a second
		//      definition would both corrupt the file AND be redundant.
		// The caller treats this error as non-fatal (pre-trust is best-effort —
		// the operator can still clear the modal via the dashboard focus
		// button), so a refusal degrades gracefully rather than failing a spawn.
		if conflictsWithProjectsTable(lines) {
			return false, nil, fmt.Errorf("codex dir-trust: refusing to edit config — `projects` is defined in a conflicting form (inline table / scalar / array-of-tables); trust %q via the Codex modal instead", projectDir)
		}
		if dirKeyedUnderProjectsNonHeader(lines, projectDir) {
			return false, nil, fmt.Errorf("codex dir-trust: %q is already keyed under [projects] in a form tclaude won't edit (a dotted or inline-table entry); leave it as-is (it may already be trusted) or fix it via the Codex modal", projectDir)
		}
		out := append([]string{}, lines...)
		if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
			out = append(out, "") // blank separator before our block
		}
		out = append(out, header, trustLine)
		return true, joinConfigLines(out, sep), nil
	}

	// The table exists. Find its body (up to the next table header) and look
	// for an existing trust_level key.
	bodyEnd := len(lines)
	for j := hdrIdx + 1; j < len(lines); j++ {
		if _, ok := tomlTableHeader(lines[j]); ok {
			bodyEnd = j
			break
		}
	}
	for j := hdrIdx + 1; j < bodyEnd; j++ {
		key, _, ok := tomlKeyValue(lines[j])
		if !ok || key != "trust_level" {
			continue
		}
		if tomlStringValueIs(lines[j], "trusted") {
			return false, data, nil // already trusted — idempotent no-op
		}
		// Replace the stale value in place, preserving indentation.
		indent := lines[j][:len(lines[j])-len(strings.TrimLeft(lines[j], " \t"))]
		out := append([]string{}, lines[:j]...)
		out = append(out, indent+trustLine)
		out = append(out, lines[j+1:]...)
		return true, joinConfigLines(out, sep), nil
	}

	// Table present but no trust_level key — insert one right after the header.
	out := append([]string{}, lines[:hdrIdx+1]...)
	out = append(out, trustLine)
	out = append(out, lines[hdrIdx+1:]...)
	return true, joinConfigLines(out, sep), nil
}

// conflictsWithProjectsTable reports whether `projects` is used in a form
// that makes adding a [projects."dir"] subtable invalid TOML. Only two
// top-level shapes actually collide:
//
//   - a bare top-level `projects = …` binding (inline table or scalar):
//     `projects` is then a value, so a [projects."dir"] subtable header
//     redefines the same key;
//   - a top-level `[[projects]]` array-of-tables: `projects` is then an
//     array, which a [projects."dir"] table likewise can't coexist with.
//
// Everything else is fine and must NOT be refused: an existing
// `[projects."other"]` subtable (the exact shape we add), a top-level dotted
// `projects.foo = …` (which also makes `projects` a table — compatible), and
// a `projects` key nested in some OTHER table (`[foo]` → foo.projects, an
// unrelated key). Tracking the current table is what distinguishes these.
func conflictsWithProjectsTable(lines []string) bool {
	currentTable := ""
	for _, raw := range lines {
		t := strings.TrimSpace(raw)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if name, ok := tomlArrayTableHeader(t); ok {
			if name == "projects" {
				return true // [[projects]] — projects is an array
			}
			currentTable = name
			continue
		}
		if name, ok := tomlTableHeader(t); ok {
			currentTable = name
			continue
		}
		// A bare top-level `projects = …` binds projects to a value; a dotted
		// `projects.x = …` (key != "projects") keeps it a table and is fine.
		if currentTable == "" {
			if key, _, ok := tomlKeyValue(t); ok && key == "projects" {
				return true
			}
		}
	}
	return false
}

// dirKeyedUnderProjectsNonHeader reports whether projectDir is already keyed
// under `projects` in a form OTHER than the `[projects."dir"]` table header
// (which the caller has already ruled out). Two such forms exist:
//
//   - a top-level dotted key `projects."dir"…` (e.g. `projects."dir".trust_level
//     = "trusted"` or `projects."dir" = { … }`);
//   - a key `"dir"…` inside a plain `[projects]` table.
//
// In either form, appending a `[projects."dir"]` table would define the same
// key twice → invalid TOML. We can't safely splice these arbitrary shapes, so
// the caller refuses. Path comparison uses the same TOML-quoted spelling we
// would write, and the trailing quote in the quoted key prevents a prefix
// match of `/a/b` against `/a/bc`.
func dirKeyedUnderProjectsNonHeader(lines []string, projectDir string) bool {
	qDir := tomlQuote(projectDir)   // "/a/b"
	topPrefix := "projects." + qDir // projects."/a/b"
	currentTable := ""
	for _, raw := range lines {
		t := strings.TrimSpace(raw)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if name, ok := tomlTableHeader(t); ok {
			currentTable = name
			continue
		}
		if name, ok := tomlArrayTableHeader(t); ok {
			currentTable = name
			continue
		}
		key, _, ok := tomlKeyValue(t)
		if !ok {
			continue
		}
		switch currentTable {
		case "":
			// Top-level: `projects."dir"` exactly, or `projects."dir".<sub>`.
			if key == topPrefix || strings.HasPrefix(key, topPrefix+".") {
				return true
			}
		case "projects":
			// Inside a plain [projects] table: `"dir"` exactly, or `"dir".<sub>`.
			if key == qDir || strings.HasPrefix(key, qDir+".") {
				return true
			}
		}
	}
	return false
}

// --- small TOML line helpers (local to keep the harness package free of a
// UI-package import; mirror statusbar/codex_install.go's toLines/fromLines/
// tomlTableName) ---

// splitConfigLines splits config bytes into lines (\r stripped), returning
// the dominant separator so it round-trips. A single trailing newline is
// dropped and re-added by joinConfigLines, normalising to exactly one.
func splitConfigLines(data []byte) (lines []string, sep string) {
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

func joinConfigLines(lines []string, sep string) []byte {
	return []byte(strings.Join(lines, sep) + sep)
}

// tomlTableHeader returns the table name of a `[name]` header line (e.g.
// `[projects."/x"]` → `projects."/x"`), ok=false for non-headers and for
// `[[array.tables]]`. Trailing inline comments are stripped.
func tomlTableHeader(raw string) (string, bool) {
	t := strings.TrimSpace(raw)
	if !strings.HasPrefix(t, "[") || strings.HasPrefix(t, "[[") {
		return "", false
	}
	t = stripTomlComment(t)
	if !strings.HasPrefix(t, "[") || !strings.HasSuffix(t, "]") {
		return "", false
	}
	return strings.TrimSpace(t[1 : len(t)-1]), true
}

// tomlArrayTableHeader is tomlTableHeader for a `[[name]]` array-of-tables
// header.
func tomlArrayTableHeader(raw string) (string, bool) {
	t := strings.TrimSpace(raw)
	if !strings.HasPrefix(t, "[[") {
		return "", false
	}
	t = stripTomlComment(t)
	if !strings.HasPrefix(t, "[[") || !strings.HasSuffix(t, "]]") {
		return "", false
	}
	return strings.TrimSpace(t[2 : len(t)-2]), true
}

// tomlKeyValue splits a `key = value` line into the trimmed key and the
// raw value text. ok=false for blanks, comments and non-assignments. The
// key is returned verbatim (dotted keys like `tui.status_line` stay
// whole), which is all the callers here need.
func tomlKeyValue(raw string) (key, value string, ok bool) {
	t := strings.TrimSpace(raw)
	if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "[") {
		return "", "", false
	}
	lhs, rhs, found := strings.Cut(t, "=")
	if !found {
		return "", "", false
	}
	return strings.TrimSpace(lhs), strings.TrimSpace(rhs), true
}

// tomlStringValueIs reports whether a `key = "<want>"` line's value is the
// quoted string want — handling both basic ("…") and literal ('…') strings
// and a trailing inline comment.
func tomlStringValueIs(raw, want string) bool {
	_, value, ok := tomlKeyValue(raw)
	if !ok {
		return false
	}
	got, ok := firstTomlString(value)
	return ok && got == want
}

// firstTomlString extracts the first quoted string from a value's text,
// honouring basic ("…", backslash-escaped) and literal ('…', verbatim)
// strings. Returns ok=false when the value doesn't start with a quote.
func firstTomlString(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	q := s[0]
	if q != '"' && q != '\'' {
		return "", false
	}
	var b strings.Builder
	escaped := false
	for i := 1; i < len(s); i++ {
		c := s[i]
		if q == '"' && !escaped && c == '\\' {
			escaped = true
			continue
		}
		if escaped {
			// Minimal unescape — enough for the comparisons here; the value
			// we test against ("trusted") contains no escapes.
			switch c {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			default:
				b.WriteByte(c)
			}
			escaped = false
			continue
		}
		if c == q {
			return b.String(), true
		}
		b.WriteByte(c)
	}
	return "", false // unterminated
}

// stripTomlComment removes a trailing `# …` comment that is outside any
// quoted string, returning the trimmed remainder.
func stripTomlComment(s string) string {
	inString := false
	var q byte
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' && q == '"' {
				escaped = true
				continue
			}
			if c == q {
				inString = false
			}
			continue
		}
		switch c {
		case '"', '\'':
			inString = true
			q = c
		case '#':
			return strings.TrimSpace(s[:i])
		}
	}
	return strings.TrimSpace(s)
}

// tomlQuote renders s as a TOML basic string (the form Codex writes for a
// projects path key). For ordinary paths this is just "…"; a backslash or
// quote is escaped, matching Codex's own output so an existing entry still
// matches.
func tomlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func ensureDirectoryTrusted(root, cwd string) error {
	if !filepath.IsAbs(root) || !filepath.IsAbs(cwd) {
		return fmt.Errorf("directory trust requires absolute native root and working directory")
	}
	return host.EditNativeConfigFile("codex config", filepath.Join(root, "config.toml"), 0600, func(data []byte) (bool, []byte, error) { return planCodexDirTrust(data, cwd) })
}
