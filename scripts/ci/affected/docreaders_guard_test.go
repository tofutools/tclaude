//ci:reads none
//
// The doc paths in this file are fixtures for the heuristics, not reads.

package main

import (
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A changed doc selects only the packages whose tests declare reading it, so
// a test that reads a doc without readsDirective would be skipped on exactly
// the change it exists to catch. This finds tests that both climb out of their
// package and name the docs tree in a string literal, and requires the
// directive in the same file.
//
// Like the whole-tree guard it is a heuristic over the shapes tests actually
// use, two-sided so that a test merely mentioning "docs/x.md" in a message, or
// climbing to a sibling package's fixtures, is not flagged.
func TestDocReadersCarryTheDirective(t *testing.T) {
	root := moduleRoot(t)
	var undeclared []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); p != root && (name == ".git" || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		// No exemption for whole-tree packages: they run on every change, but
		// their reverse dependents are reached only through the declaration.
		var reads func([]byte) bool
		switch {
		case strings.HasSuffix(d.Name(), "_test.go"):
			reads = readsDocs
		case strings.HasSuffix(d.Name(), ".test.mjs"):
			reads = jsReadsDocs
		default:
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if _, declared := parseReads(string(body)); declared || !reads(body) {
			return nil
		}
		undeclared = append(undeclared, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("scanning test files: %v", err)
	}
	if len(undeclared) > 0 {
		t.Errorf("these tests appear to read documentation but declare no %q directive, so PR CI "+
			"may skip them on a doc change they would have caught — add e.g. `%sdocs/x.md` "+
			"(or a `docs/dir/` prefix) as a comment line in the file, or `%s%s` if it reads none:\n  %s",
			strings.TrimSpace(readsDirective), readsDirective, readsDirective, readsNothing,
			strings.Join(undeclared, "\n  "))
	}
}

// readsDocs reports whether a Go source file both leaves its package
// directory (a ".." path literal or a module-root lookup) and names the docs
// tree or a root-level doc in a string literal. Comments are ignored.
func readsDocs(src []byte) bool {
	var climbs, namesDocs bool
	if moduleRootShapes.Match(src) {
		climbs = true
	}
	fset := token.NewFileSet()
	var sc scanner.Scanner
	sc.Init(fset.AddFile("", fset.Base(), len(src)), src, nil, 0)
	for {
		_, tok, lit := sc.Scan()
		if tok == token.EOF {
			break
		}
		if tok != token.STRING {
			continue
		}
		v, err := strconv.Unquote(lit)
		if err != nil || strings.Contains(v, "#") {
			continue // a link anchor names a doc, it does not read one
		}
		if v == ".." || strings.HasPrefix(v, "../") {
			climbs = true
		}
		up := strings.HasPrefix(v, "../")
		if v == "docs" || v == "mkdocs.yml" || strings.HasPrefix(v, "docs/") ||
			(up && (strings.Contains(v, "/docs/") || strings.HasSuffix(v, "/docs") || strings.HasSuffix(v, ".md"))) {
			namesDocs = true
		}
	}
	return climbs && namesDocs
}

// jsTreeClimbToDocs matches a Node suite resolving a path that climbs out of
// its directory into the docs tree or to a root-level doc, the shape
// `new URL('../../docs/x.md', import.meta.url)` and its join() spellings use.
var jsTreeClimbToDocs = regexp.MustCompile(`['"\x60](\.\./)+(docs[/'"\x60]|[A-Za-z0-9_.-]+\.md['"\x60]|mkdocs\.yml)`)

// jsReadsDocs is readsDocs for a `.test.mjs` suite. Comment lines are ignored.
func jsReadsDocs(src []byte) bool {
	for _, line := range strings.Split(string(src), "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") {
			continue
		}
		if jsTreeClimbToDocs.MatchString(line) {
			return true
		}
	}
	return false
}

// Every directive must name a doc that exists, or it silently declares
// nothing after a rename.
func TestReadsDirectivesNameExistingDocs(t *testing.T) {
	root := moduleRoot(t)
	s, err := newSelector()
	if err != nil {
		t.Fatalf("newSelector: %v", err)
	}
	if len(s.docReaders) == 0 {
		t.Fatal("the selector recorded no doc readers; the directive scan is broken")
	}
	for target, dirs := range s.docReaders {
		if !isDoc(target) {
			t.Errorf("%v declare %s%s, which is not documentation; the selector only consults "+
				"directives for docs", dirs, readsDirective, target)
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(target))); err != nil {
			t.Errorf("%v declare %s%s, which does not exist: %v", dirs, readsDirective, target, err)
		}
	}
}

func TestReadsTargetsParsesDirectiveLines(t *testing.T) {
	got := readsTargets("package x\n\n//ci:reads docs/a.md\n\t//ci:reads docs/learn/  \n// mentions //ci:reads inline\n")
	want := []string{"docs/a.md", "docs/learn/"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("readsTargets = %v, want %v", got, want)
	}
	if targets, declared := parseReads("package x\n//ci:reads none\n"); len(targets) != 0 || !declared {
		t.Errorf("parseReads(none) = (%v, %v), want no targets but declared", targets, declared)
	}
}

func TestReadsDocsHeuristic(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{`package x; var p = filepath.Join("..", "..", "docs", "dashboard.md")`, true},
		{`package x; const d = "../../docs/pwf"`, true},
		{`package x; var r = filepath.Join(filepath.Dir(f), "..", ".."); var d = filepath.Join("docs", "index.md")`, true},
		// Naming a doc without leaving the package is a message, not a read.
		{`package x; var hint = "see docs/sandboxing.md#stacked"`, false},
		// An absolute fixture path is not the module's docs tree.
		{`package x; var p = filepath.Join(root, ".."); var m = "/x/docs/providers.md"`, false},
		// A link anchor is a reference, not a read.
		{`package x; var p = ".."; var u = "https://x/blob/main/docs/sandboxing.md#stacked"`, false},
		// Comments do not count.
		{"package x\n// reads ../../docs/x.md\nvar p = \"..\"", false},
		// Climbing to a sibling package's fixtures is not a docs read.
		{`package x; var p = filepath.Join("..", "other", "testdata")`, false},
	}
	for _, c := range cases {
		if got := readsDocs([]byte(c.src)); got != c.want {
			t.Errorf("readsDocs(%q) = %v, want %v", c.src, got, c.want)
		}
	}
}

func TestJSReadsDocsHeuristic(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"const g = readFileSync(new URL('../../../../docs/sandboxing.md', import.meta.url));", true},
		{`const p = join(here, "../../README.md");`, true},
		{"// see ../../docs/x.md", false},
		{"const hint = 'docs/sandboxing.md#stacked';", false},
		{"import { x } from '../dashboard/js/x.mjs';", false},
	}
	for _, c := range cases {
		if got := jsReadsDocs([]byte(c.src)); got != c.want {
			t.Errorf("jsReadsDocs(%q) = %v, want %v", c.src, got, c.want)
		}
	}
}

// A Node suite's directive is credited to the nearest Go package above it,
// the one whose test runs the suite. No real suite declares a doc read today,
// so the crediting is pinned on a synthetic tree.
func TestJSReadsDirectiveCreditsRunningGoPackage(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pkg/runner/runner_test.go", "package runner\n")
	write("pkg/runner/suites/a.test.mjs", "//ci:reads docs/guide.md\nimport test from 'node:test';\n")
	scan, err := scanGoDirs(root)
	if err != nil {
		t.Fatalf("scanGoDirs: %v", err)
	}
	if got, want := scan.docReaders["docs/guide.md"], []string{"pkg/runner"}; !reflect.DeepEqual(got, want) {
		t.Errorf("docReaders[docs/guide.md] = %v, want %v", got, want)
	}
}

// The directive only matters if a doc change reaches its declaring package
// and nothing else, so pin the round trip against the real tree.
func TestDocChangesSelectOnlyDeclaredReaders(t *testing.T) {
	s, err := newSelector()
	if err != nil {
		t.Fatalf("newSelector: %v", err)
	}
	const agentd = "github.com/tofutools/tclaude/pkg/claude/agentd"
	got, reason := s.mapChangedFiles([]string{"docs/dashboard.md"})
	if reason != "" || !got[agentd] {
		t.Errorf("docs/dashboard.md mapped to (%v, %q), want %s selected", got, reason, agentd)
	}
	// A doc no test reads selects nothing, rather than the full shard.
	for _, doc := range []string{"docs/federation.md", "README.md", "mkdocs.yml"} {
		if readers := s.readersOf(doc); len(readers) > 0 {
			t.Logf("%s is now declared by %v; pick another undeclared doc for this test", doc, readers)
			continue
		}
		got, reason := s.mapChangedFiles([]string{doc})
		if reason != "" || len(got) != 0 {
			t.Errorf("%s mapped to (%v, %q), want no packages and no fallback", doc, got, reason)
		}
	}
	// Markdown below the root but outside docs/ belongs to its package.
	if isDoc("pkg/claude/skills/agent-coord/SKILL.md") {
		t.Error("nested Markdown must not be treated as documentation")
	}
}
