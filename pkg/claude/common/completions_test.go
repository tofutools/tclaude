package common

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

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
