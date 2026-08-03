package harness

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCopilotDescriptor pins the first Copilot wave's capability surface. The
// NEGATIVE half is the load-bearing half: every contract left nil here is one
// tclaude has no fixture-backed evidence for, and a descriptor that quietly
// grew one would make callers act on a capability that was never verified.
func TestCopilotDescriptor(t *testing.T) {
	h, ok := Get(CopilotName)
	if !ok {
		t.Fatal("copilot harness is not registered")
	}
	if h.DisplayName != "GitHub Copilot CLI" {
		t.Fatalf("DisplayName = %q, want %q", h.DisplayName, "GitHub Copilot CLI")
	}
	if h.Spawn == nil || h.Models == nil || h.Life == nil {
		t.Fatalf("copilot must provide Spawn, Models and Life: %+v", h)
	}
	if _, err := ResolveSpawnable(CopilotName); err != nil {
		t.Fatalf("ResolveSpawnable(copilot) = %v, want spawnable", err)
	}
	if got := h.Spawn.Binary(); got != "copilot" {
		t.Fatalf("Binary() = %q, want %q", got, "copilot")
	}
	if !slices.Contains(SpawnBinaries(), "copilot") {
		t.Fatalf("SpawnBinaries() = %v, want copilot", SpawnBinaries())
	}

	// Hooks graduated in TCL-972: the fixture lab ran the real 1.0.77 binary
	// and recorded both halves of the contract — where a tclaude-owned hook
	// file has to live to fire, and which event names make Copilot emit
	// Claude Code's payload. That is exactly the kind of evidence the rest of
	// this test insists on before a contract may be advertised.
	if h.Hooks == nil {
		t.Fatalf("copilot must advertise the fixture-backed hook installer: %+v", h)
	}

	// The ConvStore graduated in TCL-976 on the same terms: the fixture lab
	// ran the real binary and recorded the per-session workspace.yaml and
	// events.jsonl the store reads, so the session-state layout is now
	// observed rather than assumed.
	if h.Convs == nil {
		t.Fatalf("copilot must advertise the fixture-backed conversation store: %+v", h)
	}

	// The sandbox catalog and the model transport graduated in TCL-978 on the
	// same evidence terms. Neither is read off the documentation, which
	// describes neither: the sandbox catalog encodes what the pinned binary's
	// own help topics establish (the command sandbox is experimental, off by
	// default, and reachable ONLY through settings.json and an in-pane slash
	// command — so tclaude can assert its state but never set it), and the
	// transport's destinations are read out of the CLI's shipped runtime
	// module. Both are ASSERT/REFUSE contracts rather than control contracts,
	// which is precisely why they can be advertised without a lever.
	if h.Sandbox == nil {
		t.Fatalf("copilot must advertise the assert-off sandbox catalog: %+v", h)
	}
	if h.ModelTransport == nil {
		t.Fatalf("copilot must advertise the first-party model transport: %+v", h)
	}

	// Still-deferred contracts (TCL-965 phases 2-5): documented CLI flags are
	// evidence, runtime formats and enforcement semantics are not.
	if h.Ask != nil ||
		h.Approval != nil || h.ToolGovernance != nil ||
		h.NestedSandbox != nil || h.HostControlSandbox != nil || h.AskTimeout != nil {
		t.Fatalf("copilot must not advertise unverified contracts: %+v", h)
	}

	// BuiltinOSSandbox stays false even though Copilot really does ship an OS
	// sandbox: the flag means the harness owns an OS-enforced sandbox BEHIND
	// its catalog, and this catalog only asserts that sandbox is off.
	if h.BuiltinOSSandbox {
		t.Errorf("copilot must not claim a built-in OS sandbox its catalog cannot select: %+v", h)
	}
	if h.TclaudeLayerMode != CopilotSandboxOff {
		t.Errorf("TclaudeLayerMode = %q, want %q", h.TclaudeLayerMode, CopilotSandboxOff)
	}
	if !h.SupportsHooks() {
		t.Errorf("SupportsHooks() = false, want true now that hooks are fixture-backed")
	}
	if !h.SupportsConvs() {
		t.Errorf("SupportsConvs() = false, want true now that the store is fixture-backed")
	}
	for name, got := range map[string]bool{
		"SupportsAsk":              h.SupportsAsk(),
		"SupportsBuiltinOSSandbox": h.SupportsBuiltinOSSandbox(),
		"SupportsApproval":         h.SupportsApproval(),
		"SupportsToolGovernance":   h.SupportsToolGovernance(),
		"SupportsAutoReview":       h.SupportsAutoReview(),
		"SupportsAskTimeout":       h.SupportsAskTimeout(),
		"SupportsDirTrust":         h.SupportsDirTrust(),
		"SupportsBackgroundShells": h.SupportsBackgroundShells(),
		"SupportsMonitors":         h.SupportsMonitors(),
		"UsesAuthoritativeServer":  h.UsesAuthoritativeServer(),
		"NeedsSpawnSeed":           h.NeedsSpawnSeed(),
		"WantsTmuxScrollback":      h.WantsTmuxScrollback(),
	} {
		if got {
			t.Errorf("%s() = true, want false for the minimal Copilot wave", name)
		}
	}
	// SupportsSandbox is checked separately from the deferred set above: the
	// catalog exists, but what it selects is an ASSERTION about Copilot's own
	// wall, never a lever that moves it.
	if !h.SupportsSandbox() {
		t.Error("SupportsSandbox() = false, want true now that the assert-off catalog exists")
	}
	// The conv-id is knowable before launch (`--session-id`), so the daemon may
	// enroll the agent up front — and correspondingly Copilot needs no seeded
	// first turn to materialise the id the way Codex does.
	if !h.SupportsLaunchEnrollment() {
		t.Fatal("SupportsLaunchEnrollment() must be true: copilot accepts a preset --session-id")
	}
}

// TestCopilotLifecycleContract pins the four lifecycle tokens. They are typed
// into a tmux pane, so they must stay compile-time constants and must match
// Copilot's documented slash commands exactly.
func TestCopilotLifecycleContract(t *testing.T) {
	h := MustGet(CopilotName)

	if got := h.Life.RenameCommand(); got != "/rename" {
		t.Fatalf("RenameCommand() = %q, want %q", got, "/rename")
	}
	if got := h.Life.CompactCommand(); got != "/compact" {
		t.Fatalf("CompactCommand() = %q, want %q", got, "/compact")
	}
	if got := h.Life.SoftExitCommand(); got != "/exit" {
		t.Fatalf("SoftExitCommand() = %q, want %q", got, "/exit")
	}
	// `/remote [on|off]` is directional; the toggle contract cannot express it,
	// so remote control stays unsupported rather than half-wired.
	if got := h.Life.RemoteControlCommand(); got != "" {
		t.Fatalf("RemoteControlCommand() = %q, want \"\" (copilot's /remote is directional)", got)
	}

	if !h.SupportsRename() || !h.CanRename() {
		t.Fatal("SupportsRename()/CanRename() must be true for copilot")
	}
	if !h.SupportsCompact() || !h.CanCompact() {
		t.Fatal("SupportsCompact()/CanCompact() must be true for copilot")
	}
	if !h.SupportsSoftExit() {
		t.Fatal("SupportsSoftExit() must be true for copilot")
	}
	if h.SupportsRemoteControl() || h.CanRemoteControl() {
		t.Fatal("copilot must not advertise remote control")
	}
	if _, err := ResolveRemoteControl(h, true); err == nil {
		t.Fatal("ResolveRemoteControl(copilot, true) must be refused")
	}
}

func TestCopilotBuildCommand(t *testing.T) {
	s := copilotSpawner{}
	const uuid = "11111111-2222-3333-4444-555555555555"

	tests := []struct {
		name string
		spec SpawnSpec
		want string
	}{
		{
			name: "bare launch omits every unset flag",
			spec: SpawnSpec{},
			want: "copilot",
		},
		{
			name: "env exports are prepended verbatim",
			spec: SpawnSpec{EnvExports: "export A=1; "},
			want: "export A=1; copilot",
		},
		{
			name: "a pinned executable path replaces the PATH lookup",
			spec: SpawnSpec{ExecutablePath: "/opt/gh copilot/copilot"},
			want: "'/opt/gh copilot/copilot'",
		},
		{
			name: "fresh launch carries the preset id, name and first turn",
			spec: SpawnSpec{SessionID: uuid, Name: "review bot", InitialPrompt: "hello there"},
			want: "copilot --session-id " + uuid + " --name='review bot' -i 'hello there'",
		},
		{
			name: "resume binds the exact id with the documented equals form",
			spec: SpawnSpec{ResumeID: uuid},
			want: "copilot --resume=" + uuid,
		},
		{
			name: "resume never emits --session-id or --name alongside it",
			spec: SpawnSpec{ResumeID: uuid, SessionID: "other", Name: "renamed"},
			want: "copilot --resume=" + uuid,
		},
		{
			name: "resume still submits an initial prompt",
			spec: SpawnSpec{ResumeID: uuid, InitialPrompt: "carry on"},
			want: "copilot --resume=" + uuid + " -i 'carry on'",
		},
		{
			name: "model and effort use the documented equals form",
			spec: SpawnSpec{Model: "claude-sonnet-4.6", Effort: "high"},
			want: "copilot --model=claude-sonnet-4.6 --effort=high",
		},
		{
			name: "pass-through args are quoted individually with no -- separator",
			spec: SpawnSpec{ExtraArgs: []string{"--log-level=debug", "a b"}},
			want: "copilot --log-level=debug 'a b'",
		},
		{
			name: "the prompt is one quoted arg, emitted after pass-through args",
			spec: SpawnSpec{ExtraArgs: []string{"--no-color"}, InitialPrompt: "fix $HOME; rm -rf /"},
			want: "copilot --no-color -i 'fix $HOME; rm -rf /'",
		},
		{
			name: "contracts copilot does not implement are ignored, not approximated",
			spec: SpawnSpec{
				SandboxMode: "read-only", ApprovalPolicy: "never", AutoReview: true,
				RemoteControl: true, BypassHookTrust: true, PermissionProfile: "tclaude-agent",
				AskUserQuestionTimeout: "5m", StrongNestedSandbox: true,
			},
			want: "copilot",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.BuildCommand(tc.spec); got != tc.want {
				t.Fatalf("BuildCommand()\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestCopilotBuildCommandQuotesHostileValues pins the injection boundary: the
// built string is handed to `sh -c`, so a hostile value must land as ONE
// argument and never as extra words or a second command.
//
// Rather than pattern-matching the quoted text (which proves little), this runs
// the command through a real shell with a stand-in "copilot" that prints its
// argv, and compares the argv the shell actually produced.
func TestCopilotBuildCommandQuotesHostileValues(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "copilot")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o700); err != nil {
		t.Fatalf("write fake copilot: %v", err)
	}
	// A marker file the injected payloads try to create. It must not exist
	// afterwards: if any value escaped its quoting, the shell would run it.
	pwned := filepath.Join(dir, "pwned")

	spec := SpawnSpec{
		ExecutablePath: fake,
		SessionID:      "id'; touch " + pwned + "; echo '",
		Name:           "name`touch " + pwned + "`",
		Model:          "model$(touch " + pwned + ")",
		Effort:         "high;reboot",
		ExtraArgs:      []string{"arg && touch " + pwned},
		InitialPrompt:  "prompt' && touch " + pwned + " #",
	}
	out, err := exec.Command("sh", "-c", copilotSpawner{}.BuildCommand(spec)).Output()
	if err != nil {
		t.Fatalf("running the built command failed: %v", err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	want := []string{
		"--session-id", spec.SessionID,
		"--name=" + spec.Name,
		"--model=" + spec.Model,
		"--effort=" + spec.Effort,
		spec.ExtraArgs[0],
		"-i", spec.InitialPrompt,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("argv after the shell parsed the command:\n got: %q\nwant: %q", got, want)
	}
	if _, err := os.Stat(pwned); !os.IsNotExist(err) {
		t.Fatalf("an injected payload executed: %s exists (stat err %v)", pwned, err)
	}

	// The resume branch is mutually exclusive with the fresh one, so its id
	// needs its own pass. `--resume=` is the one flag whose value is glued to
	// the flag name, which is exactly where a quoting mistake would hide.
	resume := SpawnSpec{ExecutablePath: fake, ResumeID: "id' && touch " + pwned + " #"}
	out, err = exec.Command("sh", "-c", copilotSpawner{}.BuildCommand(resume)).Output()
	if err != nil {
		t.Fatalf("running the built resume command failed: %v", err)
	}
	got = strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if want := []string{"--resume=" + resume.ResumeID}; !slices.Equal(got, want) {
		t.Fatalf("resume argv:\n got: %q\nwant: %q", got, want)
	}
	if _, err := os.Stat(pwned); !os.IsNotExist(err) {
		t.Fatalf("an injected resume payload executed: %s exists (stat err %v)", pwned, err)
	}
}

func TestCopilotValidateModel(t *testing.T) {
	m := copilotModels{}

	// Empty means "omit the flag" so Copilot keeps its own configured default.
	if got, err := m.ValidateModel("  "); got != "" || err != nil {
		t.Fatalf("ValidateModel(blank) = (%q, %v), want (\"\", nil)", got, err)
	}
	// Every suggestion must survive its own validator.
	for _, model := range m.Models() {
		if got, err := m.ValidateModel(model); got != model || err != nil {
			t.Fatalf("ValidateModel(%q) = (%q, %v), want (%q, nil)", model, got, err, model)
		}
	}
	// Forward compatibility: unknown/future ids pass through, INCLUDING the
	// claude-* ids Codex rejects — Copilot genuinely brokers those — and
	// custom/BYOK deployment ids whose shape tclaude cannot predict.
	for _, model := range []string{
		"gpt-6.1", "claude-opus-9", "vendor/model:tag", "some_future+model",
		"my-org.MyDeployment@v2", "azure:GPT-5.4-Preview",
	} {
		if got, err := m.ValidateModel(model); got != model || err != nil {
			t.Fatalf("ValidateModel(%q) = (%q, %v), want it passed through", model, got, err)
		}
	}
	// Surrounding whitespace is trimmed, but case is PRESERVED: nothing
	// documents Copilot model matching as case-insensitive, and a mixed-case
	// BYOK id must survive intact.
	if got, err := m.ValidateModel(" GPT-5.4 "); got != "GPT-5.4" || err != nil {
		t.Fatalf("ValidateModel(\" GPT-5.4 \") = (%q, %v), want (\"GPT-5.4\", nil)", got, err)
	}
	// The safety gate: not a single bounded token → rejected. Shell quoting is
	// the injection boundary, so metacharacters inside one token are fine; what
	// is refused is a value that is plainly not a model id.
	for _, bad := range []string{
		"gpt-5.4 --allow-all",
		"model\nother",
		"model\tother",
		strings.Repeat("m", copilotMaxModelLen+1),
	} {
		if got, err := m.ValidateModel(bad); err == nil {
			t.Fatalf("ValidateModel(%q) = (%q, nil), want an error", bad, got)
		}
	}
}

func TestCopilotValidateEffort(t *testing.T) {
	m := copilotModels{}

	if got, err := m.ValidateEffort(""); got != "" || err != nil {
		t.Fatalf("ValidateEffort(\"\") = (%q, %v), want (\"\", nil)", got, err)
	}
	// Copilot's documented levels are exactly tclaude's, forwarded verbatim.
	for _, level := range []string{"low", "medium", "high", "xhigh", "max"} {
		if got, err := m.ValidateEffort(strings.ToUpper(level)); got != level || err != nil {
			t.Fatalf("ValidateEffort(%q) = (%q, %v), want (%q, nil)", level, got, err, level)
		}
	}
	if !slices.Equal(m.EffortLevels(), []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("EffortLevels() = %v", m.EffortLevels())
	}
	if _, err := m.ValidateEffort("ultra"); err == nil {
		t.Fatal("ValidateEffort(\"ultra\") must be refused")
	}
}

// TestCopilotHarnessRefusesBuiltinOSSandbox pins the TCL-977 capability answer:
// Copilot's own command sandboxing does not satisfy SupportsBuiltinOSSandbox, so
// an explicit harness-builtin implementation is refused — and the refusal names
// the property Copilot is missing rather than denying it has anything.
//
// The measurements this rests on live in
// copilotfixture/sandbox_native_smoke_test.go, against the real pinned binary;
// the reasoning lives beside the descriptor in copilot_sandbox_native.go. This
// test needs neither, which is why it sits here with the code it covers.
func TestCopilotHarnessRefusesBuiltinOSSandbox(t *testing.T) {
	copilot, ok := Get(CopilotName)
	require.True(t, ok)
	require.False(t, copilot.SupportsBuiltinOSSandbox())

	err := ValidateHarnessBuiltinOSSandbox(copilot)
	require.Error(t, err)
	require.True(t, IsBuiltinOSSandboxInvalid(err))
	assert.Contains(t, err.Error(), "built-in file edits are checked by an in-process policy",
		"the refusal must name the property Copilot is missing; a flat "+
			"\"no built-in OS sandbox\" reads as a gap in tclaude to an operator "+
			"who can see the feature in their own CLI")
}
