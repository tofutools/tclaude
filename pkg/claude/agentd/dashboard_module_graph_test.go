package agentd

import (
	"fmt"
	"io/fs"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The dashboard's browser UI is a graph of native ES modules
// (dashboard/js/*.js) embedded into the agentd binary and served as-is —
// there is no bundler. The browser links the graph at load time, and ONE
// bad named import aborts the WHOLE graph: `import { nope } from './x.js'`
// where x.js doesn't export `nope` blanks the entire dashboard, not just
// the importing feature. Neither `node --input-type=module --check` (which
// only validates a single file's syntax) nor the Go string-pin tests
// (which match literal needles) catch a cross-module mismatch. JOH-374's
// cold review caught exactly this by hand — dock.js imported profileSummary
// / roleSummary from the modal modules instead of profiles.js / roles.js.
// This test automates that check so `go test ./...` fails on any unresolved
// cross-module import.
//
// The scanner (parseESModule below) is deliberately dumb: line/regex
// scanning tuned to the house style — static imports with single-quoted
// './x.js' specifiers, inline `export function|const|let|class NAME`, and
// trailing `export { ... };` blocks. It is NOT a general JS parser. When it
// meets an import/export construct it can't parse (a namespace import, a
// dynamic import(), a re-export, a destructuring export, …) it FAILS LOUD
// ("teach the scanner") rather than skipping silently — a silent skip would
// rot into false confidence. If the house style ever grows a new form,
// teach parseESModule and add a fixture to TestParseESModule.

// moduleImport is one imported binding the graph must resolve: the external
// name the target module has to export, and the raw relative specifier it
// came from. name is "default" for a default import and "" for a
// side-effect-only import (`import './x.js'`), where only the path is
// checked. line is the 1-based line of the import statement, for messages.
type moduleImport struct {
	name string
	path string
	line int
}

// moduleParse is the extracted import/export surface of one module.
// exports is the set of external names the module exposes ("default" for a
// default export).
type moduleParse struct {
	imports []moduleImport
	exports map[string]bool
}

var (
	reIdent           = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
	reImportFrom      = regexp.MustCompile(`\bfrom\s*['"]([^'"]+)['"]\s*;?`)
	reSideEffectImp   = regexp.MustCompile(`^import\s*['"]([^'"]+)['"]\s*;?\s*$`)
	reExportBlockHead = regexp.MustCompile(`^export\s*\{`)
	reExportDefault   = regexp.MustCompile(`^export\s+default\b`)
	reExportFuncClass = regexp.MustCompile(`^export\s+(?:async\s+)?(?:function|class)\s+([A-Za-z_$][A-Za-z0-9_$]*)`)
	reExportVarHead   = regexp.MustCompile(`^export\s+(?:const|let|var)\s+`)
)

func isIdentChar(c byte) bool {
	return c == '_' || c == '$' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// startsWithKeyword reports whether s begins with the bare keyword kw (not a
// longer identifier like "important" for "import").
func startsWithKeyword(s, kw string) bool {
	if !strings.HasPrefix(s, kw) {
		return false
	}
	return len(s) == len(kw) || !isIdentChar(s[len(kw)])
}

// cutLineComment drops a trailing `//...` line comment. Only used on import
// clauses and export-block bodies, whose contents are names + specifiers
// (single-quoted, no `//`) plus real interspersed comments — so cutting at
// the first `//` is safe there. It is NOT used on arbitrary code lines.
func cutLineComment(s string) string {
	if before, _, found := strings.Cut(s, "//"); found {
		return before
	}
	return s
}

// parseESModule extracts the import/export surface of one embedded module.
// It returns an error (teach-the-scanner) on any import/export construct it
// cannot confidently parse, so an unhandled house-style change fails loudly
// instead of silently under-reporting the graph.
func parseESModule(name, src string) (moduleParse, error) {
	out := moduleParse{exports: map[string]bool{}}
	lines := strings.Split(src, "\n")
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		// Skip blanks, `//` line comments, and JSDoc-style block-comment
		// body lines (which start with `*` or `/*`). Import/export
		// statements are anchored to the line start, so string or comment
		// text that mentions "import"/"export" mid-line is never mistaken
		// for a statement. A commented-out import/export at column 0 *inside*
		// a multi-line `/* */` block is NOT treated as a comment — the house
		// style uses `//` for dead code, and full block-comment tracking
		// would need string/regex-aware lexing because `/*` and `*/` also
		// occur inside JS string literals here. If such a line is ever added
		// and points at a since-removed module/name, it fails loud at that
		// exact line rather than passing silently.
		if trimmed == "" ||
			strings.HasPrefix(trimmed, "//") ||
			strings.HasPrefix(trimmed, "*") ||
			strings.HasPrefix(trimmed, "/*") {
			continue
		}

		switch {
		case startsWithKeyword(trimmed, "import"):
			stmt, end, err := accumulateStatement(lines, i, importComplete)
			if err != nil {
				return out, fmt.Errorf("%s:%d: %w", name, i+1, err)
			}
			imps, err := parseImportStmt(stmt)
			if err != nil {
				return out, fmt.Errorf("%s:%d: %w", name, i+1, err)
			}
			for k := range imps {
				imps[k].line = i + 1
			}
			out.imports = append(out.imports, imps...)
			i = end

		case startsWithKeyword(trimmed, "export"):
			if reExportBlockHead.MatchString(trimmed) {
				stmt, end, err := accumulateStatement(lines, i, blockComplete)
				if err != nil {
					return out, fmt.Errorf("%s:%d: %w", name, i+1, err)
				}
				names, err := parseExportBlock(stmt)
				if err != nil {
					return out, fmt.Errorf("%s:%d: %w", name, i+1, err)
				}
				for _, n := range names {
					out.exports[n] = true
				}
				i = end
			} else {
				names, err := parseExportInline(trimmed)
				if err != nil {
					return out, fmt.Errorf("%s:%d: %w", name, i+1, err)
				}
				for _, n := range names {
					out.exports[n] = true
				}
			}
		}
	}
	return out, nil
}

// importComplete reports whether the accumulated buffer is a whole import
// statement: it ends at the `;` (multi-line imports put `} from './x.js';`
// on the final line) or already carries a `from '...'` specifier.
func importComplete(buf string) bool {
	return strings.HasSuffix(strings.TrimSpace(buf), ";") || reImportFrom.MatchString(buf)
}

// blockComplete reports whether an `export { ... }` block has reached its
// closing brace.
func blockComplete(buf string) bool { return strings.Contains(buf, "}") }

// accumulateStatement joins raw lines from start (dropping line comments and
// blank segments) until complete() is satisfied, so multi-line imports and
// export blocks parse as one statement. It bounds the join so a malformed
// (never-terminating) statement fails loud instead of eating the file.
func accumulateStatement(lines []string, start int, complete func(string) bool) (string, int, error) {
	var b strings.Builder
	for i := start; i < len(lines) && i < start+80; i++ {
		seg := strings.TrimSpace(cutLineComment(lines[i]))
		if seg != "" {
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(seg)
		}
		if complete(b.String()) {
			return b.String(), i, nil
		}
	}
	return b.String(), start, fmt.Errorf("unterminated import/export statement starting %q — teach the scanner", strings.TrimSpace(lines[start]))
}

// parseImportStmt turns one whole import statement into the bindings whose
// existence the graph must verify. It handles the house style (named
// imports with single-quoted './x.js' specifiers) plus the aliased
// (`a as b`), default (`Foo`), mixed (`Foo, { a }`), and side-effect
// (`import './x.js'`) forms the brief calls for, and fails loud on
// namespace / dynamic imports.
func parseImportStmt(stmt string) ([]moduleImport, error) {
	if m := reSideEffectImp.FindStringSubmatch(stmt); m != nil {
		return []moduleImport{{name: "", path: m[1]}}, nil
	}
	m := reImportFrom.FindStringSubmatch(stmt)
	if m == nil {
		return nil, fmt.Errorf("cannot parse import (no `from '...'` specifier) — teach the scanner: %q", stmt)
	}
	impPath := m[1]
	clause := strings.TrimSpace(stmt[len("import"):strings.Index(stmt, m[0])])
	if strings.Contains(clause, "*") {
		return nil, fmt.Errorf("namespace import (`import * as ...`) unsupported — teach the scanner: %q", stmt)
	}

	namedPart := ""
	defaultPart := clause
	if open := strings.Index(clause, "{"); open >= 0 {
		closeAt := strings.Index(clause, "}")
		if closeAt < open {
			return nil, fmt.Errorf("malformed import braces — teach the scanner: %q", stmt)
		}
		namedPart = clause[open+1 : closeAt]
		defaultPart = clause[:open]
	}

	var imps []moduleImport
	defaultPart = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(defaultPart), ","))
	if defaultPart != "" {
		if !reIdent.MatchString(defaultPart) {
			return nil, fmt.Errorf("cannot parse default import binding %q — teach the scanner: %q", defaultPart, stmt)
		}
		imps = append(imps, moduleImport{name: "default", path: impPath})
	}
	for spec := range strings.SplitSeq(namedPart, ",") {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		// `a as b` imports the external name `a` under the local alias `b`;
		// the external name is what the target module must export.
		ext := spec
		if before, _, found := strings.Cut(spec, " as "); found {
			ext = strings.TrimSpace(before)
		}
		if !reIdent.MatchString(ext) {
			return nil, fmt.Errorf("cannot parse imported name %q — teach the scanner: %q", spec, stmt)
		}
		imps = append(imps, moduleImport{name: ext, path: impPath})
	}
	if len(imps) == 0 {
		// `import {} from './x.js'` — no bindings, only the path is checked.
		imps = append(imps, moduleImport{name: "", path: impPath})
	}
	return imps, nil
}

// parseExportBlock extracts the external names from an `export { ... };`
// block. Interspersed `//` comments were already dropped during
// accumulation. Fails loud on a re-export (`export { a } from '...'`), which
// the house style doesn't use.
func parseExportBlock(stmt string) ([]string, error) {
	open := strings.Index(stmt, "{")
	closeAt := strings.LastIndex(stmt, "}")
	if open < 0 || closeAt < open {
		return nil, fmt.Errorf("malformed export block — teach the scanner: %q", stmt)
	}
	if strings.Contains(stmt[closeAt+1:], "from") {
		return nil, fmt.Errorf("re-export (`export { ... } from '...'`) unsupported — teach the scanner: %q", stmt)
	}
	var names []string
	for spec := range strings.SplitSeq(stmt[open+1:closeAt], ",") {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		// `local as external` exports the local binding under the external
		// name — the external name is what importers can reference.
		ext := spec
		if _, after, found := strings.Cut(spec, " as "); found {
			ext = strings.TrimSpace(after)
		}
		if !reIdent.MatchString(ext) {
			return nil, fmt.Errorf("cannot parse exported name %q — teach the scanner: %q", spec, stmt)
		}
		names = append(names, ext)
	}
	return names, nil
}

// parseExportInline extracts the external name(s) of an inline export
// declaration (`export [async] function|class NAME`, `export const|let|var
// NAME[, NAME2...]`, or `export default ...`). Fails loud on any other
// `export`-led form (e.g. a destructuring export, whose bound locals the
// scanner doesn't track).
func parseExportInline(line string) ([]string, error) {
	if reExportDefault.MatchString(line) {
		return []string{"default"}, nil
	}
	if m := reExportFuncClass.FindStringSubmatch(line); m != nil {
		return []string{m[1]}, nil
	}
	if m := reExportVarHead.FindStringSubmatchIndex(line); m != nil {
		// const/let/var can declare several comma-separated bindings
		// (`export const A = 1, B = 2`). Split on TOP-LEVEL commas only, so a
		// comma inside an initializer — an array (`= ['a', 'b']`), object,
		// call, or string — doesn't split, and a multi-line initializer that
		// opens `{`/`[` on this line just leaves one segment. Extracting the
		// first name only (as a single regex would) is the "silent skip" this
		// scanner is meant to avoid.
		var names []string
		for _, decl := range splitTopLevel(line[m[1]:], ',') {
			id := leadingIdent(decl)
			if id == "" {
				return nil, fmt.Errorf("cannot parse export declarator %q — teach the scanner: %q", strings.TrimSpace(decl), line)
			}
			names = append(names, id)
		}
		if len(names) == 0 {
			return nil, fmt.Errorf("unrecognized export form — teach the scanner: %q", line)
		}
		return names, nil
	}
	return nil, fmt.Errorf("unrecognized export form — teach the scanner: %q", line)
}

// splitTopLevel splits s on the byte sep, but only at bracket depth 0 and
// outside string/template literals — so a separator inside `f(a, b)`,
// `[a, b]`, `{a, b}`, or a quoted string is not a split point. Used to pull
// every declarator out of `export const A = 1, B = 2` without tripping on
// commas inside an initializer.
func splitTopLevel(s string, sep byte) []string {
	var parts []string
	depth := 0
	var quote byte // 0, or the open quote char: ' " `
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' {
				i++ // skip the escaped char
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\'', '"', '`':
			quote = c
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth > 0 {
				depth--
			}
		case sep:
			if depth == 0 {
				parts = append(parts, s[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, s[start:])
}

// leadingIdent returns the identifier a declarator begins with, or "" if it
// doesn't start with one (e.g. a `{...}`/`[...]` destructuring pattern).
func leadingIdent(s string) string {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) && isIdentChar(s[i]) {
		i++
	}
	if id := s[:i]; reIdent.MatchString(id) {
		return id
	}
	return ""
}

// TestDashboardModuleGraph walks the embedded dashboard/js/*.js modules and
// verifies the ES-module import graph the browser links at load time: every
// relative import must resolve to a real embedded module, and every imported
// name must be exported by that module. A mismatch here blanks the whole
// dashboard in the browser (see the file header); this fails it at
// `go test ./...` instead, naming the importing file, the imported symbol,
// and the target module.
func TestDashboardModuleGraph(t *testing.T) {
	mods, err := fs.Glob(dashboardAssetsFS, "js/*.js")
	if err != nil {
		t.Fatalf("globbing embedded dashboard js/: %v", err)
	}
	if len(mods) == 0 {
		t.Fatal("no js/*.js modules embedded")
	}

	parsed := make(map[string]moduleParse, len(mods))
	for _, name := range mods {
		data, err := fs.ReadFile(dashboardAssetsFS, name)
		if err != nil {
			t.Errorf("reading embedded module %q: %v", name, err)
			continue
		}
		p, err := parseESModule(name, string(data))
		if err != nil {
			t.Errorf("scanning module graph: %v", err)
			continue
		}
		parsed[name] = p
	}

	for name, p := range parsed {
		for _, imp := range p.imports {
			// Only the local relative graph is our concern; a bare
			// specifier would be an external dependency (there are none).
			if !strings.HasPrefix(imp.path, ".") {
				continue
			}
			target := path.Join(path.Dir(name), imp.path)
			tp, ok := parsed[target]
			if !ok {
				t.Errorf("%s:%d imports from %q → %q, which is not an embedded module",
					name, imp.line, imp.path, target)
				continue
			}
			if imp.name == "" {
				continue // side-effect import: path validated, no name to check
			}
			if !tp.exports[imp.name] {
				t.Errorf("%s:%d imports %q from %q, but %q does not export it",
					name, imp.line, imp.name, imp.path, target)
			}
		}
	}
}

// TestParseESModule pins the scanner's behavior over literal snippet
// fixtures so future house-style drift is a fixture edit, not archaeology:
// the import/export forms it must parse, the aliased/default forms, and the
// constructs it must reject loudly (teach-the-scanner). If you add a form to
// parseESModule, add a case here.
func TestParseESModule(t *testing.T) {
	cases := []struct {
		name        string
		src         string
		wantImports []moduleImport
		wantExports []string
		wantErr     bool
	}{
		{
			name:        "single-line named import",
			src:         `import { a, b } from './x.js';`,
			wantImports: []moduleImport{{name: "a", path: "./x.js", line: 1}, {name: "b", path: "./x.js", line: 1}},
		},
		{
			name:        "multi-line import with trailing comma",
			src:         "import {\n  a, b,\n  c,\n} from './y.js';",
			wantImports: []moduleImport{{name: "a", path: "./y.js", line: 1}, {name: "b", path: "./y.js", line: 1}, {name: "c", path: "./y.js", line: 1}},
		},
		{
			name:        "aliased import checks the external name",
			src:         `import { a as local } from './x.js';`,
			wantImports: []moduleImport{{name: "a", path: "./x.js", line: 1}},
		},
		{
			name:        "default import",
			src:         `import Foo from './x.js';`,
			wantImports: []moduleImport{{name: "default", path: "./x.js", line: 1}},
		},
		{
			name:        "mixed default and named import",
			src:         `import Foo, { a } from './x.js';`,
			wantImports: []moduleImport{{name: "default", path: "./x.js", line: 1}, {name: "a", path: "./x.js", line: 1}},
		},
		{
			name:        "side-effect import (path only)",
			src:         `import './x.js';`,
			wantImports: []moduleImport{{name: "", path: "./x.js", line: 1}},
		},
		{
			name:        "export function",
			src:         `export function foo() {}`,
			wantExports: []string{"foo"},
		},
		{
			name:        "export async function",
			src:         `export async function bar() {}`,
			wantExports: []string{"bar"},
		},
		{
			name:        "export const / let / class",
			src:         "export const A = 1;\nexport let b = 2;\nexport class C {}",
			wantExports: []string{"A", "b", "C"},
		},
		{
			name:        "multi-declarator export const yields every name",
			src:         `export const A = 1, B = 2;`,
			wantExports: []string{"A", "B"},
		},
		{
			name:        "array/object/string commas in an initializer don't split declarators",
			src:         "export const KINDS = ['a', 'b', 'c'];\nexport const MAP = { a: 1, b: 2 };\nexport const S = 'x, y';\nexport const F = fn(1, 2);",
			wantExports: []string{"KINDS", "MAP", "S", "F"},
		},
		{
			name:        "inline export opening a multi-line object initializer",
			src:         "export const OBJ = {\n  a: 1,\n  b: 2,\n};",
			wantExports: []string{"OBJ"},
		},
		{
			name:        "single-line export block",
			src:         `export { a, b };`,
			wantExports: []string{"a", "b"},
		},
		{
			name:        "multi-line export block with comment and trailing comma",
			src:         "export {\n  a,\n  // a note about b\n  b,\n};",
			wantExports: []string{"a", "b"},
		},
		{
			name:        "aliased export block checks the external name",
			src:         `export { local as a };`,
			wantExports: []string{"a"},
		},
		{
			name:        "export default",
			src:         `export default function foo() {}`,
			wantExports: []string{"default"},
		},
		{
			name:        "line comments and mid-line 'import' text are ignored",
			src:         "// import { nope } from './nope.js';\nconst s = \"import x\";\nexport function ok() {}",
			wantExports: []string{"ok"},
		},
		{name: "namespace import fails loud", src: `import * as ns from './x.js';`, wantErr: true},
		{name: "dynamic import fails loud", src: `import('./x.js');`, wantErr: true},
		{name: "re-export fails loud", src: `export { a } from './x.js';`, wantErr: true},
		{name: "unknown export form fails loud", src: `export enum E { A }`, wantErr: true},
		{name: "destructuring export fails loud", src: `export const { a, b } = obj;`, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseESModule("fixture.js", tc.src)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected a teach-the-scanner error, got none (imports=%v exports=%v)", got.imports, keys(got.exports))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantImports == nil {
				tc.wantImports = []moduleImport{}
			}
			gotImports := got.imports
			if gotImports == nil {
				gotImports = []moduleImport{}
			}
			if !reflect.DeepEqual(gotImports, tc.wantImports) {
				t.Errorf("imports:\n  got  %v\n  want %v", gotImports, tc.wantImports)
			}
			gotExports := keys(got.exports)
			wantExports := append([]string{}, tc.wantExports...)
			sort.Strings(wantExports)
			if !reflect.DeepEqual(gotExports, wantExports) {
				t.Errorf("exports:\n  got  %v\n  want %v", gotExports, wantExports)
			}
		})
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
