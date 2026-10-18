package main

import (
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
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
	s, err := newSelector()
	if err != nil {
		t.Fatalf("newSelector: %v", err)
	}
	var undeclared []string
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); p != root && (name == ".git" || name == "node_modules" || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if s.alwaysDirs[filepath.ToSlash(filepath.Dir(rel))] {
			return nil // selected by every change anyway
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !readsDocs(body) || len(readsTargets(string(body))) > 0 {
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
			"(or a `docs/dir/` prefix) as a comment line in the file:\n  %s",
			strings.TrimSpace(readsDirective), readsDirective, strings.Join(undeclared, "\n  "))
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
		if err != nil {
			continue
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
