package conv

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const resumeConvGeminiSeatbelt = "8e5a0f43-4444-4555-8666-777788889999"

// Gemini's own Seatbelt mode hides its OAuth credentials and trust stores, so
// a resume under it refuses an auth type that cannot work inside, as a spawn
// does.
func TestResumeLaunchCmd_GeminiSeatbeltRunsTheSpawnSeatbeltChecks(t *testing.T) {
	setupTestDB(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(harness.GeminiHomeEnvVar, "")
	// Keep a host-wide Gemini system settings file out of the auth decision.
	t.Setenv("GEMINI_CLI_SYSTEM_SETTINGS_PATH", filepath.Join(t.TempDir(), "absent-settings.json"))
	t.Setenv("GEMINI_CLI_SYSTEM_DEFAULTS_PATH", filepath.Join(t.TempDir(), "absent-defaults.json"))
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "resume-gemini-seatbelt", ConvID: resumeConvGeminiSeatbelt,
		Harness: harness.GeminiName, HarnessBuiltinMode: harness.GeminiSandboxSeatbelt,
	}))

	_, _, _, err := resumeLaunchCmd(harness.GeminiName, resumeConvGeminiSeatbelt[:8], resumeConvGeminiSeatbelt, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs a selected auth type")
}
