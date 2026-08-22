//ci:whole-tree
//
// This guard reads every test file in the module, so no import graph can
// predict when it needs to run. The marker above makes the selector run it on
// every change — which is exactly the property it is here to enforce for
// others.

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A test that walks the module root sees code its package never imports, so
// the selector cannot tell whether a change reached it. Such a test must carry
// wholeTreeMarker, which opts its package out of selection.
//
// Detection is two-sided on purpose: resolving the module root is not enough
// (a test may only read one fixed file, and a change to that file falls back
// to a full shard anyway), and walking a directory tree is not enough (most
// walks are over a temp workspace the test built itself).
var (
	// The shapes tests actually use to find the module root.
	moduleRootShapes = regexp.MustCompile(
		`os\.Stat\(filepath\.Join\([A-Za-z_][A-Za-z0-9_]*, "go\.mod"\)\)|` +
			`--show-toplevel|` +
			`runtime\.Caller\(`)
	treeWalkShapes = regexp.MustCompile(`filepath\.Walk(Dir)?\(`)
)

func TestWholeTreeScannersCarryTheMarker(t *testing.T) {
	root := moduleRoot(t)
	var unmarked []string
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
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		text := string(body)
		if !moduleRootShapes.MatchString(text) || !treeWalkShapes.MatchString(text) {
			return nil
		}
		if strings.Contains(text, wholeTreeMarker) {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		unmarked = append(unmarked, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("scanning test files: %v", err)
	}
	if len(unmarked) > 0 {
		t.Errorf("these tests scan the module root but do not carry %s, so PR CI may skip them "+
			"on a change they would have caught — add the marker as a comment in the file, or "+
			"narrow the test to what its package imports:\n  %s",
			wholeTreeMarker, strings.Join(unmarked, "\n  "))
	}
}

// The marker only has an effect if the selector reads it, so pin the round
// trip rather than trusting the two halves separately.
func TestMarkedPackagesAreAlwaysSelected(t *testing.T) {
	s, err := newSelector()
	if err != nil {
		t.Fatalf("newSelector: %v", err)
	}
	if !s.alwaysDirs[selfPkgDir] {
		t.Fatalf("%s carries %s but the selector did not record it: %v",
			selfPkgDir, wholeTreeMarker, sortedKeys(s.alwaysDirs))
	}
	// A change to one unrelated package must still select every marked one.
	affected := s.affectedBy(map[string]bool{"github.com/tofutools/tclaude/pkg/common/executil": true})
	for dir := range s.alwaysDirs {
		ip, ok := s.dirToPkg[dir]
		if !ok {
			continue
		}
		if !affected[ip] {
			t.Errorf("%s carries %s but was not selected by an unrelated change", ip, wholeTreeMarker)
		}
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package directory")
		}
		dir = parent
	}
}
