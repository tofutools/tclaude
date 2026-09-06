package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	fullIntegrationCondition = "github.event_name != 'pull_request' || github.base_ref != 'refactor/platform-v2' || contains(github.event.pull_request.labels.*.name, 'full-integration-ci')"
	platformV2Condition      = "github.event_name == 'pull_request' && github.base_ref == 'refactor/platform-v2'"
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

// The platform rewrite is allowed to defer expensive integration evidence,
// but only for PRs into that long-lived branch. Main, other PR bases, and
// manual runs retain the existing full behavior. Label transitions are named
// explicitly because GitHub's default pull_request activity types do not
// create a run when a label is added or removed.
func TestPlatformV2FocusedPolicyWiring(t *testing.T) {
	root := moduleRoot(t)
	wantTypes := []string{"opened", "synchronize", "reopened", "labeled", "unlabeled"}

	ci := readWorkflowPolicy(t, filepath.Join(root, ".github", "workflows", "ci.yml"))
	if ci.On.WorkflowDispatch == nil {
		t.Fatal("ci.yml must retain a manual full-suite entrypoint")
	}
	if !reflect.DeepEqual(ci.On.PullRequest.Types, wantTypes) {
		t.Fatalf("ci.yml pull_request types = %v, want %v", ci.On.PullRequest.Types, wantTypes)
	}
	for _, job := range []string{
		"sandbox-v2-smoke",
		"sandbox-v2-seatbelt-smoke",
		"copilot-smoke",
		"filtered-proxy-smoke",
		"proxy-posture-e2e",
	} {
		if got := ci.Jobs[job].If; got != fullIntegrationCondition {
			t.Errorf("ci.yml job %s if = %q, want the branch-scoped integration condition", job, got)
		}
	}
	if got := ci.Jobs["platform-v2-core"].If; got != platformV2Condition {
		t.Errorf("ci.yml platform-v2-core if = %q, want %q", got, platformV2Condition)
	}
	focusedRun := ""
	for _, step := range ci.Jobs["platform-v2-core"].Steps {
		focusedRun += step.Run
	}
	for _, family := range []string{
		"TestRequireSpawnPermission",
		"TestCronSpawn",
		"TestTriggerSpawn",
		"TestObserveBackgroundWork",
		"TestResolveBackgroundObservation",
		"TestDashboardAndTerminalStatusShareReadOnlyBackgroundObservation",
		"TestSessionReaper_(ProjectsFinishedShellWithoutDashboard|RefreshesLiveBackgroundLedgerBeforeStopWithoutDashboard|ExpiredBackgroundShellWithUnknownScanDoesNotEstablishIdle)",
		"TestReconcileBackground",
		"TestProjectSessionBackgroundLedgers",
		"TestSetSessionStatusFromBackgroundProjection",
	} {
		if !strings.Contains(focusedRun, family) {
			t.Errorf("ci.yml platform-v2-core does not retain the %s family", family)
		}
	}
	for _, packagePath := range []string{
		"./pkg/claude/agentd",
		"./pkg/claude/session",
		"./pkg/claude/common/db",
	} {
		if !strings.Contains(focusedRun, packagePath) {
			t.Errorf("ci.yml platform-v2-core does not run focused package %s", packagePath)
		}
	}
	shards, ok := ci.Jobs["test"].Strategy.Matrix.Shard.(string)
	if !ok {
		t.Fatalf("ci.yml test shard is %T, want an expression string", ci.Jobs["test"].Strategy.Matrix.Shard)
	}
	for _, fragment := range []string{
		"github.base_ref == 'refactor/platform-v2'",
		"full-integration-ci",
		`["jstest","process","sandbox","rest"]`,
		`["agentd","jstest","process","sandbox","rest"]`,
	} {
		if !strings.Contains(shards, fragment) {
			t.Errorf("ci.yml test shard expression %q is missing %q", shards, fragment)
		}
	}

	for _, workflow := range []string{"copilot-lab.yml", "group-route-feasibility.yml"} {
		policy := readWorkflowPolicy(t, filepath.Join(root, ".github", "workflows", workflow))
		if !reflect.DeepEqual(policy.On.PullRequest.Types, wantTypes) {
			t.Errorf("%s pull_request types = %v, want %v", workflow, policy.On.PullRequest.Types, wantTypes)
		}
		for job, body := range policy.Jobs {
			if body.If != fullIntegrationCondition {
				t.Errorf("%s job %s if = %q, want the branch-scoped integration condition", workflow, job, body.If)
			}
		}
	}
}

type workflowPolicy struct {
	On struct {
		WorkflowDispatch *struct{} `yaml:"workflow_dispatch"`
		PullRequest      struct {
			Types []string `yaml:"types"`
		} `yaml:"pull_request"`
	} `yaml:"on"`
	Jobs map[string]struct {
		If    string `yaml:"if"`
		Steps []struct {
			Run string `yaml:"run"`
		} `yaml:"steps"`
		Strategy struct {
			Matrix struct {
				Shard any `yaml:"shard"`
			} `yaml:"matrix"`
		} `yaml:"strategy"`
	} `yaml:"jobs"`
}

func readWorkflowPolicy(t *testing.T, path string) workflowPolicy {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var policy workflowPolicy
	if err := yaml.Unmarshal(body, &policy); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return policy
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
