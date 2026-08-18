package session

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// The redactor's whole job is that no environment VALUE reaches the log while
// the command's shape still does. Over-redaction is acceptable; a single leaked
// value is not, so the cases below lean on the awkward shapes ShellQuoteArg
// actually emits rather than on tidy ones.
func TestRedactPaneCommand(t *testing.T) {
	for _, tc := range []struct {
		name     string
		command  string
		wantGone []string
		wantKept []string
	}{
		{
			name:     "plain exports keep their names and lose their values",
			command:  "export ANTHROPIC_API_KEY=sk-secret-value; export PATH=/usr/bin; claude --model opus",
			wantGone: []string{"sk-secret-value", "/usr/bin"},
			wantKept: []string{"ANTHROPIC_API_KEY", "PATH", "claude --model opus"},
		},
		{
			name:     "a quoted value with spaces does not leak its tail",
			command:  "export TOKEN='sk secret with spaces'; claude",
			wantGone: []string{"sk secret with spaces", "secret"},
			wantKept: []string{"TOKEN", "claude"},
		},
		{
			name: "an embedded apostrophe does not end the value early",
			// ShellQuoteArg renders a quote as the four-byte '\'' sequence;
			// stopping at the first quote would spill the rest into the log.
			command:  `export TOKEN='sk-a'\''b-secret'; claude --resume x`,
			wantGone: []string{"secret", "sk-a"},
			wantKept: []string{"TOKEN", "claude --resume x"},
		},
		{
			name:     "an export inside the sandbox wrapper is still redacted",
			command:  "tclaude session relay -- bwrap --ro-bind / / -- sh -c 'export KEY=deep-secret; claude'",
			wantGone: []string{"deep-secret"},
			wantKept: []string{"bwrap", "--ro-bind", "KEY"},
		},
		{
			name:     "a bare export word is not mistaken for an assignment",
			command:  "echo export; claude",
			wantGone: nil,
			wantKept: []string{"echo export; claude"},
		},
		{
			name:     "an unterminated quote drops the remainder rather than leaking it",
			command:  "export TOKEN='never-closed-secret",
			wantGone: []string{"never-closed-secret"},
			wantKept: []string{"TOKEN"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactPaneCommand(tc.command)
			for _, gone := range tc.wantGone {
				assert.NotContains(t, got, gone, "a value must never survive redaction")
			}
			for _, kept := range tc.wantKept {
				assert.Contains(t, got, kept, "the command's shape must survive redaction")
			}
		})
	}
}

// The redactor is only trustworthy against the exact prefix production builds,
// so build one the same way a launch does and prove nothing survives.
func TestRedactPaneCommandAgainstRealEnvExports(t *testing.T) {
	t.Setenv("TCLAUDE_TEST_FAKE_SECRET", "sk-do-not-log-me")
	t.Setenv("TCLAUDE_TEST_AWKWARD", "va'lue with 'quotes' and ; semicolons")

	prefix := clcommon.BuildEnvExports(map[string]string{
		"TCLAUDE_TEST_ADDITIONAL": "another-secret-value",
	})
	require.Contains(t, prefix, "sk-do-not-log-me", "the fixture must actually carry the secret")

	got := RedactPaneCommand(prefix + "claude --model opus")

	assert.NotContains(t, got, "sk-do-not-log-me")
	assert.NotContains(t, got, "another-secret-value")
	assert.NotContains(t, got, "quotes")
	assert.Contains(t, got, "TCLAUDE_TEST_FAKE_SECRET")
	assert.Contains(t, got, "claude --model opus",
		"the command after the env prefix must survive intact")

	// Nothing from the REAL inherited environment may survive either — that is
	// the bulk of what the prefix carries, and the part no fixture enumerates.
	for _, kv := range os.Environ() {
		name, value, ok := strings.Cut(kv, "=")
		// Short values collide with ordinary command text ("0", "1", "en_US"),
		// so they say nothing either way; anything long enough to be a
		// credential must be absent.
		if !ok || len(value) <= 12 {
			continue
		}
		assert.NotContainsf(t, got, value,
			"the value of %s survived redaction", name)
	}
}
