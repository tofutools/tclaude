package common

import (
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestShellQuoteArg_RoundTripThroughShell proves the property the spawn path
// relies on when it inlines an agent's (arbitrary, possibly multi-line) startup
// briefing into the launch prompt: ShellQuoteArg's output, spliced into a
// `sh -c` command string exactly as production does, round-trips byte-for-byte
// as a SINGLE argument — no temp file needed, no character left unescaped.
// Single-quote wrapping makes every byte literal (newlines, $, backticks, ;,
// globs, …); the only special case is an embedded ' which the '\'' trick
// closes/reopens around. So we feed the genuinely nasty cases through a real
// shell and confirm what comes back equals what went in.
func TestShellQuoteArg_RoundTripThroughShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ShellQuoteArg targets sh -c; not used on Windows")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("sh unavailable: %v", err)
	}

	cases := []string{
		"plain",
		"with spaces",
		"single ' quote",
		"two '' adjacent quotes",
		`double " quote`,
		"dollar $HOME and `backtick` and $(subshell)",
		"meta ; | & < > ( ) { } * ? [ a ] # ~ !",
		"newline\nand\ttab",
		"[system: spawned by the human; read inbox #7]",
		"multi\nline\nbrief with 'quotes' and $vars and `ticks`\n- a bullet\n",
		`trailing backslash \`,
		"percent %s %d literal",
		"emoji 🚀 and unicode café",
	}
	for _, s := range cases {
		// Splice the quoted arg into a command string, exactly as the spawner
		// does (`claude … <ShellQuoteArg(prompt)>`). printf '%s' echoes its one
		// argument verbatim, so stdout must equal the original input.
		cmd := "printf '%s' " + ShellQuoteArg(s)
		out, err := exec.Command("sh", "-c", cmd).Output()
		assert.NoErrorf(t, err, "sh -c failed for %q (cmd=%q)", s, cmd)
		assert.Equalf(t, s, string(out), "round-trip mismatch (cmd=%q)", cmd)
	}
}

func TestBuildEnvExports(t *testing.T) {
	t.Setenv("TEST_VAR1", "value1")
	t.Setenv("TEST_VAR2", "value with spaces")
	t.Setenv("TEST_VAR3", "value'with'quotes")

	tests := []struct {
		name       string
		additional map[string]string
		wantVars   map[string]bool // variables that should be present
		skipVars   map[string]bool // variables that should be skipped
	}{
		{
			name:       "no additional vars",
			additional: nil,
			wantVars: map[string]bool{
				"TEST_VAR1": true,
				"TEST_VAR2": true,
				"TEST_VAR3": true,
			},
			skipVars: map[string]bool{
				"TMUX":      true,
				"TMUX_PANE": true,
			},
		},
		{
			name: "with additional vars",
			additional: map[string]string{
				"TCLAUDE_SESSION_ID": "test123",
				"CUSTOM_VAR":         "custom",
			},
			wantVars: map[string]bool{
				"TEST_VAR1":          true,
				"TCLAUDE_SESSION_ID": true,
				"CUSTOM_VAR":         true,
			},
			skipVars: map[string]bool{
				"TMUX": true,
			},
		},
		{
			name: "override existing var",
			additional: map[string]string{
				"TEST_VAR1": "overridden",
			},
			wantVars: map[string]bool{
				"TEST_VAR1": true,
			},
			skipVars: map[string]bool{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := BuildEnvExports(tt.additional)

			// Check that wanted variables are present
			for varName := range tt.wantVars {
				assert.Contains(t, result, "export "+varName+"=", "BuildEnvExports() missing variable %s", varName)
			}

			// Check that skipped variables are not present
			for varName := range tt.skipVars {
				assert.NotContains(t, result, "export "+varName+"=", "BuildEnvExports() should skip variable %s", varName)
			}

			// Verify it ends with "; " if there are exports
			if len(result) > 0 {
				assert.True(t, strings.HasSuffix(result, "; "), "BuildEnvExports() should end with '; ', got: %q", result[len(result)-10:])
			}

			// Test specific override case
			if tt.name == "override existing var" && tt.additional != nil {
				// Accept either quoted or unquoted (simple values don't need quotes)
				hasOverride := strings.Contains(result, "TEST_VAR1=overridden") || strings.Contains(result, "TEST_VAR1='overridden'")
				assert.True(t, hasOverride, "BuildEnvExports() should override TEST_VAR1 with 'overridden', got: %s", result)
			}
		})
	}
}

func TestBuildEnvExports_Quoting(t *testing.T) {
	tests := []struct {
		name     string
		envVars  map[string]string
		wantPart string
	}{
		{
			name:     "simple value",
			envVars:  map[string]string{"SIMPLE": "value"},
			wantPart: "export SIMPLE=value",
		},
		{
			name:     "value with spaces",
			envVars:  map[string]string{"SPACES": "value with spaces"},
			wantPart: "export SPACES='value with spaces'",
		},
		{
			name:     "value with quotes",
			envVars:  map[string]string{"QUOTES": "value'with'quotes"},
			wantPart: "export QUOTES='value'\\''with'\\''quotes'",
		},
		{
			name:     "value with special chars",
			envVars:  map[string]string{"SPECIAL": "value$with&special"},
			wantPart: "export SPECIAL='value$with&special'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := BuildEnvExports(tt.envVars)
			assert.Contains(t, result, tt.wantPart, "BuildEnvExports() = %q, want to contain %q", result, tt.wantPart)
		})
	}
}

func TestBuildEnvExports_EmptyAdditional(t *testing.T) {
	result := BuildEnvExports(map[string]string{})
	// Should still export current environment (minus skipped vars)
	assert.NotEmpty(t, result, "BuildEnvExports() with empty map should still export current environment")
}

func TestIsValidUUID(t *testing.T) {
	valid := []string{
		"2567b392-357b-4d6c-9a59-74fd23424cda",
		"00000000-0000-0000-0000-000000000000",
		"ABCDEF12-3456-7890-ABCD-EF1234567890", // upper-case hex accepted
	}
	for _, s := range valid {
		assert.Truef(t, IsValidUUID(s), "expected %q to be a valid UUID", s)
	}

	invalid := []string{
		"",
		"not-a-uuid",
		"2567b392357b4d6c9a5974fd23424cda",      // no hyphens
		"2567b392-357b-4d6c-9a59-74fd23424cd",   // too short
		"2567b392-357b-4d6c-9a59-74fd23424cda0", // too long
		"2567b392_357b_4d6c_9a59_74fd23424cda",  // wrong separators
		"gggggggg-357b-4d6c-9a59-74fd23424cda",  // non-hex
	}
	for _, s := range invalid {
		assert.Falsef(t, IsValidUUID(s), "expected %q to be rejected", s)
	}
}
