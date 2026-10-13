package harness

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// piTestAgentDir isolates HOME (and so tclaude's DB) and points pi's agent
// directory at a fresh fixture.
func piTestAgentDir(t *testing.T) string {
	t.Helper()
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv(PiSessionDirEnvVar, "")
	agentDir := filepath.Join(home, "pi-agent")
	t.Setenv(PiAgentDirEnvVar, agentDir)
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	return agentDir
}

func piWriteSession(t *testing.T, agentDir, cwd, id string, records ...any) string {
	t.Helper()
	dir := filepath.Join(agentDir, "sessions", PiSessionDirName(cwd))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "2026-10-01T10-00-00-000Z_"+id+".jsonl")
	require.NoError(t, os.WriteFile(path, jsonl(t, records...), 0o600))
	return path
}

func piHeader(id, cwd string) obj {
	return obj{"type": "session", "version": 3, "id": id, "timestamp": "2026-10-01T10:00:00.000Z", "cwd": cwd}
}

func piMsg(id string, parent any, m obj) obj {
	return obj{"type": "message", "id": id, "parentId": parent, "timestamp": "2026-10-01T10:00:01.000Z", "message": m}
}

func TestPiConvStoreListsTitlesAndResolves(t *testing.T) {
	agentDir := piTestAgentDir(t)
	projA, projB := testutil.CanonicalTempDir(t), testutil.CanonicalTempDir(t)
	const named, plain, empty = "aaaa1111-0000-4000-8000-000000000001", "bbbb2222-0000-4000-8000-000000000002", "cccc3333-0000-4000-8000-000000000003"
	piWriteSession(t, agentDir, projA, named, piHeader(named, projA),
		obj{"type": "model_change", "id": "m0", "parentId": nil, "provider": "openai", "modelId": "gpt-5.5"},
		piMsg("u1", "m0", obj{"role": "user", "content": []obj{{"type": "text", "text": "Investigate the flaky test"}}, "timestamp": 1790000000000}),
		obj{"type": "session_info", "id": "n1", "parentId": "u1", "name": "flaky hunt"},
	)
	piWriteSession(t, agentDir, projB, plain, piHeader(plain, projB),
		piMsg("u1", nil, obj{"role": "user", "content": "just a prompt", "timestamp": 1790000001000}),
	)
	// Launched, never prompted: not a conversation yet.
	piWriteSession(t, agentDir, projB, empty, piHeader(empty, projB))

	store := piConvStore{}
	all, err := store.ListConvs("")
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, plain, all[0].SessionID, "newest activity first")
	assert.Equal(t, "just a prompt", all[0].DisplayTitle())
	assert.Equal(t, "flaky hunt", all[1].DisplayTitle(), "pi's session name is the title")
	assert.Equal(t, "openai/gpt-5.5", all[1].Model)
	assert.Equal(t, PiName, all[1].Harness)

	scoped, err := store.ListConvs(projA)
	require.NoError(t, err)
	require.Len(t, scoped, 1)
	assert.Equal(t, named, scoped[0].SessionID)

	ref, err := store.Resolve("aaaa", projB, true)
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, named, ref.ConvID)
	assert.Equal(t, projA, ref.ProjectPath)
	ref, err = store.Resolve("aaaa", projB, false)
	require.NoError(t, err)
	assert.Nil(t, ref, "a cwd-scoped resolve does not reach another project")

	ok, err := store.Exists(empty, "")
	require.NoError(t, err)
	assert.False(t, ok)

	// A cold rename appends a session_info pi itself reads, on the current
	// branch.
	require.NoError(t, store.SetTitle(plain, "renamed cold"))
	title, err := store.Title(plain)
	require.NoError(t, err)
	assert.Equal(t, "renamed cold", title)
	f, err := os.Open(piFindSessionFile(filepath.Join(agentDir, "sessions"), plain))
	require.NoError(t, err)
	defer f.Close()
	session, err := readPiSession(f)
	require.NoError(t, err)
	branch := session.Branch()
	assert.Equal(t, "session_info", branch[len(branch)-1].Type)
	assert.Equal(t, "u1", branch[len(branch)-1].parent())
}

func TestPiHistoryImportRemintsAndResumesInReceiverStore(t *testing.T) {
	agentDir := piTestAgentDir(t)
	const source = "dddd4444-0000-4000-8000-000000000004"
	raw := jsonl(t,
		obj{"type": "session", "version": 3, "id": source, "timestamp": "2026-10-01T10:00:00.000Z", "cwd": "/sender/proj", "parentSession": "/sender/other.jsonl"},
		piMsg("u1", nil, obj{"role": "user", "content": "keep this text /sender/proj verbatim"}),
	)
	h := piHistory{}
	require.NoError(t, h.Validate(raw, source))
	require.Error(t, h.Validate(raw, "someone-else"), "the header must name the source")
	require.Error(t, h.Validate([]byte(`{"type":"message","id":"x"}`+"\n"), "x"), "a session needs its header")
	require.Error(t, h.Validate([]byte(`{"type":"session","version":4,"id":"x"}`+"\n"), "x"), "a newer format is refused")

	cwd := testutil.CanonicalTempDir(t)
	id, cleanup, err := h.Import(raw, source, cwd)
	require.NoError(t, err)
	require.NotEqual(t, source, id)
	path := piFindSessionFile(filepath.Join(agentDir, "sessions"), id)
	require.NotEmpty(t, path)
	assert.Equal(t, PiSessionDirName(cwd), filepath.Base(filepath.Dir(path)), "landed where pi looks for cwd's sessions")

	r, err := h.Open(id, cwd)
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, r.Close())
	require.NoError(t, err)
	session, err := readPiSession(bytes.NewReader(got))
	require.NoError(t, err)
	assert.Equal(t, id, session.Header.ID)
	assert.Equal(t, cwd, session.Header.Cwd)
	assert.Empty(t, session.Header.ParentSession, "the sender's parent link is dropped")
	assert.Greater(t, session.Header.Timestamp, "2026-10-01T10:00:00.000Z",
		"the session starts at the import, so the extension does not count the copied spend again")
	assert.Contains(t, string(got), "keep this text /sender/proj verbatim", "message text is untouched")
	row, err := db.GetConvIndex(id)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, PiName, row.Harness)

	cleanup()
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))
}

func TestPiTranscriptFollowsCompactionEditsAndBranch(t *testing.T) {
	const id = fixturePi
	asst := func(stop, text string) obj {
		return obj{"role": "assistant", "content": []obj{{"type": "text", "text": text}}, "provider": "openai", "model": "gpt-5.5", "stopReason": stop}
	}
	raw := jsonl(t,
		piHeader(id, "/src"),
		piMsg("s0", nil, obj{"role": "system", "content": "base prompt"}),
		piMsg("u1", "s0", obj{"role": "user", "content": "old prompt"}),
		piMsg("a1", "u1", asst("stop", "old answer")),
		piMsg("u2", "a1", obj{"role": "user", "content": "kept prompt"}),
		piMsg("s1", "u2", obj{"role": "system", "content": "a later instruction"}),
		piMsg("a2", "s1", asst("stop", "kept answer")),
		obj{"type": "compaction", "id": "c1", "parentId": "a2", "summary": "SUMMARY-TEXT", "firstKeptEntryId": "u2", "tokensBefore": 900},
		piMsg("u3", "c1", obj{"role": "user", "content": "edited away"}),
		obj{"type": "context_edit", "id": "e1", "parentId": "u3", "targetId": "u3", "replacement": obj{"content": "replaced prompt"}},
		piMsg("a3", "e1", asst("error", "partial")),
		piMsg("x1", "a3", obj{"role": "bashExecution", "command": "ls", "output": "README.md", "exitCode": 0, "excludeFromContext": true}),
		piMsg("x2", "x1", obj{"role": "bashExecution", "command": "pwd", "output": "/src", "exitCode": 2}),
		obj{"type": "custom_message", "id": "cm", "parentId": "x2", "customType": "note", "content": "extension note", "display": true},
		obj{"type": "branch_summary", "id": "bs", "parentId": "cm", "fromId": "zz", "summary": "BRANCH-TEXT"},
		piMsg("a4", "bs", asst("stop", "final answer")),
		obj{"type": "mystery", "id": "q1", "parentId": "a4"},
		piMsg("ab", "a2", obj{"role": "user", "content": "abandoned"}),
		piMsg("a5", "q1", asst("stop", "really final")),
	)
	tr := decodeFixture(t, piTranscript{}, id, raw)
	assert.Equal(t, []string{
		"user|SUMMARY:SUMMARY-TEXT",
		"user|kept prompt",
		"assistant|kept answer",
		"user|replaced prompt",
		"user|Ran `pwd`\n```\n/src\n```\n\nCommand exited with code 2",
		"user|extension note," + piBranchSummaryPrefix + "BRANCH-TEXT" + piBranchSummarySuffix,
		"assistant|final answer,really final",
	}, entryTexts(tr))
	assert.Equal(t, 2, tr.Losses.Count(transcript.LossPreCompaction), "the two messages before the kept range")
	assert.Equal(t, 1, tr.Losses.Count(transcript.LossInactiveBranch))
	assert.Equal(t, 2, tr.Losses.Count(transcript.LossHarnessContext), "both system messages on the branch")
	assert.Equal(t, 1, tr.Losses.Count(transcript.LossUnknownRecord))
	assert.Equal(t, "openai/gpt-5.5", tr.Source.Model)

	_, err := piTranscript{}.Decode(bytes.NewReader(raw), "another-id")
	require.Error(t, err)
}

func TestPiTranscriptEncodeResumesAsLinearBranch(t *testing.T) {
	plan := transcript.NewPlan(decodeFixture(t, piTranscript{}, fixturePi, piFixture(t)),
		transcript.Options{Target: PiName, TargetModel: "openai/gpt-5.5", InlineMedia: true})
	var buf bytes.Buffer
	id, err := piTranscript{}.Encode(plan, "/dst", &buf)
	require.NoError(t, err)
	require.NoError(t, piHistory{}.Validate(buf.Bytes(), id))
	session, err := readPiSession(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, "/dst", session.Header.Cwd)
	assert.Len(t, session.Branch(), len(session.Entries), "one linear branch")
	assert.Equal(t, "model_change", session.Entries[0].Type)
	assert.Equal(t, "gpt-5.5", session.Entries[0].ModelID)
	assert.Equal(t, plan.Title, session.Name())
	for _, e := range session.Entries {
		if e.Type == "message" && e.Message.Role == "assistant" {
			require.NotNil(t, e.Message.Usage, "pi sums usage over every assistant message")
			assert.Equal(t, "openai", e.Message.Provider)
		}
	}
	assert.True(t, strings.Contains(buf.String(), `"type":"image"`), "images stay inline")
}

func TestPiColdRenamePreservesOversizedLeaf(t *testing.T) {
	agentDir := piTestAgentDir(t)
	cwd := testutil.CanonicalTempDir(t)
	path := piWriteSession(t, agentDir, cwd, fixturePi, piHeader(fixturePi, cwd),
		piMsg("u1", nil, obj{"role": "user", "content": "original task"}),
		piMsg("huge", "u1", obj{"role": "toolResult", "content": strings.Repeat("x", copilotEventLineLimit+100)}))
	require.NoError(t, (piConvStore{}).SetTitle(fixturePi, "cold-name"))
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	parsed, err := decodePiSession(bytes.NewReader(raw))
	require.NoError(t, err)
	leaf := parsed.Entries[len(parsed.Entries)-1]
	require.Equal(t, "huge", leaf.parent(), "rename must retain pi's actual final entry as parent")
	require.Len(t, parsed.Branch(), 3, "the original task and huge result remain on the branch")
}

func TestPiOldSessionVersionsRefuseUntilNativeMigration(t *testing.T) {
	for _, version := range []int{1, 2, 4} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			header := piHeader(fixturePi, "/work")
			header["version"] = version
			raw := jsonl(t, header, piMsg("u1", nil, obj{"role": "user", "content": "old task"}))
			_, err := readPiSession(bytes.NewReader(raw))
			require.ErrorContains(t, err, "reopen it in pi")
			_, err = decodePiSession(bytes.NewReader(raw))
			require.ErrorContains(t, err, "reopen it in pi")
		})
	}
}
