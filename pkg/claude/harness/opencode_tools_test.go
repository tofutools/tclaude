package harness

import (
	"strings"
	"testing"
)

func TestOpenCodeToolGovernanceCatalog(t *testing.T) {
	c := openCodeToolGovernance{}
	want := []string{OpenCodeToolsAllow, OpenCodeToolsAsk, OpenCodeToolsDeny}
	if got := c.Modes(); !equalStrings(got, want) {
		t.Fatalf("Modes() = %v, want %v", got, want)
	}
	if got := c.DefaultPolicy(); got != OpenCodeToolsAllow {
		t.Fatalf("DefaultPolicy() = %q, want %q", got, OpenCodeToolsAllow)
	}
	for _, mode := range want {
		got, err := c.ValidatePolicy("  " + mode + "  ")
		if err != nil || got != mode {
			t.Fatalf("ValidatePolicy(%q) = (%q, %v), want (%q, nil)", mode, got, err, mode)
		}
		if c.ModeHelp(mode) == "" {
			t.Fatalf("ModeHelp(%q) is empty", mode)
		}
	}
	if _, err := c.ValidatePolicy("sometimes"); err == nil {
		t.Fatal("ValidatePolicy(sometimes) must reject an unsupported action")
	}
	if !strings.Contains(c.ModeHelp(OpenCodeToolsAsk), "⚠") {
		t.Fatalf("ask must warn that detached agents can block: %q", c.ModeHelp(OpenCodeToolsAsk))
	}
}

func TestToolGovernanceHarnessResolution(t *testing.T) {
	opencode, err := Resolve(OpenCodeName)
	if err != nil {
		t.Fatalf("Resolve(opencode): %v", err)
	}
	if !opencode.SupportsToolGovernance() {
		t.Fatal("OpenCode must expose the tool-governance axis")
	}
	if got, err := ValidateToolGovernance(opencode, ""); err != nil || got != "" {
		t.Fatalf("ValidateToolGovernance(opencode, blank) = (%q, %v), want blank", got, err)
	}
	if got, err := ResolveToolGovernance(opencode, ""); err != nil || got != OpenCodeToolsAllow {
		t.Fatalf("ResolveToolGovernance(opencode, blank) = (%q, %v), want allow", got, err)
	}
	if got, err := ResolveToolGovernance(opencode, OpenCodeToolsDeny); err != nil || got != OpenCodeToolsDeny {
		t.Fatalf("ResolveToolGovernance(opencode, deny) = (%q, %v), want deny", got, err)
	}

	for _, name := range []string{DefaultName, CodexName} {
		h, resolveErr := Resolve(name)
		if resolveErr != nil {
			t.Fatalf("Resolve(%s): %v", name, resolveErr)
		}
		if h.SupportsToolGovernance() {
			t.Fatalf("%s must not expose OpenCode tool governance", name)
		}
		if got, validateErr := ValidateToolGovernance(h, ""); validateErr != nil || got != "" {
			t.Fatalf("ValidateToolGovernance(%s, blank) = (%q, %v), want blank", name, got, validateErr)
		}
		if _, validateErr := ValidateToolGovernance(h, OpenCodeToolsAsk); validateErr == nil {
			t.Fatalf("ValidateToolGovernance(%s, ask) must reject unsupported axis", name)
		}
	}
}
