package copilotfixture_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// The real-binary evidence behind tclaude's Copilot usage/context follower.
//
// TCL-980 needs a different artifact from the scenario goldens next door. Those
// record what the CLI's stdout event STREAM looked like; a follower reads the
// on-disk events.jsonl, and the two are not the same set. Copilot's shipped
// session-events.schema.json marks 60-odd event types `ephemeral: true` —
// "not persisted to the session event log on disk" — and three of them are
// exactly the ones a usage follower would want:
//
//	assistant.usage      per-call input/output/cache/reasoning tokens and cost
//	session.usage_info   live context window (currentTokens + tokenLimit)
//	model.call_start     per-call model/provider
//
// This test is what turns that schema claim into evidence. It runs the pinned
// binary, records the sanitized log, and asserts BOTH directions: the durable
// fields the follower projects are present, and the ephemeral events it must
// never wait for are absent. If a future CLI starts persisting per-call usage,
// this fails and the follower gets to grow a live token meter; if it stops
// persisting the shutdown accounting, this fails before the dashboard silently
// empties in the field.

const eventLogFixtureName = "session_events.jsonl"

func eventLogFixturePath() string {
	return filepath.Join("testdata", copilotfixture.PinnedCLIVersion, eventLogFixtureName)
}

// TestCopilotSessionEventLogFixture records and pins the durable event log of
// a fresh turn, a tool-calling turn and a resume against the same session.
func TestCopilotSessionEventLogFixture(t *testing.T) {
	requireSmokeParallel(t)

	const sessionID = "9a1c2d3e-4f50-4617-8829-0b1c2d3e4f50"

	mock := copilotfixture.NewMockProvider(t, []copilotfixture.Turn{
		{Text: "MOCK FIRST TURN"},
		{ToolCall: &copilotfixture.ToolCall{
			ID:   "call_copilotfixture_1",
			Name: "bash",
			Args: `{"command":"echo copilotfixture-tool-ran","description":"fixture probe"}`,
		}},
		{Text: "MOCK TOOL FOLLOW UP"},
	})
	dirs := copilotfixture.NewSandboxDirs(t)

	fresh := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(), Model: copilotfixture.MockModel,
		SessionID: sessionID, Prompt: "first prompt about widgets",
	})
	require.Equal(t, 0, fresh.ExitCode, "stderr: %s", fresh.Stderr)

	// The resume is the case the follower most needs pinned: Copilot APPENDS
	// to the same file rather than starting a new one, so a byte cursor has to
	// survive a second CLI lifetime writing into it.
	resumed := copilotfixture.Run(t, copilotfixture.RunOptions{
		Root: dirs.Root, Home: dirs.Home, Cache: dirs.Cache, WorkDir: dirs.WorkDir,
		BaseURL: mock.BaseURL(), Model: copilotfixture.MockModel,
		ResumeID: sessionID, Prompt: "use the bash tool as instructed",
	})
	require.Equal(t, 0, resumed.ExitCode, "stderr: %s", resumed.Stderr)

	sanitizer := newSanitizer(dirs)
	path := eventLogFixturePath()
	if *update {
		require.NoError(t, copilotfixture.WriteEventLogFixture(sanitizer, dirs.Home, sessionID, path))
		t.Logf("re-recorded %s", path)
	}

	recorded, err := os.ReadFile(path)
	require.NoError(t, err,
		"missing fixture %s; re-record with `go test -run %s -update`", path, t.Name())
	assertEventLogClean(t, recorded, dirs)

	types := copilotfixture.SortedUnique(copilotfixture.EventTypesIn(recorded))

	// The durable surface the follower projects. Each of these is a field the
	// harness scan state reads; losing one is a silent capability regression.
	for _, required := range []string{
		"session.start",
		"session.resume",
		"session.model_change",
		"user.message",
		"assistant.turn_start",
		"assistant.message",
		"assistant.turn_end",
		"session.shutdown",
		"tool.execution_start",
		"tool.execution_complete",
	} {
		assert.Contains(t, types, required,
			"Copilot stopped persisting %s; the follower's projection of it goes blank", required)
	}

	// The ephemeral surface. Asserting absence is what stops a future change
	// from building a live token meter on an event that never lands on disk.
	for _, ephemeral := range []string{
		"assistant.usage",
		"session.usage_info",
		"model.call_start",
		"assistant.message_delta",
		"session.idle",
	} {
		assert.NotContains(t, types, ephemeral,
			"Copilot now persists %s; TCL-980's ephemeral-usage limitation may be liftable", ephemeral)
	}

	// The stream, by contrast, DOES carry the ephemeral events. Pinning that
	// keeps the "ephemeral means stream-only, not nonexistent" distinction
	// honest: these types are reachable by a stdout consumer and unreachable
	// by a file follower, which is the whole reason TCL-980 stops where it
	// does.
	//
	// model.call_start rather than assistant.usage, because the mock provider
	// returns no `copilot_usage` block and so gives the CLI nothing to bill —
	// asserting on assistant.usage would be asserting on the mock, not on
	// Copilot.
	assert.Contains(t, resumed.EventTypes(), "model.call_start",
		"an ephemeral event must still be observable on the JSONL stream")
}

// assertEventLogClean is the committed-content gate. It runs before the
// fixture is trusted, and — because -update writes first — a leak that slipped
// through fails the same run that recorded it.
func assertEventLogClean(t *testing.T, encoded []byte, dirs copilotfixture.Dirs) {
	t.Helper()
	body := string(encoded)
	for _, forbidden := range []string{dirs.Root, dirs.Home, dirs.Cache, dirs.WorkDir} {
		require.NotContains(t, body, forbidden, "event-log fixture leaked a private path")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
		require.NotContains(t, body, home, "event-log fixture leaked the operator's home directory")
	}
	require.False(t, strings.Contains(body, "<environment_context>"),
		"event-log fixture captured host-probed prompt scaffolding")
	// The system prompt alone is ~26 kB per lifetime. A fixture past this size
	// means bulk content stopped being reduced to a digest.
	require.Less(t, len(encoded), 32*1024,
		"event-log fixture grew past the size that indicates raw prompt/tool content crept in")
}
