package harness

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestClaudeAskTimeout_Catalog pins the catalog the spawn dialog / profile
// editor / CLI drive their Claude AskUserQuestion-timeout selector off: the
// inherit/never/60s/5m/10m value set, the inherit default (the dropdown's
// recommended option), and the tri-state normalization — "" stays "" (omitted),
// inherit stays "inherit" (a first-class sentinel, carried so an overlay won't
// override it; it collapses to "no override" only at emission), reals stay
// themselves.
func TestClaudeAskTimeout_Catalog(t *testing.T) {
	c := claudeAskTimeout{}

	want := []string{"inherit", "never", "60s", "5m", "10m"}
	if got := c.Modes(); !equalStrings(got, want) {
		t.Fatalf("Modes() = %v, want %v", got, want)
	}
	if got := c.DefaultMode(); got != ClaudeAskTimeoutInherit {
		t.Fatalf("DefaultMode() = %q, want %q", got, ClaudeAskTimeoutInherit)
	}

	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},                   // omitted stays omitted
		{"inherit", "inherit", false},     // first-class sentinel, NOT collapsed here
		{"  inherit  ", "inherit", false}, // trimmed, kept
		{"never", "never", false},
		{"60s", "60s", false},
		{"  5m  ", "5m", false}, // trimmed, kept
		{"10m", "10m", false},
		{"30s", "", true},     // not one of Claude Code's options
		{"5", "", true},       // missing unit
		{"forever", "", true}, // not "never"
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

	// Every selectable value carries help copy.
	for _, m := range c.Modes() {
		if c.ModeHelp(m) == "" {
			t.Fatalf("ModeHelp(%q) is empty", m)
		}
	}
	if c.ModeHelp("nonsense") != "" {
		t.Fatal("ModeHelp of an unknown value must be empty")
	}
}

// TestClaudeAskTimeout_HarnessResolution pins the harness-level resolver: Claude
// SupportsAskTimeout, an explicit value validates, blank resolves to "" (omitted)
// while an explicit inherit is preserved as "inherit" (the tri-state), and an
// invalid value errors — the entry point every spawn boundary uses.
func TestClaudeAskTimeout_HarnessResolution(t *testing.T) {
	h, err := Resolve(DefaultName)
	if err != nil {
		t.Fatalf("Resolve(claude): %v", err)
	}
	if !h.SupportsAskTimeout() {
		t.Fatal("claude must SupportsAskTimeout (per-session --settings override)")
	}

	// Blank → "" (omitted: a higher level may fill it). ResolveAskTimeoutMode
	// applies no default, so an un-chosen ask-timeout stays omit.
	if got, err := ResolveAskTimeoutMode(h, ""); err != nil || got != "" {
		t.Fatalf("ResolveAskTimeoutMode(claude, \"\") = (%q, %v), want (\"\", nil)", got, err)
	}
	// Explicit inherit → "inherit" (preserved, NOT collapsed): this is what keeps
	// an actively-chosen inherit from being overwritten by a profile/group default.
	if got, err := ResolveAskTimeoutMode(h, "inherit"); err != nil || got != "inherit" {
		t.Fatalf("ResolveAskTimeoutMode(claude, inherit) = (%q, %v), want (inherit, nil)", got, err)
	}
	if got, err := ResolveAskTimeoutMode(h, "5m"); err != nil || got != "5m" {
		t.Fatalf("ResolveAskTimeoutMode(claude, 5m) = (%q, %v), want (5m, nil)", got, err)
	}
	if _, err := ResolveAskTimeoutMode(h, "30s"); err == nil {
		t.Fatal("a non-option value must be rejected for claude")
	}
}

// TestClaudeSpawner_AskTimeout is the acceptance check for the spawn surface: an
// explicit value delivers a `--settings '<json>'` override carrying
// askUserQuestionTimeout; inherit / unset emit nothing, leaving the agent on the
// operator's own settings.json.
func TestClaudeSpawner_AskTimeout(t *testing.T) {
	spawn := func(v string) string {
		return claudeSpawner{}.BuildCommand(SpawnSpec{AskUserQuestionTimeout: v})
	}

	for _, v := range []string{"", "inherit"} {
		if got := spawn(v); strings.Contains(got, "--settings") {
			t.Fatalf("value %q must omit --settings, got %q", v, got)
		}
	}
	for _, v := range []string{"never", "60s", "5m", "10m"} {
		if got := spawn(v); !strings.Contains(got, "--settings ") {
			t.Fatalf("value %q must emit --settings, got %q", v, got)
		}
	}

	// The payload carries the value under the askUserQuestionTimeout key.
	s := claudeSettingsJSON(SpawnSpec{AskUserQuestionTimeout: "5m"})
	var wrap map[string]any
	if err := json.Unmarshal([]byte(s), &wrap); err != nil {
		t.Fatalf("claudeSettingsJSON is not valid JSON (%v): %q", err, s)
	}
	if wrap["askUserQuestionTimeout"] != "5m" {
		t.Fatalf("askUserQuestionTimeout = %v, want 5m (payload %q)", wrap["askUserQuestionTimeout"], s)
	}
}

// TestClaudeSettingsJSON_Merge is the load-bearing invariant of this feature:
// the sandbox block and the AskUserQuestion timeout share ONE `--settings`
// payload (the spawner emits the flag at most once), so both keys must appear
// together when both are set — and BuildCommand must carry exactly one
// --settings flag.
func TestClaudeSettingsJSON_Merge(t *testing.T) {
	spec := SpawnSpec{SandboxMode: "on", AskUserQuestionTimeout: "5m"}

	s := claudeSettingsJSON(spec)
	var wrap map[string]any
	if err := json.Unmarshal([]byte(s), &wrap); err != nil {
		t.Fatalf("merged claudeSettingsJSON is not valid JSON (%v): %q", err, s)
	}
	if _, ok := wrap["sandbox"].(map[string]any); !ok {
		t.Fatalf("merged payload missing sandbox block: %v", wrap)
	}
	if wrap["askUserQuestionTimeout"] != "5m" {
		t.Fatalf("merged payload missing askUserQuestionTimeout: %v", wrap)
	}

	// Exactly one --settings flag in the command.
	cmd := claudeSpawner{}.BuildCommand(spec)
	if n := strings.Count(cmd, " --settings "); n != 1 {
		t.Fatalf("BuildCommand must emit exactly one --settings (got %d): %q", n, cmd)
	}

	// Neither key set → no payload at all.
	if got := claudeSettingsJSON(SpawnSpec{}); got != "" {
		t.Fatalf("empty spec must yield no --settings payload, got %q", got)
	}
}
