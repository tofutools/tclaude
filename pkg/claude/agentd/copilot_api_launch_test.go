package agentd

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/copilotapi"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ---------------------------------------------------------------------------
// Creating vs resuming
// ---------------------------------------------------------------------------

// The bug this pins is a data-loss bug, and it is worth being precise about how
// it got in. `session.create` was the correct and only call for as long as every
// launch the bootstrap saw was a fresh one — the drive was built before resume
// reached it. Nothing announced that the set of launches had widened; the code
// did not change, its callers did. So the assertion here is not "create is
// wrong" but "the two situations take different calls".
//
// Asserted as a TRANSITION rather than a level, deliberately: "create was not
// called" alone is satisfied by a bootstrap that made no call at all, which is
// also what a broken resume looks like from outside.
func TestCopilotAPIBootstrapResumesRatherThanRecreatingAResumedConversation(t *testing.T) {
	const convID = "conv-with-history"

	for _, tc := range []struct {
		name       string
		kind       copilotAPILaunchKind
		wantCalled string
		wantAbsent string
	}{
		{
			name: "a fresh launch creates", kind: copilotAPILaunchFresh,
			wantCalled: copilotapi.MethodSessionCreate, wantAbsent: copilotapi.MethodSessionResume,
		},
		{
			name: "a resumed launch resumes", kind: copilotAPILaunchResume,
			wantCalled: copilotapi.MethodSessionResume, wantAbsent: copilotapi.MethodSessionCreate,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newFakeCopilotServer(t)
			server.answer(copilotapi.MethodSessionCreate, `{"sessionId":"`+convID+`"}`)
			server.answer(copilotapi.MethodSessionResume, `{"sessionId":"`+convID+`"}`)
			client := dialFakeCopilot(t, server)

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			info, err := openCopilotAPISession(ctx, client, convID, tc.kind, "/work")
			require.NoError(t, err)
			assert.Equal(t, convID, info.SessionID)

			assert.Contains(t, server.methodsCalled(), tc.wantCalled)
			assert.NotContains(t, server.methodsCalled(), tc.wantAbsent,
				"a %s launch must not reach %s: create at an id with history starts it "+
					"FRESH, so the two calls are the difference between reloading the "+
					"conversation and replacing it", tc.kind, tc.wantAbsent)

			// The working directory is supplied on BOTH paths — an agent
			// relaunched elsewhere must resume against where it is now, not
			// where it used to be.
			var params copilotapi.ResumeSessionParams
			require.NoError(t, json.Unmarshal(server.paramsFor(tc.wantCalled), &params))
			assert.Equal(t, convID, params.SessionID)
			assert.Equal(t, "/work", params.WorkingDirectory)
		})
	}
}

// The failure arm, which is the one a fallback would be tempting on. A resume
// that cannot be reached must NOT recover by creating: that would convert "I
// could not reach the history" into "I replaced the history", silently, while
// reporting a healthy launch.
func TestCopilotAPIBootstrapRefusesToCreateWhenAResumeFails(t *testing.T) {
	server := newFakeCopilotServer(t)
	server.failMethod(copilotapi.MethodSessionResume, "Session not found: conv-gone")
	server.answer(copilotapi.MethodSessionCreate, `{"sessionId":"conv-gone"}`)
	client := dialFakeCopilot(t, server)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := openCopilotAPISession(ctx, client, "conv-gone", copilotAPILaunchResume, "/work")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Refusing to create a session at that id instead",
		"the failure must name what it refused to do, or the next reader will add it")
	assert.Contains(t, server.methodsCalled(), copilotapi.MethodSessionResume,
		"the resume must actually have been attempted — without this the assertion "+
			"below passes against a function that gave up before calling anything")
	assert.NotContains(t, server.methodsCalled(), copilotapi.MethodSessionCreate,
		"creating here would discard exactly the history the resume existed to keep")
}

// ---------------------------------------------------------------------------
// The launch kind reaches the bootstrap from the facade that knows it
// ---------------------------------------------------------------------------

// recordingSpawner accepts every launch and remembers nothing: these tests are
// about what the FACADE does after a successful hand-off, not about the fork.
type recordingSpawner struct{ resumed bool }

func (s *recordingSpawner) SpawnNew(clcommon.SpawnArgs) error    { return nil }
func (s *recordingSpawner) SpawnResume(clcommon.SpawnArgs) error { s.resumed = true; return nil }

// Which call the bootstrap makes is decided by a fact known at the TOP of the
// call stack — `session new` versus `session new -r` — and threaded down.
// Anything the bootstrap could infer for itself would be a proxy for that fact,
// and the cost of getting it wrong is not symmetric: a resume mistaken for a
// fresh launch destroys the conversation.
//
// So this asserts the thread, at the two facades that are the fresh/resume
// choice, rather than trusting the constant at each call site to be the right
// one by inspection.
func TestSpawnFacadesThreadTheLaunchKindToTheBootstrap(t *testing.T) {
	setupTestDB(t)

	previousSpawn := Spawn
	Spawn = &recordingSpawner{}
	t.Cleanup(func() { Spawn = previousSpawn })

	type kicked struct {
		convID string
		kind   copilotAPILaunchKind
	}
	var seen []kicked
	previousBootstrap := startCopilotAPIBootstrap
	startCopilotAPIBootstrap = func(
		convID string, copilotAPI bool, kind copilotAPILaunchKind, _ string,
	) {
		if copilotAPI {
			seen = append(seen, kicked{convID: convID, kind: kind})
		}
	}
	t.Cleanup(func() { startCopilotAPIBootstrap = previousBootstrap })

	require.NoError(t, SpawnDetachedTclaudeNew(clcommon.SpawnArgs{
		Harness: harness.CopilotName, CopilotAPI: true, SessionID: "conv-fresh", Cwd: "/work",
	}))
	require.NoError(t, SpawnDetachedTclaudeResume(clcommon.SpawnArgs{
		Harness: harness.CopilotName, CopilotAPI: true, ConvID: "conv-resumed", Cwd: "/work",
	}))

	assert.Equal(t, []kicked{
		{convID: "conv-fresh", kind: copilotAPILaunchFresh},
		{convID: "conv-resumed", kind: copilotAPILaunchResume},
	}, seen, "the facade IS the fresh/resume choice; the bootstrap must be told, not guess")
}

// ---------------------------------------------------------------------------
// The durable posture, for every conv id a launch mints
// ---------------------------------------------------------------------------

// Since TCL-1058 this record decides whether a message to the conversation goes
// over RPC or gets TYPED into its pane, so an unrecorded posture is not a
// missing badge — it is a connected agent routed back onto keystrokes. Recorded
// even when the drive is OFF, because "known: send-keys" and "nothing recorded"
// are different facts and only the first may be acted on.
func TestCopilotAPILaunchRecordsTheDriveForANewlyMintedConv(t *testing.T) {
	for _, tc := range []struct {
		name string
		api  bool
	}{{"the drive is on", true}, {"the drive is off", false}} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			const convID = "conv-just-minted"

			// Driven through the whole launch-completion seam rather than the
			// posture writer alone, so this covers the wiring as well as the
			// write. No session row, no agent, no conversation profile: the
			// state a freshly minted conv id is actually in when its launch
			// completes, and the whole reason the daemon cannot wait for the
			// launched process to record it.
			completeCopilotAPILaunch(convID, copilotAPILaunchFresh, clcommon.SpawnArgs{
				Harness: harness.CopilotName, CopilotAPI: tc.api, Cwd: "/work",
			})

			profile, err := db.ConversationResumeProfileForConv(convID)
			require.NoError(t, err)
			require.NotNil(t, profile, "a conv id with no profile yet must be seeded, not skipped")
			require.NotNil(t, profile.FallbackRelaunch)
			require.NotNil(t, profile.FallbackRelaunch.CopilotAPI,
				"a missing record is indistinguishable from 'this agent chose keystrokes', "+
					"which is why false is recorded rather than omitted")
			assert.Equal(t, tc.api, *profile.FallbackRelaunch.CopilotAPI)
			assert.Equal(t, harness.CopilotName, profile.Harness)

			// And the routing predicate agrees, with no handle anywhere — which
			// is the state every API-driven agent is in during the bootstrap
			// window and after an agentd restart.
			assert.Equal(t, tc.api, copilotLaunchIntentForConv(convID).API)
		})
	}
}

// A non-Copilot launch has no drive to record, and writing one would put a
// Copilot-shaped fact on a conversation that can never have it.
func TestCopilotAPILaunchRecordsNothingForAnotherHarness(t *testing.T) {
	setupTestDB(t)
	const convID = "conv-claude"

	completeCopilotAPILaunch(convID, copilotAPILaunchFresh, clcommon.SpawnArgs{
		Harness: harness.DefaultName, Cwd: "/work",
	})

	profile, err := db.ConversationResumeProfileForConv(convID)
	require.NoError(t, err)
	assert.Nil(t, profile, "a Claude Code launch must not mint a Copilot posture record")
}

// ---------------------------------------------------------------------------
// The pairing, asserted structurally
// ---------------------------------------------------------------------------

// copilotAPILaunchRecorders are the per-launch facts that must all be written at
// the one moment a launch and its conv id are both in hand.
//
// They are listed rather than remembered because that is how the posture write
// went missing in the first place. The port record and the bootstrap were paired
// by hand at four sites — with a comment at one of them explaining that pairing
// them was the point — and when the durable posture arrived later, and mattered
// more, it was added at none of them. Anything that can be forgotten at four
// sites will be forgotten at the fifth.
// The bootstrap entry points are listed too, and not only the indirection
// variable. Naming just `startCopilotAPIBootstrap` would leave a new site free
// to call `runCopilotAPIBootstrap` or `bootstrapCopilotAPISessionFn` directly
// and bypass the pairing entirely — a guard with a way around it watches
// nothing in particular. Those two are additionally allowed inside the
// bootstrap's own file, where the variable is defined in terms of them.
var copilotAPILaunchRecorders = map[string][]string{
	"recordCopilotAPIPosture":      {copilotAPILaunchSeam},
	"recordCopilotAPIPort":         {copilotAPILaunchSeam},
	"startCopilotAPIBootstrap":     {copilotAPILaunchSeam, copilotAPIBootstrapFile},
	"runCopilotAPIBootstrap":       {copilotAPILaunchSeam, copilotAPIBootstrapFile},
	"bootstrapCopilotAPISessionFn": {copilotAPILaunchSeam, copilotAPIBootstrapFile},
}

// copilotAPILaunchSeam is the only non-test file allowed to call the per-launch
// recorders; copilotAPIBootstrapFile additionally owns the bootstrap's own
// indirection.
const (
	copilotAPILaunchSeam    = "copilot_api_launch.go"
	copilotAPIBootstrapFile = "copilot_api_bootstrap.go"
)

// copilotAPILaunchSeamOnly are the recorders that must be reachable from the
// seam and from nowhere else — the positive control's subject.
var copilotAPILaunchSeamOnly = []string{
	"recordCopilotAPIPosture",
	"recordCopilotAPIPort",
	"startCopilotAPIBootstrap",
}

// TestCopilotLaunchesRecordPortAndPostureTogether makes the pairing structural.
//
// A behavioural test can only cover the launch paths someone thought of, and
// the failure guarded against here is a launch path nobody thought of: one that
// records a port, or starts a channel, for a conversation whose drive was never
// written down. That conversation reads as send-keys forever after, which is
// silent, looks healthy, and re-opens the injection sink TCL-1058 closed.
func TestCopilotLaunchesRecordPortAndPostureTogether(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	callers := map[string][]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		require.NoError(t, err)
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, name, source, 0)
		require.NoError(t, err)

		ast.Inspect(parsed, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			if _, watched := copilotAPILaunchRecorders[ident.Name]; !watched {
				return true
			}
			if !slices.Contains(callers[ident.Name], name) {
				callers[ident.Name] = append(callers[ident.Name], name)
			}
			return true
		})
	}

	// The positive control, and it is doing real work rather than decorating
	// the test: without it every assertion below is satisfied by a seam that
	// calls none of these — which is exactly what a rename, or a refactor that
	// moved the pairing elsewhere, would produce.
	for _, recorder := range copilotAPILaunchSeamOnly {
		assert.Contains(t, callers[recorder], copilotAPILaunchSeam,
			"%s is no longer called from %s. Either it moved, in which case this guard "+
				"is now watching the wrong file, or the pairing has been taken apart",
			recorder, copilotAPILaunchSeam)
	}

	for recorder, files := range callers {
		allowed := copilotAPILaunchRecorders[recorder]
		for _, file := range files {
			assert.Containsf(t, allowed, file,
				"%s is called from %s, which is not one of %v. Every per-launch record has "+
					"to be written at the ONE moment a launch and its conversation id are "+
					"both in hand, and completeCopilotAPILaunch in %s is that moment. A site "+
					"that records a port or starts a channel without also recording the "+
					"drive leaves a conversation that routes its messages as keystrokes for "+
					"the rest of its life. Call completeCopilotAPILaunch instead",
				recorder, file, allowed, copilotAPILaunchSeam)
		}
	}
}
