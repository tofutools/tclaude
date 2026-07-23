package harness

import (
	"reflect"
	"slices"
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
	if !slices.Contains(SpawnBinaries(), "opencode") {
		t.Fatalf("SpawnBinaries() = %v, want opencode", SpawnBinaries())
	}
	if h.Sandbox == nil || h.Approval == nil || h.ToolGovernance == nil || h.Ask == nil || h.Convs == nil ||
		h.ApprovalsReviewer {
		t.Fatalf("unexpected OpenCode capability contracts: %+v", h)
	}
	if h.SupportsRename() {
		t.Fatal("OpenCode rename must use the out-of-band ConvStore API path")
	}
	if !h.CanRename() {
		t.Fatal("OpenCode ConvStore must expose the rename affordance")
	}
}

// TestOpenCodeLifecycleContract pins the four lifecycle tokens and the
// capability flags they fold into (TCL-670). OpenCode's conversation lives in
// a daemon-owned server, so compact/exit are dispatched through the managed
// TUI command API, not tmux send-keys — but the descriptor still names the
// tokens, and agentd keys its managed-command translation off these exact
// strings. Renames go out-of-band through ConvStore.SetTitle (the Codex
// pattern) and OpenCode has no built-in remote access, so both of those tokens
// are empty. All four are compile-time constants because their string values
// flow toward an injection sink.
func TestOpenCodeLifecycleContract(t *testing.T) {
	h, ok := Get(OpenCodeName)
	if !ok {
		t.Fatal("opencode harness is not registered")
	}

	if got := h.Life.RenameCommand(); got != "" {
		t.Fatalf("RenameCommand() = %q, want \"\" (rename is out-of-band via ConvStore.SetTitle)", got)
	}
	if got := h.Life.CompactCommand(); got != "/compact" {
		t.Fatalf("CompactCommand() = %q, want %q", got, "/compact")
	}
	if got := h.Life.SoftExitCommand(); got != "/exit" {
		t.Fatalf("SoftExitCommand() = %q, want %q", got, "/exit")
	}
	if got := h.Life.RemoteControlCommand(); got != "" {
		t.Fatalf("RemoteControlCommand() = %q, want \"\" (OpenCode has no built-in remote access)", got)
	}

	if h.SupportsRename() {
		t.Fatal("SupportsRename() must be false: OpenCode has no in-pane rename command")
	}
	if !h.SupportsCompact() || !h.CanCompact() {
		t.Fatal("SupportsCompact()/CanCompact() must be true for OpenCode")
	}
	if !h.SupportsSoftExit() {
		t.Fatal("SupportsSoftExit() must be true for OpenCode")
	}
	if h.SupportsRemoteControl() || h.CanRemoteControl() {
		t.Fatal("SupportsRemoteControl()/CanRemoteControl() must be false for OpenCode")
	}
	if _, err := ResolveRemoteControl(h, true); err == nil {
		t.Fatal("ResolveRemoteControl(opencode, true) must error: no built-in remote access")
	}
}

// TestOpenCodeConvIDMaterialisation pins the conv-id materialisation posture
// (TCL-670): OpenCode's id is knowable before the pane starts because agentd
// pre-mints it through the managed server API, so it enrolls at launch rather
// than seeding a first turn (Codex's mechanism). The two are mutually exclusive.
func TestOpenCodeConvIDMaterialisation(t *testing.T) {
	h, ok := Get(OpenCodeName)
	if !ok {
		t.Fatal("opencode harness is not registered")
	}
	if !h.SupportsLaunchEnrollment() {
		t.Fatal("OpenCode must enroll at launch: agentd pre-mints the conv-id via POST /session")
	}
	if h.NeedsSpawnSeed() {
		t.Fatal("OpenCode must not seed a first turn: its conv-id is pre-minted, not discovered post-turn")
	}
	if !h.UsesAuthoritativeServer() {
		t.Fatal("OpenCode is server-authoritative: the conversation lives in the managed serve process")
	}
}

func TestOpenCodeSandboxCatalog(t *testing.T) {
	h, ok := Get(OpenCodeName)
	if !ok {
		t.Fatal("opencode harness is not registered")
	}
	if got := h.Sandbox.DefaultMode(); got != OpenCodeSandboxAccessControl {
		t.Fatalf("DefaultMode() = %q, want %q", got, OpenCodeSandboxAccessControl)
	}
	if got := h.Sandbox.Modes(); !slices.Equal(got, []string{OpenCodeSandboxAccessControl, OpenCodeSandboxOff}) {
		t.Fatalf("Modes() = %v, want [%s %s]", got, OpenCodeSandboxAccessControl, OpenCodeSandboxOff)
	}
	if help := h.Sandbox.ModeHelp(OpenCodeSandboxOff); !strings.Contains(help, "No directory scoping or OS containment") {
		t.Fatalf("ModeHelp(%q) = %q, want explicit no-containment warning", OpenCodeSandboxOff, help)
	}
	if got, err := ResolveSandboxMode(h, ""); err != nil || got != OpenCodeSandboxAccessControl {
		t.Fatalf("ResolveSandboxMode(opencode, blank) = %q, %v; want %q, nil", got, err, OpenCodeSandboxAccessControl)
	}
	if got, err := ValidateSandboxMode(h, OpenCodeSandboxOff); err != nil || got != OpenCodeSandboxOff {
		t.Fatalf("ValidateSandboxMode(opencode, off) = %q, %v; want %q, nil", got, err, OpenCodeSandboxOff)
	}
	if _, err := ValidateSandboxMode(h, SandboxWorkspaceWrite); err == nil {
		t.Fatal("OpenCode must reject sandbox modes it cannot enforce")
	}
	if got, err := ResolveApprovalPolicy(h, ""); err != nil || got != OpenCodeApprovalDeny {
		t.Fatalf("ResolveApprovalPolicy(opencode, blank) = %q, %v; want %q, nil", got, err, OpenCodeApprovalDeny)
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
