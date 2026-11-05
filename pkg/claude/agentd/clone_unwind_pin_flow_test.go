package agentd_test

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A failed clone must leave nothing behind, whichever launch branch it took
// and whether it failed at the launch or after it. These pin the end state
// cloneSpawnOnce's unwind produces: no actor or membership for the clone, no
// pre-launch handoff message, no session row, no in-flight launch claim, and
// (copy branch) no copied conversation on disk. The source conversation is
// never touched, and a clone that succeeds keeps its copy.

// cloneFailSpawner records each launch and fails it the way the scenario says.
type cloneFailSpawner struct {
	newFn    func(clcommon.SpawnArgs) error
	resumeFn func(clcommon.SpawnArgs) error
	news     []clcommon.SpawnArgs
	resumes  []clcommon.SpawnArgs
}

func (s *cloneFailSpawner) SpawnNew(a clcommon.SpawnArgs) error {
	s.news = append(s.news, a)
	return s.newFn(a)
}

func (s *cloneFailSpawner) SpawnResume(a clcommon.SpawnArgs) error {
	s.resumes = append(s.resumes, a)
	return s.resumeFn(a)
}

// launched is the one launch the clone attempted, and the conv it named.
func (s *cloneFailSpawner) launched(t *testing.T) (clcommon.SpawnArgs, string) {
	t.Helper()
	require.Equal(t, 1, len(s.news)+len(s.resumes), "exactly one launch was attempted")
	if len(s.news) == 1 {
		return s.news[0], s.news[0].SessionID
	}
	return s.resumes[0], s.resumes[0].ConvID
}

func TestCloneUnwind_AFailedCloneLeavesNothingBehind(t *testing.T) {
	refuse := func(clcommon.SpawnArgs) error { return errors.New("pinned launch refusal") }
	// The wrapper starts, then dies before its pane comes up.
	dieAfterLaunch := func(a clcommon.SpawnArgs) error {
		agentd.SignalSpawnWrapperFailureForTest(a.Label, errors.New("pinned wrapper death"))
		return nil
	}
	// The resume starts, but its session never registers.
	neverRegisters := func(clcommon.SpawnArgs) error { return nil }

	for _, tc := range []struct {
		name     string
		body     map[string]any
		spawner  *cloneFailSpawner
		status   int
		code     string
		errorHas string
	}{
		{"no-copy refused at launch", map[string]any{"no_copy_conv": true},
			&cloneFailSpawner{newFn: refuse, resumeFn: refuse},
			http.StatusInternalServerError, "spawn", "pinned launch refusal"},
		{"copy refused at launch", map[string]any{},
			&cloneFailSpawner{newFn: refuse, resumeFn: refuse},
			http.StatusInternalServerError, "spawn", "pinned launch refusal"},
		{"no-copy wrapper dies after launch", map[string]any{"no_copy_conv": true},
			&cloneFailSpawner{newFn: dieAfterLaunch, resumeFn: refuse},
			http.StatusInternalServerError, "spawn", "pinned wrapper death"},
		{"copy never comes online after launch", map[string]any{},
			&cloneFailSpawner{newFn: refuse, resumeFn: neverRegisters},
			http.StatusGatewayTimeout, "timeout", "the timed-out clone was stopped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The copy branch's online wait is a deadline poll; keep it short.
			t.Cleanup(agentd.SetReincarnateSpawnTimeoutForTest(500 * time.Millisecond))
			f := newFlow(t)
			f.HaveGroup("crew")
			spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker"})
			require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
			source := snapshotConversation(t, spawn.ConvID)
			agentsBefore, err := db.ListActiveAgents()
			require.NoError(t, err)
			sessionsBefore, err := db.ListSessions()
			require.NoError(t, err)
			prev := agentd.Spawn
			agentd.Spawn = tc.spawner
			t.Cleanup(func() { agentd.Spawn = prev })

			clone := f.AsHuman().CloneWith(spawn.ConvID, tc.body)

			assert.Equal(t, tc.status, clone.Code, "body=%s", clone.Raw)
			var body struct {
				Code  string `json:"code"`
				Error string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(clone.Raw, &body), "body=%s", clone.Raw)
			assert.Equal(t, tc.code, body.Code)
			assert.Contains(t, body.Error, tc.errorHas)

			args, conv := tc.spawner.launched(t)
			require.NotEmpty(t, conv, "the launch named the clone's conv")
			assert.Contains(t, args.InitialPrompt, "message #",
				"the clone's handoff was written before the launch, so its rollback is exercised")

			agentID, err := db.AgentIDForConv(conv)
			require.NoError(t, err)
			assert.Empty(t, agentID, "no actor is left for the failed clone")
			groups, err := db.ListGroupsForConv(conv)
			require.NoError(t, err)
			assert.Empty(t, groups, "the failed clone is in no group")
			msgs, err := db.ListAgentMessagesForConv(conv, 50)
			require.NoError(t, err)
			assert.Empty(t, msgs, "the pre-launch handoff message is rolled back")
			agentsAfter, err := db.ListActiveAgents()
			require.NoError(t, err)
			assert.Len(t, agentsAfter, len(agentsBefore), "no active actor is added")
			sessionsAfter, err := db.ListSessions()
			require.NoError(t, err)
			assert.Len(t, sessionsAfter, len(sessionsBefore), "no session row is left")
			assert.Zero(t, agentd.AgentLaunchClaimsForTest(), "the launch claim is released")
			assert.Empty(t, filesNamed(t, conv+".jsonl"), "no copied conversation is left on disk")
			assertSourceUntouched(t, source, spawn.ConvID)
		})
	}
}

// A clone that succeeds keeps its copied conversation: the failure cleanup
// never runs on success, and the source is untouched.
func TestCloneUnwind_ASuccessfulCopyCloneKeepsItsCopy(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	source := snapshotConversation(t, spawn.ConvID)

	clone := f.AsHuman().CloneWith(spawn.ConvID, map[string]any{})
	require.Equalf(t, http.StatusOK, clone.Code, "body=%s", clone.Raw)
	require.True(t, clone.CopyConv)

	assert.NotEmpty(t, filesNamed(t, clone.NewConv+".jsonl"), "the clone keeps its copied conversation")
	agentID, err := db.AgentIDForConv(clone.NewConv)
	require.NoError(t, err)
	assert.NotEmpty(t, agentID, "the clone is an agent")
	assertSourceUntouched(t, source, spawn.ConvID)
}

// conversationSnapshot is what a conversation owns on disk and in the index:
// its transcript bytes, the files in its side directory, and its index row.
type conversationSnapshot struct {
	Transcripts map[string]string
	SideFiles   []string
	Indexed     bool
}

// assertSourceUntouched checks the clone's source after the clone: its
// transcript and side files are byte-for-byte what they were, and it is
// indexed. The clone itself may index the source (the index fills lazily), so
// the row is checked for presence only: what the cleanup must never do is
// delete it.
func assertSourceUntouched(t *testing.T, before conversationSnapshot, convID string) {
	t.Helper()
	after := snapshotConversation(t, convID)
	assert.Equal(t, before.Transcripts, after.Transcripts, "the source's transcript is untouched")
	assert.Equal(t, before.SideFiles, after.SideFiles, "the source's side files are untouched")
	assert.True(t, after.Indexed, "the source is still indexed")
}

func snapshotConversation(t *testing.T, convID string) conversationSnapshot {
	t.Helper()
	snap := conversationSnapshot{Transcripts: map[string]string{}}
	for _, path := range filesNamed(t, convID+".jsonl") {
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		snap.Transcripts[path] = string(raw)
		side := filepath.Join(filepath.Dir(path), convID)
		_ = filepath.WalkDir(side, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				snap.SideFiles = append(snap.SideFiles, p)
			}
			return nil
		})
	}
	require.NotEmpty(t, snap.Transcripts, "conversation %s has a transcript on disk", convID)
	row, err := db.GetConvIndex(convID)
	require.NoError(t, err)
	snap.Indexed = row != nil
	return snap
}

// filesNamed lists the files called name under the test's HOME.
func filesNamed(t *testing.T, name string) []string {
	t.Helper()
	var found []string
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	_ = filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == name {
			found = append(found, path)
		}
		return nil
	})
	return found
}
