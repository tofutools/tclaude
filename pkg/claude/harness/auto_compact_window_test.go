package harness

import "testing"

func TestParseAutoCompactWindow(t *testing.T) {
	// Every accepted spelling, and the one canonical form they all collapse to.
	// "" is the unset sentinel and round-trips as itself.
	cases := map[string]string{
		"":         "",
		"   ":      "",
		"450000":   "450000",
		" 450000 ": "450000",
		"450_000":  "450000",
		"450 000":  "450000",
		"450k":     "450000",
		"450K":     "450000",
		"1m":       "1000000",
		"1M":       "1000000",
		"0.5M":     "500000",
		"0.45M":    "450000",
		"10000":    "10000",    // the floor
		"10000000": "10000000", // the ceiling
	}
	for in, want := range cases {
		got, err := ParseAutoCompactWindow(in)
		if err != nil {
			t.Errorf("ParseAutoCompactWindow(%q) unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseAutoCompactWindow(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestParseAutoCompactWindowRejects(t *testing.T) {
	// Split by intent: malformed input, then well-formed but unusable input.
	for _, in := range []string{
		"abc", "450 000 tokens", "45o000", "-450000", "450.", ".", "k", "450kk", "1e6",
	} {
		if got, err := ParseAutoCompactWindow(in); err == nil {
			t.Errorf("ParseAutoCompactWindow(%q) = %q, nil; want a syntax error", in, got)
		}
	}
	// A fraction finer than the suffix can express is not a whole token count.
	if _, err := ParseAutoCompactWindow("1.0005k"); err == nil {
		t.Error("ParseAutoCompactWindow(\"1.0005k\") = nil error; want a whole-tokens error")
	}
	// Out of range in both directions, including a slipped digit.
	for _, in := range []string{"1", "9999", "1500", "4500000000", "100M"} {
		if _, err := ParseAutoCompactWindow(in); err == nil {
			t.Errorf("ParseAutoCompactWindow(%q) = nil error; want a range error", in)
		}
	}
}

func TestParseAutoCompactWindowNeverOverflows(t *testing.T) {
	// A digit string long enough to overflow int64 must surface as a range
	// error, not as a wrapped-around value that then passes the bounds check.
	for _, in := range []string{
		"99999999999999999999", "9999999999999999999999999999M",
	} {
		got, err := ParseAutoCompactWindow(in)
		if err == nil {
			t.Errorf("ParseAutoCompactWindow(%q) = %q, nil; want a range error", in, got)
		}
	}
}

func TestResolveAutoCompactWindowHarnessGate(t *testing.T) {
	claude, err := Resolve(DefaultName)
	if err != nil {
		t.Fatalf("resolve claude: %v", err)
	}
	codex, err := Resolve(CodexName)
	if err != nil {
		t.Fatalf("resolve codex: %v", err)
	}

	got, err := ResolveAutoCompactWindow(claude, "450k")
	if err != nil || got != "450000" {
		t.Errorf("ResolveAutoCompactWindow(claude, \"450k\") = %q, %v; want \"450000\", nil", got, err)
	}
	// Blank is valid for EVERY harness — it means "pin nothing", which is not a
	// Claude Code feature request at all.
	if got, err := ResolveAutoCompactWindow(codex, ""); err != nil || got != "" {
		t.Errorf("ResolveAutoCompactWindow(codex, \"\") = %q, %v; want \"\", nil", got, err)
	}
	// A real value for a harness with no such knob must be an error rather than a
	// silent drop, so a Claude profile carried onto a Codex spawn surfaces at the
	// boundary instead of vanishing at runtime.
	if _, err := ResolveAutoCompactWindow(codex, "450k"); err == nil {
		t.Error("ResolveAutoCompactWindow(codex, \"450k\") = nil error; want a harness-gate error")
	}
	if !claude.CanAutoCompactWindow() {
		t.Error("claude.CanAutoCompactWindow() = false; want true")
	}
	if codex.CanAutoCompactWindow() {
		t.Error("codex.CanAutoCompactWindow() = true; want false")
	}
}

func TestEffectiveContextWindow(t *testing.T) {
	cases := []struct {
		name       string
		model, pin int64
		want       int64
	}{
		{"pin below model wins", 1_000_000, 450_000, 450_000},
		{"model below pin wins — Claude Code caps the pin anyway", 200_000, 450_000, 200_000},
		{"no pin", 1_000_000, 0, 1_000_000},
		{"model window not reported yet", 0, 450_000, 450_000},
		{"nothing known", 0, 0, 0},
		{"equal", 450_000, 450_000, 450_000},
		{"negative pin is treated as unset", 1_000_000, -5, 1_000_000},
	}
	for _, c := range cases {
		if got := EffectiveContextWindow(c.model, c.pin); got != c.want {
			t.Errorf("%s: EffectiveContextWindow(%d, %d) = %d; want %d", c.name, c.model, c.pin, got, c.want)
		}
	}
}

func TestRebaseContextPercentage(t *testing.T) {
	// The headline case: 21% of a 1M window is ~47% of the way to a 450K pin.
	if got := RebaseContextPercentage(21, 1_000_000, 450_000); int(got) != 46 {
		t.Errorf("RebaseContextPercentage(21, 1M, 450k) = %v; want ~46.67", got)
	}
	// No pin, an unknown window, or a pin at/above the model window all leave the
	// harness's own percentage exactly as it reported it.
	for _, c := range []struct {
		pct        float64
		model, eff int64
	}{
		{21, 1_000_000, 1_000_000},
		{21, 0, 450_000},
		{21, 1_000_000, 0},
		{0, 1_000_000, 450_000},
	} {
		if got := RebaseContextPercentage(c.pct, c.model, c.eff); got != c.pct {
			t.Errorf("RebaseContextPercentage(%v, %d, %d) = %v; want it unchanged", c.pct, c.model, c.eff, got)
		}
	}
	// An agent briefly above its pinned window clamps at 100 rather than
	// rendering a >100% bar.
	if got := RebaseContextPercentage(60, 1_000_000, 450_000); got != 100 {
		t.Errorf("RebaseContextPercentage(60, 1M, 450k) = %v; want 100 (clamped)", got)
	}
}

func TestAutoCompactWindowTokens(t *testing.T) {
	for in, want := range map[string]int64{
		"":        0,
		"  ":      0,
		"450000":  450_000,
		" 450000": 450_000,
		"nope":    0,
		"-1":      0,
		"0":       0,
	} {
		if got := AutoCompactWindowTokens(in); got != want {
			t.Errorf("AutoCompactWindowTokens(%q) = %d; want %d", in, got, want)
		}
	}
}

func TestFormatAutoCompactWindow(t *testing.T) {
	for in, want := range map[string]string{
		"":        "",
		"450000":  "450k",
		"1000000": "1M",
		"450500":  "450500",
		"nope":    "",
	} {
		if got := FormatAutoCompactWindow(in); got != want {
			t.Errorf("FormatAutoCompactWindow(%q) = %q; want %q", in, got, want)
		}
	}
}
