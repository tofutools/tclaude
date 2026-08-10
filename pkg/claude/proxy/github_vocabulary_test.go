package proxy_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/proxy"
)

// github_vocabulary_test.go ties the CLI's copy of gh's `run list --status`
// vocabulary to the daemon's.
//
// The CLI deliberately does not import the daemon — the gate has to live where
// it cannot be skipped — so the completion list is a copy, and a copy drifts.
// The drift is asymmetric and only one direction is harmless: a stale entry in
// the CLI costs one refusal from the daemon, but a status ADDED to the gate and
// not here silently stops being offered, which reads to an agent as "that
// filter is not supported" rather than "completion is out of date".
//
// This test lives in an external test package so it may import both.
func TestGHRunStatusCompletionMatchesTheGate(t *testing.T) {
	assert.ElementsMatch(t, agentd.GHRunStatusesForTest(), proxy.GHRunStatusAlternativesForTest(),
		"the CLI's completion vocabulary and the daemon's allow-list have drifted apart")
}

// TestGHMergeMethodCompletionMatchesTheGate is the same pin for `pr merge
// --method`. Order matters here as it does not for statuses: an empty method
// resolves to the gate's FIRST entry, so the two lists agreeing as sets is not
// enough — completion offering "squash" first while the daemon defaults to
// "merge" would read as a default that is not one.
func TestGHMergeMethodCompletionMatchesTheGate(t *testing.T) {
	assert.Equal(t, agentd.GHMergeMethodsForTest(), proxy.GHMergeMethodAlternativesForTest(),
		"the CLI's completion vocabulary and the daemon's allow-list have drifted apart")
}
