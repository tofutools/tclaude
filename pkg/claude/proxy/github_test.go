package proxy

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// github_test.go covers the CLI half of the GitHub proxy. The interesting
// behaviour here is rendering: gh's JSON is passed through UNMODELLED, so the
// one thing this side must not do is change it on the way past.

// TestGHProxyOutcome_RenderPreservesTheBytesGitHubSent is the regression that
// an unmarshal-into-`any` round-trip would break in two separate ways: JSON
// numbers become float64 (so anything past 2^53 comes back a different number),
// and objects become maps (so Marshal re-orders the keys alphabetically).
func TestGHProxyOutcome_RenderPreservesTheBytesGitHubSent(t *testing.T) {
	// 9007199254740993 is 2^53+1 — the smallest integer float64 cannot hold.
	const raw = `[{"number":7,"databaseId":9007199254740993,"title":"a pr","author":{"login":"someone"}}]`
	o := &ghProxyOutcome{JSON: json.RawMessage(raw)}

	var stdout, stderr bytes.Buffer
	require.Equal(t, rcOK, o.render(&stdout, &stderr, "pr ls"), "stderr=%s", stderr.String())

	out := stdout.String()
	assert.Contains(t, out, "9007199254740993",
		"a float64 round-trip would render this as 9007199254740992")
	// Field order is GitHub's, not Go's map ordering — "number" is written
	// before "databaseId" exactly as it arrived.
	assert.Less(t, indexOf(out, `"number"`), indexOf(out, `"databaseId"`),
		"re-marshalling a map would sort the keys alphabetically")
	assert.JSONEq(t, raw, out, "and it is still the same document")
	assert.Contains(t, out, "\n  ", "indented for a human reading the result")
}

// TestGHProxyOutcome_RenderFallsBackToRawOnUnparseableJSON — the daemon may
// have truncated the tail of a large response, which leaves invalid JSON. Show
// it rather than swallowing it; a truncated answer the agent can read beats an
// empty one.
func TestGHProxyOutcome_RenderFallsBackToRawOnUnparseableJSON(t *testing.T) {
	o := &ghProxyOutcome{JSON: json.RawMessage(`[{"number":7,"title":"a p`)}

	var stdout, stderr bytes.Buffer
	require.Equal(t, rcOK, o.render(&stdout, &stderr, "pr ls"))
	assert.Contains(t, stdout.String(), `"number":7`)
}

// TestGHProxyOutcome_RenderPassesTextPayloadsThrough — `pr comments` and
// `run log-failed` are the two verbs whose payload is prose rather than JSON.
// Indentation and column alignment inside a log line are load-bearing, so the
// renderer must not reflow them, and a tail-truncated answer must say so —
// otherwise an agent reads half a log as the whole failure.
//
// Trailing newlines are the one thing render does normalise (TrimRight then
// Fprintln), which is why the fixture's interior whitespace is what is
// asserted rather than the byte-for-byte string.
func TestGHProxyOutcome_RenderPassesTextPayloadsThrough(t *testing.T) {
	const log = "=== inline review comments ===\n\nfile:\tmain.go:42\n--\n    indented body\n\n\n"
	o := &ghProxyOutcome{Stdout: log, Truncated: true}

	var stdout, stderr bytes.Buffer
	require.Equal(t, rcOK, o.render(&stdout, &stderr, "pr comments"))
	got := stdout.String()
	assert.Contains(t, got, "file:\tmain.go:42", "tabs inside a line are not reflowed")
	assert.Contains(t, got, "\n    indented body", "leading indentation survives")
	assert.Contains(t, got, "=== inline review comments ===\n\nfile:",
		"blank lines BETWEEN sections are structure, not padding")
	assert.Contains(t, stderr.String(), "truncated",
		"a half answer that does not say so reads as a whole one")
}

func indexOf(s, sub string) int {
	return bytes.Index([]byte(s), []byte(sub))
}
