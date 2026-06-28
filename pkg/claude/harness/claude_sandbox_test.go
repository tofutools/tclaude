package harness

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestClaudeSandbox_Catalog pins the catalog the spawn dialog / profile editor
// / CLI drive their Claude sandbox selector off: the inherit/on/off mode set,
// the inherit default (the dropdown's recommended option), and the
// normalization that makes inherit collapse to "" (no override).
func TestClaudeSandbox_Catalog(t *testing.T) {
	c := claudeSandbox{}

	if got := c.Modes(); !equalStrings(got, []string{"inherit", "on", "off"}) {
		t.Fatalf("Modes() = %v, want [inherit on off]", got)
	}
	if got := c.DefaultMode(); got != ClaudeSandboxInherit {
		t.Fatalf("DefaultMode() = %q, want %q", got, ClaudeSandboxInherit)
	}

	// ValidateMode: both "" and inherit normalize to "" (omit the override);
	// on/off return themselves; anything else errors.
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"inherit", "", false},
		{"  inherit  ", "", false}, // trimmed then normalized
		{"on", "on", false},
		{"off", "off", false},
		{"workspace-write", "", true}, // a Codex mode is not a Claude mode
		{"enabled", "", true},
	}
	for _, tc := range cases {
		got, err := c.ValidateMode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ValidateMode(%q) = (%q, nil), want error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("ValidateMode(%q) = (%q, %v), want (%q, nil)", tc.in, got, err, tc.want)
		}
	}

	// Every selectable mode carries help; only off flags a ⚠ caveat.
	for _, m := range c.Modes() {
		if c.ModeHelp(m) == "" {
			t.Fatalf("ModeHelp(%q) is empty", m)
		}
	}
	if strings.Contains(c.ModeHelp("inherit"), "⚠") {
		t.Fatalf("inherit (the default) must carry no ⚠ caveat: %q", c.ModeHelp("inherit"))
	}
	if !strings.Contains(c.ModeHelp("off"), "⚠") {
		t.Fatalf("off must flag a ⚠ sandbox-disabled caveat: %q", c.ModeHelp("off"))
	}
}

// TestClaudeSandbox_HarnessResolution pins how the harness-level resolvers
// treat Claude's sandbox: SupportsSandbox is true, an explicit on/off
// validates, inherit/blank resolves to "" (omit), and an invalid mode errors —
// the same entry points the daemon (ResolveSandboxMode) and the direct CLI
// (ValidateSandboxMode) use.
func TestClaudeSandbox_HarnessResolution(t *testing.T) {
	h, err := Resolve(DefaultName)
	if err != nil {
		t.Fatalf("Resolve(claude): %v", err)
	}
	if !h.SupportsSandbox() {
		t.Fatal("claude must SupportsSandbox now (per-session --settings override)")
	}

	// Daemon path: blank resolves to the inherit default, which normalizes to ""
	// — an un-chosen Claude spawn imposes no sandbox override.
	if got, err := ResolveSandboxMode(h, ""); err != nil || got != "" {
		t.Fatalf("ResolveSandboxMode(claude, \"\") = (%q, %v), want (\"\", nil)", got, err)
	}
	if got, err := ResolveSandboxMode(h, "on"); err != nil || got != "on" {
		t.Fatalf("ResolveSandboxMode(claude, on) = (%q, %v), want (on, nil)", got, err)
	}
	// Direct CLI path: same validation, no defaulting.
	if got, err := ValidateSandboxMode(h, "off"); err != nil || got != "off" {
		t.Fatalf("ValidateSandboxMode(claude, off) = (%q, %v), want (off, nil)", got, err)
	}
	if _, err := ValidateSandboxMode(h, "danger-full-access"); err == nil {
		t.Fatal("a Codex sandbox mode must be rejected for claude")
	}
}

// TestClaudeSpawner_Sandbox is the acceptance check for the spawn surface: the
// on/off modes deliver a `--settings '<json>'` override (Claude Code has no
// `--sandbox` flag); inherit / unset emit nothing, leaving the agent on the
// operator's own settings.json.
func TestClaudeSpawner_Sandbox(t *testing.T) {
	spawn := func(mode string) string {
		return claudeSpawner{}.BuildCommand(SpawnSpec{SandboxMode: mode})
	}

	// inherit / unset → no --settings anywhere.
	for _, mode := range []string{"", "inherit"} {
		if got := spawn(mode); strings.Contains(got, "--settings") {
			t.Fatalf("mode %q must omit --settings, got %q", mode, got)
		}
	}

	// on / off → the command carries a --settings flag. The payload itself is
	// verified via claudeSandboxSettingsJSON below rather than by re-parsing the
	// shell-quoted command arg (whose escaping is quoting-style-specific and
	// fragile to assert against).
	for _, mode := range []string{"on", "off"} {
		if got := spawn(mode); !strings.Contains(got, "--settings ") {
			t.Fatalf("mode %q must emit --settings, got %q", mode, got)
		}
	}

	// on enables the sandbox AND preserves agentd reachability (the socket
	// allowlist) so the agent can still coordinate.
	on := sandboxBlock(t, "on")
	if on["enabled"] != true {
		t.Fatalf("on must set sandbox.enabled=true, got %v", on["enabled"])
	}
	if !strings.Contains(claudeSandboxSettingsJSON("on"), "agentd.sock") {
		t.Fatal("on must allowlist the agentd socket so the agent can run `tclaude agent`")
	}

	// off disables the sandbox.
	if off := sandboxBlock(t, "off"); off["enabled"] != false {
		t.Fatalf("off must set sandbox.enabled=false, got %v", off["enabled"])
	}
}

// TestClaudeSandboxOnBlock_MatchesHardening guards the single-source-of-truth
// contract: the per-session `on` block IS the block the global
// `--install-sandbox-hardening` setup writes, so they cannot drift. (The setup
// package asserts its own spec separately; here we just pin the keys the
// spawner/setup both depend on.)
func TestClaudeSandboxOnBlock_MatchesHardening(t *testing.T) {
	b := ClaudeSandboxOnBlock()
	if b["enabled"] != true {
		t.Fatalf("on block must enable the sandbox, got %v", b["enabled"])
	}
	net, _ := b["network"].(map[string]any)
	if net == nil || net["allowAllUnixSockets"] != true {
		t.Fatalf("on block must keep unix sockets reachable, got %v", b["network"])
	}
	fs, _ := b["filesystem"].(map[string]any)
	if fs == nil {
		t.Fatalf("on block must protect tclaude state via filesystem rules, got %v", b)
	}
	if dr, _ := fs["denyRead"].([]any); len(dr) == 0 {
		t.Fatalf("on block must denyRead tclaude state, got %v", fs["denyRead"])
	}
	// Fresh map each call so the setup merge engine can mutate it in place
	// without aliasing a later caller's block: mutating one must not be visible
	// in the next.
	b["enabled"] = "mutated"
	if again := ClaudeSandboxOnBlock(); again["enabled"] != true {
		t.Fatal("ClaudeSandboxOnBlock must return a fresh map each call (mutation leaked)")
	}
}

// sandboxBlock parses claudeSandboxSettingsJSON(mode) and returns its inner
// `sandbox` block. Asserting against the builder's output directly — rather than
// re-parsing the shell-quoted BuildCommand arg — keeps the test robust to the
// command's quoting style (single vs double quotes, escaping). The command's job
// is just to carry this payload as one `--settings` arg, checked separately.
func sandboxBlock(t *testing.T, mode string) map[string]any {
	t.Helper()
	payload := claudeSandboxSettingsJSON(mode)
	var wrap map[string]any
	if err := json.Unmarshal([]byte(payload), &wrap); err != nil {
		t.Fatalf("claudeSandboxSettingsJSON(%q) is not valid JSON (%v): %q", mode, err, payload)
	}
	block, ok := wrap["sandbox"].(map[string]any)
	if !ok {
		t.Fatalf("claudeSandboxSettingsJSON(%q) missing a sandbox block: %v", mode, wrap)
	}
	return block
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
