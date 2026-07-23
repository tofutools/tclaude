package harness

import (
	"reflect"
	"strings"
	"testing"
)

func TestOpenCodeDescriptor(t *testing.T) {
	h, ok := Get(OpenCodeName)
	if !ok {
		t.Fatal("opencode harness is not registered")
	}
	if h.DisplayName != "OpenCode" || !h.UsesAuthoritativeServer() ||
		!h.SupportsLaunchEnrollment() {
		t.Fatalf("unexpected OpenCode descriptor: %+v", h)
	}
	if !slicesContains(SpawnBinaries(), "opencode") {
		t.Fatalf("SpawnBinaries() = %v, want opencode", SpawnBinaries())
	}
	if h.Sandbox != nil || h.Approval != nil || h.Ask != nil || h.Convs == nil {
		t.Fatalf("out-of-scope OpenCode contracts must degrade as nil: %+v", h)
	}
	if h.SupportsRename() {
		t.Fatal("OpenCode rename must use the out-of-band ConvStore API path")
	}
	if !h.CanRename() {
		t.Fatal("OpenCode ConvStore must expose the rename affordance")
	}
}

func TestOpenCodeSpawnerAttachAndResume(t *testing.T) {
	spawner := openCodeSpawner{}
	got := spawner.BuildCommand(SpawnSpec{
		ExecutablePath: "/tmp/open code",
		EnvExports:     "export SAFE='yes'; ",
		Cwd:            "/tmp/project with space",
		ServerURL:      "http://127.0.0.1:43210",
		SessionID:      "ses_fresh",
		ExtraArgs:      []string{"--mini", "literal;touch nope"},
	})
	for _, want := range []string{
		"export SAFE='yes'; '/tmp/open code' attach http://127.0.0.1:43210",
		"--dir '/tmp/project with space'",
		"--session ses_fresh",
		"'literal;touch nope'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("BuildCommand() = %q, want substring %q", got, want)
		}
	}
	if strings.Contains(got, " serve") {
		t.Fatalf("pane command must never start a competing server: %q", got)
	}

	resumed := spawner.BuildCommand(SpawnSpec{
		ServerURL: "http://127.0.0.1:43210",
		Cwd:       "/tmp/project",
		SessionID: "ses_stale",
		ResumeID:  "ses_resume",
	})
	if !strings.Contains(resumed, "--session ses_resume") ||
		strings.Contains(resumed, "ses_stale") {
		t.Fatalf("resume command = %q", resumed)
	}
}

func TestParseOpenCodeModelsVerbose(t *testing.T) {
	input := `openai/gpt-a
{
  "variants": {
    "none": {
      "reasoningEffort": "none"
    },
    "high": {
      "reasoningEffort": "high"
    }
  }
}
openai/gpt-b
{
  "variants": {
    "low": {
      "reasoningEffort": "low"
    },
    "high": {
      "reasoningEffort": "high"
    },
    "max": {
      "reasoningEffort": "max"
    }
  }
}`
	models, efforts := parseOpenCodeModelsVerbose(input)
	if !reflect.DeepEqual(models, []string{"openai/gpt-a", "openai/gpt-b"}) {
		t.Fatalf("models = %v", models)
	}
	if !reflect.DeepEqual(efforts, []string{"none", "high", "low", "max"}) {
		t.Fatalf("efforts = %v", efforts)
	}
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
