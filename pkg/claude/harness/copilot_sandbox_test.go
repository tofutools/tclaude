package harness

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// writeCopilotSettings creates a COPILOT_HOME containing settings.json with the
// given raw body, and returns a getenv that points a launch at it.
func writeCopilotSettings(t *testing.T, body string) (home string, getenv func(string) string) {
	t.Helper()
	home = t.TempDir()
	stateDir := filepath.Join(home, ".copilot")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatalf("prepare Copilot home: %v", err)
	}
	if body != "" {
		path := filepath.Join(stateDir, CopilotSettingsFileName)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write Copilot settings: %v", err)
		}
	}
	return home, func(string) string { return "" }
}

// TestCopilotSandboxCatalog pins the mode set. The absence of an `on` mode is
// the assertion that matters: Copilot's own sandbox has no launch lever, so a
// catalog offering to turn it on would be advertising a capability tclaude
// cannot perform.
func TestCopilotSandboxCatalog(t *testing.T) {
	catalog := copilotSandbox{}
	if got := catalog.DefaultMode(); got != CopilotSandboxInherit {
		t.Fatalf("DefaultMode() = %q, want %q", got, CopilotSandboxInherit)
	}
	modes := catalog.Modes()
	want := []string{CopilotSandboxInherit, CopilotSandboxOff}
	if len(modes) != len(want) {
		t.Fatalf("Modes() = %v, want %v", modes, want)
	}
	for i, mode := range want {
		if modes[i] != mode {
			t.Fatalf("Modes() = %v, want %v", modes, want)
		}
		if catalog.ModeHelp(mode) == "" {
			t.Errorf("ModeHelp(%q) is empty; every selectable mode needs help copy", mode)
		}
	}
	if catalog.ModeHelp("on") != "" {
		t.Error("ModeHelp(\"on\") must be empty: copilot has no sandbox-enabling mode")
	}

	for _, mode := range []string{"", CopilotSandboxInherit, CopilotSandboxOff} {
		got, err := catalog.ValidateMode(mode)
		if err != nil || got != mode {
			t.Fatalf("ValidateMode(%q) = (%q, %v), want (%q, nil)", mode, got, err, mode)
		}
	}
	// `inherit` must NOT collapse to "" — an actively chosen inherit has to stay
	// distinguishable from an omitted mode, or a profile overlay silently wins.
	if got, _ := catalog.ValidateMode("  " + CopilotSandboxInherit + " "); got != CopilotSandboxInherit {
		t.Fatalf("ValidateMode trims to %q, want %q", got, CopilotSandboxInherit)
	}
	if _, err := catalog.ValidateMode("on"); err == nil {
		t.Fatal("ValidateMode(\"on\") must be refused")
	}
}

// TestCopilotTclaudeLayerMode pins the descriptor wiring end to end: the mode
// tclaude-layer resolves to, and the sandbox-off mapping a temporary operator
// unlock uses.
func TestCopilotTclaudeLayerMode(t *testing.T) {
	h := MustGet(CopilotName)
	mode, err := TclaudeLayerSandboxMode(h)
	if err != nil {
		t.Fatalf("TclaudeLayerSandboxMode(copilot) = %v, want the assert-off mode", err)
	}
	if mode != CopilotSandboxOff {
		t.Fatalf("TclaudeLayerSandboxMode(copilot) = %q, want %q", mode, CopilotSandboxOff)
	}
	off, err := SandboxOffMode(h)
	if err != nil || off != CopilotSandboxOff {
		t.Fatalf("SandboxOffMode(copilot) = (%q, %v), want (%q, nil)", off, err, CopilotSandboxOff)
	}
}

// TestResolveCopilotInnerSandboxReadsSettings covers the states that are
// DETERMINATE — the ones a launch may proceed from.
func TestResolveCopilotInnerSandboxReadsSettings(t *testing.T) {
	tests := []struct {
		name             string
		body             string
		wantPresent      bool
		wantEnabled      bool
		wantExperimental bool
	}{
		{
			name: "no settings file at all is a clean off",
			body: "",
		},
		{
			name:        "a settings file that says nothing about the sandbox is off",
			body:        `{"model":"claude-sonnet-5"}`,
			wantPresent: true,
		},
		{
			name:        "an explicit disabled sandbox is off",
			body:        `{"sandbox":{"enabled":false,"allowBypass":true}}`,
			wantPresent: true,
		},
		{
			name:        "an enabled sandbox is reported, not refused, by the reader",
			body:        `{"sandbox":{"enabled":true}}`,
			wantPresent: true,
			wantEnabled: true,
		},
		{
			name:             "experimental is read from the top level",
			body:             `{"experimental":true}`,
			wantPresent:      true,
			wantExperimental: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home, getenv := writeCopilotSettings(t, tc.body)
			state, err := ResolveCopilotInnerSandbox(getenv, home)
			if err != nil {
				t.Fatalf("ResolveCopilotInnerSandbox = %v, want a determinate state", err)
			}
			if state.Present != tc.wantPresent {
				t.Errorf("Present = %v, want %v", state.Present, tc.wantPresent)
			}
			if state.Enabled != tc.wantEnabled {
				t.Errorf("Enabled = %v, want %v", state.Enabled, tc.wantEnabled)
			}
			if state.Experimental != tc.wantExperimental {
				t.Errorf("Experimental = %v, want %v", state.Experimental, tc.wantExperimental)
			}
			wantPath := filepath.Join(home, ".copilot", CopilotSettingsFileName)
			if state.SettingsPath != wantPath {
				t.Errorf("SettingsPath = %q, want %q", state.SettingsPath, wantPath)
			}
		})
	}
}

// TestResolveCopilotInnerSandboxRefusesAmbiguity is the load-bearing half. Each
// body below would unmarshal to the zero value under a typed struct — which for
// a field named `enabled` reads as "sandbox off" — so a resolver that did not
// inspect the SHAPE would wave through exactly the configurations this gate
// exists to catch.
func TestResolveCopilotInnerSandboxRefusesAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "settings that are not JSON", body: `{not json`},
		{name: "settings that are a JSON array", body: `[]`},
		{name: "a sandbox key that is a bare boolean", body: `{"sandbox":true}`},
		{name: "a sandbox key that is a string", body: `{"sandbox":"on"}`},
		{name: "a stringly-typed enabled value", body: `{"sandbox":{"enabled":"true"}}`},
		{name: "a numeric enabled value", body: `{"sandbox":{"enabled":1}}`},
		{name: "a stringly-typed experimental value", body: `{"experimental":"on"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, getenv := writeCopilotSettings(t, tc.body)
			_, err := ResolveCopilotInnerSandbox(getenv, home)
			var capErr *SandboxCapabilityError
			if !errors.As(err, &capErr) {
				t.Fatalf("ResolveCopilotInnerSandbox = %v, want a *SandboxCapabilityError", err)
			}
			if capErr.Kind != SandboxCapabilityCopilotInnerSandbox {
				t.Errorf("Kind = %q, want %q", capErr.Kind, SandboxCapabilityCopilotInnerSandbox)
			}
			if capErr.Harness != CopilotName {
				t.Errorf("Harness = %q, want %q", capErr.Harness, CopilotName)
			}
		})
	}
}

// TestResolveCopilotInnerSandboxHonorsCopilotHome proves the reader and the
// sandbox baseline agree about which settings file governs a launch: both
// resolve COPILOT_HOME the same way, so an operator who moved it does not get
// a gate reading a file the CLI will never open.
func TestResolveCopilotInnerSandboxHonorsCopilotHome(t *testing.T) {
	home := t.TempDir()
	moved := filepath.Join(home, "elsewhere", "copilot")
	if err := os.MkdirAll(moved, 0o700); err != nil {
		t.Fatalf("prepare moved Copilot home: %v", err)
	}
	if err := os.WriteFile(filepath.Join(moved, CopilotSettingsFileName),
		[]byte(`{"sandbox":{"enabled":true}}`), 0o600); err != nil {
		t.Fatalf("write moved settings: %v", err)
	}
	getenv := func(name string) string {
		if name == CopilotHomeEnvVar {
			return moved
		}
		return ""
	}
	state, err := ResolveCopilotInnerSandbox(getenv, home)
	if err != nil {
		t.Fatalf("ResolveCopilotInnerSandbox = %v", err)
	}
	if !state.Enabled {
		t.Fatal("the moved COPILOT_HOME's settings were not read")
	}
	entries, err := CopilotSandboxBaseline(CopilotBaselineInput{Home: home, Getenv: getenv})
	if err != nil {
		t.Fatalf("CopilotSandboxBaseline = %v", err)
	}
	var stateDir string
	for _, entry := range entries {
		if entry.ID == CopilotBaselineStateDir {
			stateDir = entry.Path
		}
	}
	if filepath.Dir(state.SettingsPath) != stateDir {
		t.Fatalf("the assert-off gate inspects %q but the baseline grants %q; "+
			"the two must name one directory", state.SettingsPath, stateDir)
	}
}

// TestValidateCopilotTclaudeLayerInnerSandbox pins the assert-off verdicts and,
// as importantly, that each refusal NAMES the file and key an operator has to
// change.
func TestValidateCopilotTclaudeLayerInnerSandbox(t *testing.T) {
	const path = "/home/u/.copilot/settings.json"
	if err := ValidateCopilotTclaudeLayerInnerSandbox(CopilotInnerSandboxState{
		SettingsPath: path,
	}); err != nil {
		t.Fatalf("a determinate off state must launch: %v", err)
	}
	if err := ValidateCopilotTclaudeLayerInnerSandbox(CopilotInnerSandboxState{
		SettingsPath: path, Present: true,
	}); err != nil {
		t.Fatalf("an explicit off state must launch: %v", err)
	}

	for _, tc := range []struct {
		name     string
		state    CopilotInnerSandboxState
		wantWord string
	}{
		{
			name:     "an enabled inner sandbox would stack two walls",
			state:    CopilotInnerSandboxState{SettingsPath: path, Present: true, Enabled: true},
			wantWord: "sandbox.enabled",
		},
		{
			name:     "experimental features expose the in-pane /sandbox command",
			state:    CopilotInnerSandboxState{SettingsPath: path, Present: true, Experimental: true},
			wantWord: "experimental",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCopilotTclaudeLayerInnerSandbox(tc.state)
			var capErr *SandboxCapabilityError
			if !errors.As(err, &capErr) {
				t.Fatalf("got %v, want a *SandboxCapabilityError", err)
			}
			if capErr.Kind != SandboxCapabilityCopilotInnerSandbox {
				t.Errorf("Kind = %q, want %q", capErr.Kind, SandboxCapabilityCopilotInnerSandbox)
			}
			if !strings.Contains(capErr.Message, tc.wantWord) {
				t.Errorf("message %q does not name %q", capErr.Message, tc.wantWord)
			}
			if !strings.Contains(capErr.Message, path) {
				t.Errorf("message %q does not name the settings file", capErr.Message)
			}
		})
	}
}

// TestCopilotTclaudeLayerExtraArgRefusal closes the argv half of the gate: the
// settings reader inspects a file, and `--experimental` re-opens the same
// in-pane path from the command line.
func TestCopilotTclaudeLayerExtraArgRefusal(t *testing.T) {
	for _, args := range [][]string{
		{"--experimental"},
		{"--banner", "--experimental"},
		{"--experimental=true"},
	} {
		err := CopilotTclaudeLayerExtraArgRefusal(args)
		var capErr *SandboxCapabilityError
		if !errors.As(err, &capErr) {
			t.Fatalf("CopilotTclaudeLayerExtraArgRefusal(%v) = %v, want a refusal", args, err)
		}
	}
	// Autonomy flags are NOT refused: they widen Copilot's own prompts, which
	// the outer wall still contains. Refusing them would confuse "this agent
	// may act without asking" with "this agent has a second sandbox".
	for _, args := range [][]string{
		nil,
		{"--yolo"},
		{"--allow-all"},
		{"--no-experimental"},
	} {
		if err := CopilotTclaudeLayerExtraArgRefusal(args); err != nil {
			t.Errorf("CopilotTclaudeLayerExtraArgRefusal(%v) = %v, want nil", args, err)
		}
	}
}

// TestCopilotTclaudeLayerGrantsTranslateBaseline proves the translation
// preserves the three properties the mount plan depends on: mode, exec-bearing
// rows, and one grant per catalog row (not per row ID).
func TestCopilotTclaudeLayerGrantsTranslateBaseline(t *testing.T) {
	home := t.TempDir()
	socketA := filepath.Join(home, ".tclaude", "api", "agentd.sock")
	socketB := filepath.Join(home, ".tclaude-agentd.sock")
	set, err := CopilotTclaudeLayerGrants(CopilotBaselineInput{
		GOOS:              "linux",
		Home:              home,
		Getenv:            func(string) string { return "" },
		TempDir:           filepath.Join(home, "tmp"),
		AgentdSockets:     []string{socketA, socketB},
		CopilotExecutable: filepath.Join(home, "bin", "copilot"),
		TclaudeExecutable: filepath.Join(home, "bin", "tclaude"),
		Workspace:         filepath.Join(home, "work"),
	})
	if err != nil {
		t.Fatalf("CopilotTclaudeLayerGrants = %v", err)
	}
	if len(set.Grants) != len(set.Entries) {
		t.Fatalf("got %d grants for %d catalog rows; the translation must be 1:1",
			len(set.Grants), len(set.Entries))
	}

	access := make(map[string]sandboxpolicy.Access, len(set.Grants))
	for _, grant := range set.Grants {
		access[grant.Path] = grant.Access
		if grant.MountPath != "" {
			t.Errorf("grant %q is remapped; every Copilot row is a same-path grant", grant.Path)
		}
	}

	// The state directory and both caches are writable; the executables are not.
	for path, want := range map[string]sandboxpolicy.Access{
		filepath.Join(home, ".copilot"):                              sandboxpolicy.AccessWrite,
		filepath.Join(home, ".cache", "copilot"):                     sandboxpolicy.AccessWrite,
		filepath.Join(home, ".cache", "Microsoft", "DeveloperTools"): sandboxpolicy.AccessWrite,
		filepath.Join(home, "tmp"):                                   sandboxpolicy.AccessWrite,
		socketA:                                                      sandboxpolicy.AccessWrite,
		socketB:                                                      sandboxpolicy.AccessWrite,
		filepath.Join(home, "bin", "copilot"):                        sandboxpolicy.AccessRead,
		filepath.Join(home, "bin", "tclaude"):                        sandboxpolicy.AccessRead,
	} {
		if got, found := access[path]; !found || got != want {
			t.Errorf("grant for %q = (%q, present=%v), want %q", path, got, found, want)
		}
	}

	// The package cache MUST be exec-bearing: it holds the bundled ripgrep and
	// tgrep binaries and prebuilt native modules the CLI loads and runs, so a
	// noexec mount would break tool search in a way that reads as a Copilot bug
	// rather than a sandbox one.
	wantExec := map[string]bool{
		filepath.Join(home, ".cache", "copilot"): true,
		filepath.Join(home, "bin", "copilot"):    true,
		filepath.Join(home, "bin", "tclaude"):    true,
	}
	if len(set.ExecutablePaths) != len(wantExec) {
		t.Fatalf("ExecutablePaths = %v, want exactly %v", set.ExecutablePaths, wantExec)
	}
	for _, path := range set.ExecutablePaths {
		if !wantExec[path] {
			t.Errorf("unexpected exec-bearing path %q", path)
		}
	}

	// Two distinct agentd endpoints produce two rows: the row ID is a KIND, and
	// a translation keyed by ID would have kept only the last one.
	sockets := 0
	for _, entry := range set.Entries {
		if entry.ID == CopilotBaselineAgentdSocket {
			sockets++
		}
	}
	if sockets != 2 {
		t.Fatalf("got %d agentd socket rows, want 2 (the row ID is a kind, not a key)", sockets)
	}
}

// TestCopilotTclaudeLayerGrantsPropagateBaselineRefusals proves the refusal set
// is part of the contract: the translation never repairs, narrows or drops a
// row the catalog refused, and it preserves the catalog's own wire kind so the
// daemon still renders the right remedy.
func TestCopilotTclaudeLayerGrantsPropagateBaselineRefusals(t *testing.T) {
	home := t.TempDir()
	for _, tc := range []struct {
		name   string
		input  CopilotBaselineInput
		reason string
	}{
		{
			name: "a COPILOT_HOME pointed at HOME",
			input: CopilotBaselineInput{
				GOOS: "linux", Home: home,
				Getenv: func(name string) string {
					if name == CopilotHomeEnvVar {
						return home
					}
					return ""
				},
			},
			reason: "grants the whole home directory",
		},
		{
			name: "a cache home landing on the shared XDG base",
			input: CopilotBaselineInput{
				GOOS: "linux", Home: home,
				Getenv: func(name string) string {
					if name == CopilotCacheHomeEnvVar {
						return filepath.Join(home, ".cache")
					}
					return ""
				},
			},
			reason: "grants every other application's cache",
		},
		{
			name: "an agentd endpoint inside tclaude's private state",
			input: CopilotBaselineInput{
				GOOS: "linux", Home: home,
				Getenv:        func(string) string { return "" },
				AgentdSockets: []string{filepath.Join(home, ".tclaude", "data", "agentd.sock")},
			},
			reason: "reaches through the deny that protects the daemon database",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CopilotTclaudeLayerGrants(tc.input)
			var capErr *SandboxCapabilityError
			if !errors.As(err, &capErr) {
				t.Fatalf("got %v, want a refusal because it %s", err, tc.reason)
			}
			if !strings.HasPrefix(capErr.Kind, "copilot-sandbox-baseline-") {
				t.Errorf("Kind = %q, want the baseline's own kind preserved", capErr.Kind)
			}
		})
	}
}
