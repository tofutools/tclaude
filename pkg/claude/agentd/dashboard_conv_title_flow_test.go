package agentd_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// Scenario (issue #91): a plain conversation — a non-agent, never
// /rename'd — has a raw first prompt that, like every real Claude Code
// .jsonl, carries an inline <system-reminder> block the user never
// typed. The dashboard's conversations[] list must render its title
// through the same convindex.FormatConvTitle the CLI's `conv ls` uses:
// system tag AND its content stripped. Before the fix the dashboard
// echoed the raw first prompt verbatim, leaking the injected noise.
func TestDashboardSnapshot_PlainConvTitleMatchesConvLs(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const plainConv = "plan-1111-2222-3333-444444444444"
	f.HaveConvWithPrompt(plainConv,
		"fix the login crash<system-reminder>injected noise the user never typed</system-reminder>")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	var got string
	found := false
	for _, c := range snap.Conversations {
		if c.ConvID == plainConv {
			got, found = c.Title, true
		}
	}
	require.True(t, found, "plain conv should surface in conversations[]")
	assert.NotContains(t, got, "<system-reminder>",
		"the raw system tag must be stripped — same as conv ls")
	assert.NotContains(t, got, "injected noise",
		"system-tag CONTENT must be stripped too, not just the tag")
	assert.Equal(t, "fix the login crash", got,
		"plain-conv title must be the cleaned first prompt, conv ls parity")
}

// Scenario: the fix is scoped to plain conversations only. An agent's
// title (its custom name, set via /rename) must keep displaying bare —
// NOT wrapped as "[name]: prompt" — because agent titles already render
// the way intended. This pins that the issue-#91 fix did not bleed into
// the agent rows on the Agents tab.
func TestDashboardSnapshot_AgentTitleStaysBareName(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)

	const agentConv = "agnt-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(agentConv, "backend-worker")
	f.HaveEnrolledAgent(agentConv)

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	var got string
	found := false
	for _, a := range snap.Agents {
		if a.ConvID == agentConv {
			got, found = a.Title, true
		}
	}
	require.True(t, found, "enrolled agent should surface in agents[]")
	assert.Equal(t, "backend-worker", got,
		"agent title must stay the bare custom name")
	assert.False(t, strings.HasPrefix(got, "["),
		"agent title must NOT be wrapped in the conv-ls [..] form")
}
