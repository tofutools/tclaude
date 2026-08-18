package session

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// nestOnce re-encodes a string the way ShellQuoteArg does when the command
// carrying it is embedded as a single-quoted argument — one wrapper layer.
func nestOnce(s string) string { return strings.ReplaceAll(s, "'", `'\''`) }

// The redactor's whole job is that no environment VALUE reaches the log. These
// cases are built from a REAL BuildEnvExports prefix, because the bug this
// replaces passed every hand-written fixture and still leaked in production:
// the assembled command is quoted twice, so an inner value opens with '\''
// rather than a bare quote.
func TestRedactPaneCommandRemovesTheEnvironmentAtEveryNestingDepth(t *testing.T) {
	t.Setenv("TCLAUDE_TEST_FAKE_SECRET", "sk-do-not-log-me")
	// An awkward value with the quote, semicolon and colon soup that LS_COLORS
	// has in reality — the shape the previous implementation broke on.
	t.Setenv("TCLAUDE_TEST_AWKWARD", "rs=0:di=01;34:it's='quoted';x")

	envExports := clcommon.BuildEnvExports(map[string]string{
		"TCLAUDE_TEST_ADDITIONAL": "another-secret-value",
	})
	require.Contains(t, envExports, "sk-do-not-log-me", "the fixture must carry the secret")

	const tail = "claude --resume abc --model 'opus'"
	for depth, prefix := range []string{
		envExports,
		nestOnce(envExports),
		nestOnce(nestOnce(envExports)),
		nestOnce(nestOnce(nestOnce(envExports))),
	} {
		command := "tclaude session resource-limit-exec --command '" + prefix + tail + "'"

		got, ok := RedactPaneCommand(command, envExports)

		require.Truef(t, ok, "depth %d: the prefix must be locatable", depth)
		assert.NotContainsf(t, got, "sk-do-not-log-me", "depth %d: a value survived", depth)
		assert.NotContainsf(t, got, "another-secret-value", "depth %d: a value survived", depth)
		assert.NotContainsf(t, got, "it'", "depth %d: an awkward value survived", depth)
		assert.Containsf(t, got, envRedactionPlaceholder, "depth %d", depth)
		assert.Containsf(t, got, "resource-limit-exec", "depth %d: shape must survive", depth)
		assert.Containsf(t, got, "claude --resume abc", "depth %d: shape must survive", depth)

		// The real guarantee: nothing from the live environment survives.
		for _, kv := range os.Environ() {
			name, value, cut := strings.Cut(kv, "=")
			// Short values collide with ordinary command text ("0", "en_US"),
			// so they prove nothing either way.
			if !cut || len(value) <= 12 {
				continue
			}
			assert.NotContainsf(t, got, value,
				"depth %d: the value of %s survived redaction", depth, name)
		}
	}
}

// Failing closed is the contract: a redactor that cannot prove it removed the
// environment must report failure so the caller writes no command at all.
func TestRedactPaneCommandFailsClosedWhenThePrefixIsNotFound(t *testing.T) {
	got, ok := RedactPaneCommand(
		"claude --resume abc", "export SECRET='never-appears-in-the-command'; ")

	assert.False(t, ok, "an unlocatable prefix must not be reported as redacted")
	assert.Empty(t, got, "no command may be returned when redaction could not be proved")
}

// A prefix restated by a wrapper must not be half-removed.
func TestRedactPaneCommandFailsClosedWhenAnOccurrenceWouldSurvive(t *testing.T) {
	const envExports = "export SECRET='sk-leaked'; "
	got, ok := RedactPaneCommand(envExports+"claude && "+envExports+"claude", envExports)

	assert.False(t, ok, "a surviving second occurrence must fail closed")
	assert.Empty(t, got)
}

// A launch that forwards no environment has nothing to remove, and its command
// must still be loggable.
func TestRedactPaneCommandPassesThroughWhenThereIsNoEnvironment(t *testing.T) {
	got, ok := RedactPaneCommand("claude --resume abc", "")

	require.True(t, ok)
	assert.Equal(t, "claude --resume abc", got)
}
