package main

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// Selection is a property of PR CI only. The release flows must keep running
// every shard in full on the merge commit, so that a package a PR wrongly
// skipped is still measured before anything ships. That is an operator
// decision, and this pins it: the release workflows must not acquire a
// diff-base, however the action's default changes.
func TestOnlyPRCIPassesADiffBase(t *testing.T) {
	root := moduleRoot(t)
	for _, c := range []struct {
		workflow string
		wantBase bool
	}{
		{"ci.yml", true},
		{"release.yml", false},
		{"manual-release.yml", false},
	} {
		bases := testSuiteDiffBases(t, filepath.Join(root, ".github", "workflows", c.workflow))
		if len(bases) == 0 {
			t.Errorf("%s does not call ./.github/actions/test-suite at all", c.workflow)
			continue
		}
		for _, base := range bases {
			if got := base != ""; got != c.wantBase {
				t.Errorf("%s calls the test-suite action with diff-base %q; want a diff base: %v",
					c.workflow, base, c.wantBase)
			}
		}
	}
}

// testSuiteDiffBases returns the diff-base input of every test-suite call in a
// workflow, empty string for a call that passes none.
func testSuiteDiffBases(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string            `yaml:"uses"`
				With map[string]string `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(body, &wf); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	var bases []string
	for _, job := range wf.Jobs {
		for _, step := range job.Steps {
			if step.Uses == "./.github/actions/test-suite" {
				bases = append(bases, step.With["diff-base"])
			}
		}
	}
	return bases
}
