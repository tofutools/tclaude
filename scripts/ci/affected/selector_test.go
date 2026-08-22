package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// testSelector builds a selector over a synthetic module: two leaf packages, a
// package that imports one of them, a test-only edge, and a directory that is
// Go-owned but not reported by `go list` (platform-excluded).
func testSelector() *selector {
	return &selector{
		root: "/repo",
		dirToPkg: map[string]string{
			".":            "m",
			"pkg/leaf":     "m/pkg/leaf",
			"pkg/other":    "m/pkg/other",
			"pkg/consumer": "m/pkg/consumer",
			"pkg/tester":   "m/pkg/tester",
		},
		goDirs: map[string]bool{
			".": true, "pkg/leaf": true, "pkg/other": true,
			"pkg/consumer": true, "pkg/tester": true, "pkg/excluded": true,
		},
		testClosure: map[string]map[string]bool{
			"m":              {"m": true, "m/pkg/consumer": true, "m/pkg/leaf": true},
			"m/pkg/leaf":     {"m/pkg/leaf": true},
			"m/pkg/other":    {"m/pkg/other": true},
			"m/pkg/consumer": {"m/pkg/consumer": true, "m/pkg/leaf": true},
			"m/pkg/tester":   {"m/pkg/tester": true, "m/pkg/other": true},
		},
	}
}

func TestOwnerDirWalksUpToTheOwningPackage(t *testing.T) {
	s := testSelector()
	cases := []struct {
		file string
		want string
		ok   bool
	}{
		{"pkg/leaf/leaf.go", "pkg/leaf", true},
		// Asset trees have no Go files of their own; the nearest Go directory
		// above them is the package that embeds or reads them.
		{"pkg/leaf/testdata/golden.json", "pkg/leaf", true},
		{"pkg/leaf/static/app.mjs", "pkg/leaf", true},
		{"main.go", ".", true},
		// Everything that reaches the module root without passing a package is
		// unowned: go.mod, workflows, scripts, docs. Tests read those.
		{"go.mod", "", false},
		{"go.sum", "", false},
		{"README.md", "", false},
		{"docs/dashboard.md", "", false},
		{".github/workflows/ci.yml", "", false},
		{"scripts/lib/smoke/common.sh", "", false},
	}
	for _, c := range cases {
		got, ok := s.ownerDir(c.file)
		if got != c.want || ok != c.ok {
			t.Errorf("ownerDir(%q) = (%q, %v), want (%q, %v)", c.file, got, ok, c.want, c.ok)
		}
	}
}

func TestMapChangedFilesFallsBackWhenOwnershipIsUnknown(t *testing.T) {
	s := testSelector()
	for _, file := range []string{
		"docs/dashboard.md",            // read by a Go test, and unowned
		"go.sum",                       // a dependency bump reaches everything
		"pkg/excluded/only_windows.go", // Go-owned, but not built on this platform
		selfPkgDir + "/selector.go",    // the selector must not grade itself
	} {
		pkgs, reason := s.mapChangedFiles([]string{"pkg/leaf/leaf.go", file})
		if reason == "" {
			t.Errorf("mapChangedFiles(%q) = %v, want a full-run reason", file, pkgs)
		}
	}
}

func TestMapChangedFilesResolvesOwnedPaths(t *testing.T) {
	s := testSelector()
	got, reason := s.mapChangedFiles([]string{"pkg/leaf/leaf.go", "pkg/leaf/testdata/x.json", "main.go"})
	if reason != "" {
		t.Fatalf("unexpected full-run fallback: %s", reason)
	}
	want := map[string]bool{"m/pkg/leaf": true, "m": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mapChangedFiles = %v, want %v", got, want)
	}
}

func TestAffectedByFollowsCompiledAndTestOnlyEdges(t *testing.T) {
	s := testSelector()
	got := sortedKeys(s.affectedBy(map[string]bool{"m/pkg/leaf": true}))
	want := []string{"m", "m/pkg/consumer", "m/pkg/leaf"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("affectedBy(leaf) = %v, want %v", got, want)
	}
	// m/pkg/tester only reaches m/pkg/other through its test files, which is
	// still a reason to re-run it.
	got = sortedKeys(s.affectedBy(map[string]bool{"m/pkg/other": true}))
	want = []string{"m/pkg/other", "m/pkg/tester"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("affectedBy(other) = %v, want %v", got, want)
	}
}

func TestBuildTestClosureIncludesTestImportDependencies(t *testing.T) {
	pkgs := []goPkg{
		{ImportPath: "m/a", Deps: []string{"m/b", "fmt"}},
		{ImportPath: "m/b"},
		{ImportPath: "m/helper", Deps: []string{"m/b"}},
		{ImportPath: "m/c", TestImports: []string{"m/helper", "testing"}},
		{ImportPath: "m/d", XTestImports: []string{"m/a"}},
	}
	closure := buildTestClosure(pkgs)
	// m/c's test binary compiles m/helper, and through it m/b.
	for _, want := range []string{"m/c", "m/helper", "m/b"} {
		if !closure["m/c"][want] {
			t.Errorf("closure[m/c] is missing %s: %v", want, sortedKeys(closure["m/c"]))
		}
	}
	// External test files count too, and packages outside the module do not.
	if !closure["m/d"]["m/b"] {
		t.Errorf("closure[m/d] is missing m/b: %v", sortedKeys(closure["m/d"]))
	}
	if closure["m/a"]["fmt"] {
		t.Errorf("closure[m/a] should not carry out-of-module packages: %v", sortedKeys(closure["m/a"]))
	}
}

func TestSelectForDegradesToTheFullShard(t *testing.T) {
	s := testSelector()
	want := []string{"m/pkg/leaf", "m/pkg/other"}
	got, reason := s.selectFor("", "", want)
	if !reflect.DeepEqual(got, want) || reason == "" {
		t.Errorf("selectFor with no base = (%v, %q), want the full shard", got, reason)
	}
	t.Setenv("TCLAUDE_CI_FULL_TESTS", "1")
	got, reason = s.selectFor("HEAD~1", "", want)
	if !reflect.DeepEqual(got, want) || reason == "" {
		t.Errorf("selectFor with the override set = (%v, %q), want the full shard", got, reason)
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// A file moved between packages must be reported under both paths: the package
// that lost it changed just as much as the one that gained it, and git's
// rename detection would hide the old path.
func TestChangedFilesReportsBothSidesOfARename(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if _, err := runGit(repo, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	git("init", "-q", ".")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "test")
	if err := os.MkdirAll(filepath.Join(repo, "pkg", "leaf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "pkg", "other"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "pkg", "leaf", "moved.go"), []byte("package leaf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "before")
	git("mv", "pkg/leaf/moved.go", "pkg/other/moved.go")
	git("commit", "-qam", "after")

	s := testSelector()
	s.root = repo
	got, err := s.changedFiles("HEAD~1", "HEAD")
	if err != nil {
		t.Fatalf("changedFiles: %v", err)
	}
	want := []string{"pkg/leaf/moved.go", "pkg/other/moved.go"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("changedFiles = %v, want %v", got, want)
	}
}
