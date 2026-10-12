package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withPiSessionLookup(t *testing.T, fn func(cwd, id string) string) {
	t.Helper()
	prev := piSessionFileLookup
	piSessionFileLookup = fn
	t.Cleanup(func() { piSessionFileLookup = prev })
}

func TestPiSpawnerFreshLaunch(t *testing.T) {
	got := piSpawner{}.BuildCommand(SpawnSpec{
		EnvExports:     "export A=1; ",
		SessionID:      "0b9d9a6e-3c8c-4f43-9d0c-2b8b6f1e7a10",
		Name:           "my agent",
		Model:          "openai/gpt-5:high",
		Effort:         "medium",
		ApprovalPolicy: PiApprovalReadOnly,
		ExtraArgs:      []string{"--verbose"},
		InitialPrompt:  "[system: welcome] -- don't",
	})
	want := `export A=1; pi --session-id 0b9d9a6e-3c8c-4f43-9d0c-2b8b6f1e7a10 --name 'my agent' ` +
		`--model openai/gpt-5:high --thinking medium --tools read,grep,find,ls --verbose ` +
		`-- '[system: welcome] -- don'\''t'`
	if got != want {
		t.Fatalf("command:\n got %s\nwant %s", got, want)
	}
}

func TestPiSpawnerOmitsUnset(t *testing.T) {
	if got := (piSpawner{}).BuildCommand(SpawnSpec{ApprovalPolicy: PiApprovalAuto}); got != "pi" {
		t.Fatalf("bare launch = %q, want pi", got)
	}
}

func TestPiSpawnerResumeUsesSessionFile(t *testing.T) {
	withPiSessionLookup(t, func(cwd, id string) string {
		if cwd == "/work" && id == "abc" {
			return "/store/--work--/2026_abc.jsonl"
		}
		return ""
	})
	got := piSpawner{}.BuildCommand(SpawnSpec{Cwd: "/work", ResumeID: "abc", SessionID: "ignored"})
	if got != `pi --session /store/--work--/2026_abc.jsonl` {
		t.Fatalf("resume = %q", got)
	}
	// No file found: fall back to the id (pi resolves it in the current project).
	got = piSpawner{}.BuildCommand(SpawnSpec{Cwd: "/elsewhere", ResumeID: "abc"})
	if got != `pi --session abc` {
		t.Fatalf("resume fallback = %q", got)
	}
}

func TestPiFindSessionFile(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "--tmp-proj--")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(nested, "2026-10-12T19-49-51-659Z_tcl-1.jsonl")
	flat := filepath.Join(dir, "2026-10-12T20-00-00-000Z_tcl-2.jsonl")
	for _, p := range []string{want, flat} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := piFindSessionFile(dir, "tcl-1"); got != want {
		t.Fatalf("nested lookup = %q, want %q", got, want)
	}
	if got := piFindSessionFile(dir, "tcl-2"); got != flat {
		t.Fatalf("flat lookup = %q, want %q", got, flat)
	}
	for _, id := range []string{"", "tcl-*", "../x", "missing"} {
		if got := piFindSessionFile(dir, id); got != "" {
			t.Fatalf("lookup %q = %q, want none", id, got)
		}
	}
}

func TestPiSessionsDirResolution(t *testing.T) {
	agent := t.TempDir()
	env := map[string]string{PiAgentDirEnvVar: agent}
	getenv := func(k string) string { return env[k] }
	if got, _ := piSessionsDir(getenv, "/cwd"); got != filepath.Join(agent, "sessions") {
		t.Fatalf("default sessions dir = %q", got)
	}
	if err := os.WriteFile(filepath.Join(agent, "settings.json"), []byte(`{"sessionDir":"rel/sessions"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := piSessionsDir(getenv, "/cwd"); got != "/cwd/rel/sessions" {
		t.Fatalf("settings sessionDir = %q", got)
	}
	env[PiSessionDirEnvVar] = "/explicit"
	if got, _ := piSessionsDir(getenv, "/cwd"); got != "/explicit" {
		t.Fatalf("env sessions dir = %q", got)
	}
}

func TestPiModels(t *testing.T) {
	m := piModels{}
	for _, ok := range []string{"", "anthropic/claude-opus-5-5", "sonnet:high", "openrouter/meta-llama/llama-3.1-8b", "gpt-5@azure"} {
		if _, err := m.ValidateModel(ok); err != nil {
			t.Errorf("ValidateModel(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"two words", "-x", "a;b", strings.Repeat("a", 201)} {
		if _, err := m.ValidateModel(bad); err == nil {
			t.Errorf("ValidateModel(%q) accepted", bad)
		}
	}
	if _, err := m.ValidateEffort("xhigh"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ValidateEffort("ultra"); err == nil {
		t.Fatal("unknown effort accepted")
	}
}

func TestParsePiModelList(t *testing.T) {
	out := "provider   model                       context  max-out  thinking  images\n" +
		"anthropic  claude-opus-5-5             1M       128K     yes       yes\n" +
		"mock       mock-1                      64K      4.1K     no        no\n"
	got := parsePiModelList(out)
	if strings.Join(got, ",") != "anthropic/claude-opus-5-5,mock/mock-1" {
		t.Fatalf("parsed %v", got)
	}
	none := "No models available. Use /login to log into a provider via OAuth or API key. See:\n  /x/docs/providers.md\n"
	if got := parsePiModelList(none); len(got) != 0 {
		t.Fatalf("prose parsed as models: %v", got)
	}
}

func TestPiCatalogs(t *testing.T) {
	if p, err := (piApproval{}).ValidatePolicy("read-only"); err != nil || p != PiApprovalReadOnly {
		t.Fatalf("read-only = %q, %v", p, err)
	}
	if _, err := (piApproval{}).ValidatePolicy("yolo"); err == nil {
		t.Fatal("foreign approval mode accepted")
	}
	if args := piApprovalArgs(PiApprovalAuto); args != nil {
		t.Fatalf("auto renders %v", args)
	}
	if m, err := (piSandbox{}).ValidateMode("off"); err != nil || m != PiSandboxOff {
		t.Fatalf("off = %q, %v", m, err)
	}
	if _, err := (piSandbox{}).ValidateMode("seatbelt"); err == nil {
		t.Fatal("pi accepted a sandbox it does not have")
	}
	h := MustGet(PiName)
	if err := ValidateHarnessBuiltinOSSandbox(h); err == nil {
		t.Fatal("pi must refuse harness-builtin sandboxing")
	}
}

func TestPiExtraArgs(t *testing.T) {
	for _, owned := range [][]string{{"--session", "x"}, {"-c"}, {"--model=x"}, {"-t", "read"}, {"--api-key", "k"}, {"-na"}, {"--print"}} {
		if err := ValidateLaunchExtraArgs(MustGet(PiName), owned); err == nil {
			t.Errorf("%v accepted", owned)
		}
	}
	for _, free := range [][]string{{"--verbose"}, {"--offline"}, {"--", "--session"}} {
		if err := ValidateLaunchExtraArgs(MustGet(PiName), free); err != nil {
			t.Errorf("%v refused: %v", free, err)
		}
	}
}

func TestPiExperimentalGate(t *testing.T) {
	t.Setenv(ExperimentalHarnessesEnvVar, "")
	t.Setenv("HOME", t.TempDir())
	restore := SetExperimentalHarnessEnabledForTest(PiName, false)
	defer restore()
	if _, err := ResolveSpawnable(PiName); err == nil || !strings.Contains(err.Error(), "experimental") {
		t.Fatalf("disabled pi spawnable: %v", err)
	}
	if _, err := Resolve(PiName); err != nil {
		t.Fatalf("pi must still resolve: %v", err)
	}
	if _, err := ResolveSpawnCatalog(PiName); err != nil {
		t.Fatalf("client-side pre-validation must leave the opt-in to the daemon: %v", err)
	}
	restore()
	t.Setenv(ExperimentalHarnessesEnvVar, "codex, pi")
	if _, err := ResolveSpawnable(PiName); err != nil {
		t.Fatalf("opted-in pi refused: %v", err)
	}
}
