package copilotfixture_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// ClassifyPermission is the one piece of this suite whose correctness cannot be
// established by the scenarios that use it.
//
// Every real scenario feeds it ONE combination of inputs, from a launch that
// takes seconds and needs the pinned CLI. So the arms nothing currently
// exercises — and, more importantly, the ORDER in which they are tried — have
// no coverage at all from that direction. That order is not incidental: a
// denial and an execution produce the identical coarse observable (a second
// provider request), and the only thing separating them is that the denial arm
// is checked first. Swap the two and every silent denial in the suite would be
// recorded as the tool having run, which is the worst available direction to
// be wrong in for a ticket about what a detached agent may do.
//
// These tests are pure Go and deliberately UNGATED: they need no binary, so
// they run in plain `go test ./...` where everyone sees them, rather than only
// in the CLI-provisioned job.

func TestClassifyPermissionArms(t *testing.T) {
	const trustTranscript = "…\n" + copilotfixture.TrustPromptMarker + "\nDo you trust…"

	for _, tc := range []struct {
		name             string
		total, followUps int
		stillAlive       bool
		quiesced         bool
		transcript       string
		toolResults      []string
		want             copilotfixture.PermissionOutcome
		wantErr          bool
	}{
		{
			// The plain success: a tool ran and said something about it.
			name: "executed", total: 2, followUps: 1, stillAlive: true, quiesced: true,
			toolResults: []string{"copilotfixture-tool-ran\n"},
			want:        copilotfixture.PermissionAllowed,
		},
		{
			// THE ORDERING TEST. Identical to the row above in every input the
			// Allowed arm looks at — same counts, same liveness — and differing
			// only in what the tool result SAYS. If the arms are ever reordered
			// so Allowed is tried first, this is the assertion that fails.
			name: "denied-beats-allowed/deny-rule", total: 2, followUps: 1,
			stillAlive: true, quiesced: true,
			toolResults: []string{
				"Permission to run this tool was denied due to the following rules: `shell(echo)`"},
			want: copilotfixture.PermissionDenied,
		},
		{
			// The headless path fallback, which is a denial rather than a block.
			name: "denied-beats-allowed/path", total: 2, followUps: 1,
			stillAlive: false, quiesced: true,
			toolResults: []string{"Permission denied and could not request permission from user"},
			want:        copilotfixture.PermissionDenied,
		},
		{
			name: "denied-beats-allowed/url", total: 2, followUps: 1,
			stillAlive: true, quiesced: true,
			toolResults: []string{"Permission to access this URL was denied."},
			want:        copilotfixture.PermissionDenied,
		},
		{
			// A denial mixed in among ordinary results still wins: one refused
			// call is the finding, whatever else the turn managed to do.
			name: "denied-among-several-results", total: 3, followUps: 2,
			stillAlive: true, quiesced: true,
			toolResults: []string{"ordinary output", "Permission denied"},
			want:        copilotfixture.PermissionDenied,
		},
		{
			// The arm this test exists to add: a follow-up request that carries
			// NO tool result proves nothing about execution, so it must be
			// undecidable rather than quietly counted as a success.
			name: "follow-up-without-a-tool-result", total: 2, followUps: 1,
			stillAlive: true, quiesced: true,
			wantErr: true,
		},
		{
			name: "trust-gate", total: 0, followUps: 0, stillAlive: true, quiesced: true,
			transcript: trustTranscript,
			want:       copilotfixture.PermissionBlocked,
		},
		{
			// Reached the provider, then parked on a prompt.
			name: "blocked-on-a-prompt", total: 1, followUps: 0,
			stillAlive: true, quiesced: true,
			want: copilotfixture.PermissionBlocked,
		},
		{
			// Died on its own: a rejected flag, a refused startup.
			name: "exited-without-running-the-tool", total: 1, followUps: 0,
			stillAlive: false, quiesced: true,
			want: copilotfixture.PermissionRefused,
		},
		{
			// Alive but never settled: most likely still working, so the
			// deadline was too short. Guessing "blocked" here is precisely the
			// guess this ticket cannot afford.
			name: "alive-but-still-producing-output", total: 1, followUps: 0,
			stillAlive: true, quiesced: false,
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verdict, err := copilotfixture.ClassifyPermission(
				tc.total, tc.followUps, tc.stillAlive, tc.quiesced,
				tc.transcript, tc.toolResults)
			if tc.wantErr {
				require.Error(t, err,
					"this combination establishes nothing and must be an error, not an arm")
				assert.Empty(t, verdict.Outcome)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, verdict.Outcome)
			assert.NotEmpty(t, verdict.Evidence, "every verdict must carry its evidence")
		})
	}
}

// TestClassifyPermissionDenialWinsForEveryMarker pins the ordering property
// against the whole marker set rather than the three spellings sampled above,
// so adding a marker cannot quietly leave a denial classifiable as execution.
func TestClassifyPermissionDenialWinsForEveryMarker(t *testing.T) {
	for _, marker := range []string{
		"Permission denied and could not request permission from user",
		"Permission to run this tool was denied",
		"Permission to access this URL was denied",
		"Permission denied",
	} {
		t.Run(marker, func(t *testing.T) {
			require.NotEmpty(t, copilotfixture.DenialMarker([]string{"prefix " + marker + " suffix"}),
				"the marker must be recognized when embedded in a larger tool result")
			verdict, err := copilotfixture.ClassifyPermission(
				2, 1, true, true, "", []string{"prefix " + marker + " suffix"})
			require.NoError(t, err)
			assert.Equal(t, copilotfixture.PermissionDenied, verdict.Outcome)
		})
	}
}

// TestDenialMarkerIgnoresOrdinaryOutput guards the other direction: a marker
// set that matched too eagerly would report working launches as denied, which
// would be just as wrong and much harder to notice, since a "denied" verdict
// reads as a legitimate finding rather than as a bug.
func TestDenialMarkerIgnoresOrdinaryOutput(t *testing.T) {
	for _, result := range []string{
		"",
		"copilotfixture-tool-ran\n",
		// Deliberately adjacent wording: a tool whose own OUTPUT talks about
		// permissions must not be read as the CLI refusing the call.
		"-rw-r--r-- 1 user user 0 Jan 1 00:00 file.txt\n",
		"chmod: changing permissions of 'x': Operation not permitted\n",
		"Permission is hereby granted, free of charge, to any person obtaining a copy\n",
	} {
		t.Run(result, func(t *testing.T) {
			assert.Empty(t, copilotfixture.DenialMarker([]string{result}))
		})
	}
}

// TestToolResultsReadsCompletionsOnly pins the wire scope, so the function
// cannot regrow an unmeasured Responses-wire fallback without this failing.
func TestToolResultsReadsCompletionsOnly(t *testing.T) {
	completions := copilotfixture.RecordedRequest{Body: map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "…"},
			map[string]any{"role": "user", "content": "…"},
			map[string]any{"role": "assistant", "content": nil},
			map[string]any{"role": "tool", "content": "copilotfixture-tool-ran"},
		},
	}}
	assert.Equal(t, []string{"copilotfixture-tool-ran"},
		copilotfixture.ToolResults(completions))

	// The Responses wire's shape has never been observed by this suite, so it
	// must read as "no tool result" — which lands in ClassifyPermission's
	// undecidable arm — rather than as a guess that happens to look right.
	responses := copilotfixture.RecordedRequest{Body: map[string]any{
		"instructions": "…",
		"input": []any{
			map[string]any{"role": "user", "content": "…"},
			map[string]any{"role": "tool", "content": "copilotfixture-tool-ran"},
		},
	}}
	assert.Empty(t, copilotfixture.ToolResults(responses),
		"the Responses wire is not measured; reading it here would claim support "+
			"this suite has no fixture for")

	assert.Empty(t, copilotfixture.ToolResults(copilotfixture.RecordedRequest{}))
}
