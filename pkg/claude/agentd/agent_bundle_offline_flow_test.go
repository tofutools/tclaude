package agentd_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// countingSpawner records which launches reach the session spawner.
type countingSpawner struct {
	inner   agentd.Spawner
	mu      sync.Mutex
	news    int
	resumes []clcommon.SpawnArgs
}

func (s *countingSpawner) SpawnNew(args clcommon.SpawnArgs) error {
	s.mu.Lock()
	s.news++
	s.mu.Unlock()
	return s.inner.SpawnNew(args)
}

func (s *countingSpawner) SpawnResume(args clcommon.SpawnArgs) error {
	s.mu.Lock()
	s.resumes = append(s.resumes, args)
	s.mu.Unlock()
	return s.inner.SpawnResume(args)
}

// An applied import with start=false lands the agent offline: it joins the
// group with its conversation installed, nothing launches, and resume
// starts it on its landing harness in its working directory.
func TestAgentImportWithoutStartLandsOffline(t *testing.T) {
	f := newFlow(t)
	t.Setenv("CODEX_HOME", "")
	group := f.HaveGroup("receiver")
	const source = "0f0e0d0c-0b0a-4908-8706-050403020b01"
	cwd := testutil.CanonicalTempDir(t)
	seedClaudeConversation(t, f, source, "offline-src", cwd)
	rec := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&history=true&format=paf", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	raw := rec.Body.Bytes()

	spawner := &countingSpawner{inner: agentd.Spawn}
	agentd.Spawn = spawner
	t.Cleanup(func() { agentd.Spawn = spawner.inner })

	dest := testutil.CanonicalTempDir(t)
	query := url.Values{"harness": {"codex"}, "model": {"gpt-5.5"}, "cwd": {dest}, "apply": {"true"}, "group": {"receiver"}, "name": {"parked"}, "start": {"false"}}
	r := agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/agent-bundle/import?"+query.Encode(), bytes.NewReader(raw)))
	out := httptest.NewRecorder()
	f.Mux.ServeHTTP(out, r)
	require.Equal(t, http.StatusOK, out.Code, out.Body.String())
	var res struct {
		landingResult
		Offline bool `json:"offline"`
	}
	require.NoError(t, json.Unmarshal(out.Body.Bytes(), &res))
	require.True(t, res.Applied)
	assert.True(t, res.Offline)
	conv := res.Spawn.ConvID
	require.NotEmpty(t, conv)
	assert.Zero(t, spawner.news, "an offline import starts no session")
	assert.Contains(t, historyText(t, mustGetHarness(t, harness.CodexName), conv, dest), morphSecret)

	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	require.NotEmpty(t, agentID)
	members, err := db.ListAgentGroupMembers(group.ID)
	require.NoError(t, err)
	var member bool
	for _, m := range members {
		member = member || m.ConvID == conv
	}
	assert.True(t, member, "the offline agent joins the receiving group")

	resumed := f.Resume(conv)
	f.AssertResumeSpawned(resumed)
	require.Len(t, spawner.resumes, 1)
	assert.Equal(t, harness.CodexName, spawner.resumes[0].Harness)
	assert.Equal(t, dest, spawner.resumes[0].Cwd)
	assert.Zero(t, spawner.news)

	// Without a conversation to land there is nothing to resume.
	query.Set("skip_history", "true")
	r = agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/agent-bundle/import?"+query.Encode(), bytes.NewReader(raw)))
	out = httptest.NewRecorder()
	f.Mux.ServeHTTP(out, r)
	assert.Equal(t, http.StatusBadRequest, out.Code, out.Body.String())
	assert.Contains(t, out.Body.String(), "no_start")
}

// An OpenCode agent lands offline with no server started. Its conversation goes
// in the store its posture's server reads, and resume finds it there.
// OpenCode takes a model only from the launch prompt, so an offline landing says
// that the model it records is not yet the one the conversation runs on.
func TestAgentImportWithoutStartWarnsOfOpenCodeModel(t *testing.T) {
	f := newFlow(t)
	store := fakeOpenCodeStore(t)
	const source = "ses_0123456789abcdefghijklmnoq"
	seedOpenCodeSession(t, store, source, "scout", testutil.CanonicalTempDir(t))
	f.HaveGroup("source")
	f.HaveMember("source", source)
	f.HaveGroup("receiver")
	rec := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&history=true&format=paf", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	query := url.Values{"apply": {"true"}, "start": {"false"}, "cwd": {testutil.CanonicalTempDir(t)},
		"group": {"receiver"}, "model": {"openai/gpt-landing"}}

	res := importAgentArchive(t, f, rec.Body.Bytes(), query)

	require.True(t, res.Applied)
	assert.Contains(t, strings.Join(res.Warnings, " "), "continues on the model it last used")
}

func TestAgentImportWithoutStartLandsOnOpenCode(t *testing.T) {
	for _, tc := range []struct {
		name, implementation string
		private              bool
	}{
		{name: "shared store at the baseline"},
		{name: "private state under the sandbox", implementation: "tclaude-layer", private: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			store := fakeOpenCodeStore(t)
			const source = "ses_0123456789abcdefghijklmnoq"
			seedOpenCodeSession(t, store, source, "scout", testutil.CanonicalTempDir(t))
			f.HaveGroup("source")
			f.HaveMember("source", source)
			f.HaveGroup("receiver")
			rec := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&history=true&format=paf", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			if tc.implementation != "" {
				setSandboxImplDefault(t, f, tc.implementation)
			}
			before := storedOpenCodeSessions(t, store)
			spawner := &countingSpawner{inner: agentd.Spawn}
			agentd.Spawn = spawner
			t.Cleanup(func() { agentd.Spawn = spawner.inner })
			var served []string
			t.Cleanup(agentd.SetOpenCodeRuntimeForTest(func(sessionID, _, _, resumeID string) (agentd.OpenCodeRuntimeFixture, error) {
				served = append(served, resumeID)
				return agentd.OpenCodeRuntimeFixture{ConvID: resumeID, ServerURL: "http://127.0.0.1:1", Password: "pw", PID: 1}, nil
			}))

			dest := testutil.CanonicalTempDir(t)
			res := importAgentArchive(t, f, rec.Body.Bytes(), url.Values{
				"apply": {"true"}, "start": {"false"}, "cwd": {dest}, "group": {"receiver"}})
			require.True(t, res.Applied)
			conv := res.Spawn.ConvID
			assert.Empty(t, served, "landing offline starts no server")
			assert.Zero(t, spawner.news)
			profile, err := db.ConversationResumeProfileForConv(conv)
			require.NoError(t, err)
			require.NotNil(t, profile)
			assert.Equal(t, harness.OpenCodeName, profile.Harness)
			assert.Equal(t, dest, profile.Cwd)

			agentID, err := db.AgentIDForConv(conv)
			require.NoError(t, err)
			allocation, err := db.GetOpenCodeAgentStateAllocation(agentID)
			require.NoError(t, err)
			if tc.private {
				require.NotNil(t, allocation)
				assert.Contains(t, storedOpenCodeSessions(t, filepath.Join(allocation.StateRoot, "data", "fake-store")), conv)
				assert.Equal(t, before, storedOpenCodeSessions(t, store), "the shared store takes nothing")
			} else {
				assert.Nil(t, allocation)
				assert.Contains(t, storedOpenCodeSessions(t, store), conv)
			}

			f.AssertResumeSpawned(f.Resume(conv))
			require.Len(t, spawner.resumes, 1)
			assert.Equal(t, harness.OpenCodeName, spawner.resumes[0].Harness)
			assert.Equal(t, dest, spawner.resumes[0].Cwd)
			if tc.private {
				assert.Equal(t, tc.implementation, spawner.resumes[0].SandboxImplementation)
			}
		})
	}
}

// privateOpenCodeStateLeft is what failed spawns left behind: the state roots on
// disk and the allocation rows.
func privateOpenCodeStateLeft(t *testing.T) ([]string, int) {
	t.Helper()
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		require.NoError(t, err)
		base = filepath.Join(home, ".local", "share")
	}
	roots, err := filepath.Glob(filepath.Join(base, "tclaude", "opencode-agents", "agt_*"))
	require.NoError(t, err)
	d, err := db.Open()
	require.NoError(t, err)
	var rows int
	require.NoError(t, d.QueryRow(`SELECT COUNT(*) FROM opencode_agent_state_allocations`).Scan(&rows))
	return roots, rows
}

// A spawn that fails before its agent exists leaves no private state behind, on
// the offline path and on the started one.
func TestAgentImport_FailedLandingLeavesNoPrivateState(t *testing.T) {
	for _, tc := range []struct {
		name, start           string
		failImport, failStart bool
		failPrompt            bool
	}{
		{name: "offline, the conversation does not import", start: "false", failImport: true},
		{name: "started, the conversation does not import", start: "true", failImport: true},
		{name: "started, the server does not start", start: "true", failStart: true},
		{name: "started, the first prompt fails after the fork", start: "true", failPrompt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			store := fakeOpenCodeStore(t)
			const source = "ses_0123456789abcdefghijklmnoz"
			seedOpenCodeSession(t, store, source, "scout", testutil.CanonicalTempDir(t))
			f.HaveGroup("source")
			f.HaveMember("source", source)
			f.HaveGroup("receiver")
			rec := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+source+"&history=true&format=paf", nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			setSandboxImplDefault(t, f, "tclaude-layer")
			if tc.failImport {
				require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(store), "import-fails"), nil, 0o600))
			}
			t.Cleanup(agentd.SetOpenCodeRuntimeForTest(func(sessionID, _, _, resumeID string) (agentd.OpenCodeRuntimeFixture, error) {
				if tc.failStart {
					return agentd.OpenCodeRuntimeFixture{}, errors.New("port already in use")
				}
				return agentd.OpenCodeRuntimeFixture{SessionID: sessionID, ConvID: resumeID, ServerURL: "http://127.0.0.1:1", Password: "pw", PID: 1}, nil
			}))
			if tc.failPrompt {
				t.Cleanup(agentd.SetOpenCodePromptErrorForTest(errors.New("server went away")))
			}
			query := url.Values{"apply": {"true"}, "start": {tc.start}, "cwd": {testutil.CanonicalTempDir(t)}, "group": {"receiver"}}
			r := agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/agent-bundle/import?"+query.Encode(), bytes.NewReader(rec.Body.Bytes())))
			failed := httptest.NewRecorder()
			f.Mux.ServeHTTP(failed, r)

			require.NotEqual(t, http.StatusOK, failed.Code, failed.Body.String())
			roots, rows := privateOpenCodeStateLeft(t)
			assert.Empty(t, roots)
			assert.Zero(t, rows)
		})
	}
}

// State that was there before a spawn is not that spawn's to remove.
func TestPrivateOpenCodeState_ClaimOwnsOnlyWhatItCreated(t *testing.T) {
	newFlow(t)
	const agentID = "agt_0123456789abcdef0123456789abcdef"
	discard, err := agentd.ClaimPrivateOpenCodeStateForTest(agentID)
	require.NoError(t, err)
	require.NotNil(t, discard)

	again, err := agentd.ClaimPrivateOpenCodeStateForTest(agentID)
	require.NoError(t, err)
	assert.Nil(t, again)

	discard()
	allocation, err := db.GetOpenCodeAgentStateAllocation(agentID)
	require.NoError(t, err)
	assert.Nil(t, allocation)
}
