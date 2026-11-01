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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

const postureMember = "0f0e0d0c-0b0a-4908-8706-0504030201c1"

// inspectGroupWith drives the import dry run.
func inspectGroupWith(t *testing.T, f *testharness.Flow, archive []byte, query url.Values) []groupLandingView {
	t.Helper()
	r := agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/groups/import/inspect?"+query.Encode(), bytes.NewReader(archive)))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var insp struct {
		Landings []groupLandingView `json:"landings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &insp))
	return insp.Landings
}

// exportClaudeMember exports a group of one Claude Code member and forgets the
// source, as if the file had reached another node.
func exportClaudeMember(t *testing.T, f *testharness.Flow, format string) []byte {
	t.Helper()
	seedClaudeConversation(t, f, postureMember, "scout", f.TestCwd("src"))
	f.HaveGroup("crew")
	f.HaveMember("crew", postureMember)
	return exportGroupAs(t, f, "crew", format)
}

func landedMember(t *testing.T, group string) string {
	t.Helper()
	g, err := db.GetAgentGroupByName(group)
	require.NoError(t, err)
	require.NotNil(t, g)
	members, err := db.ListAgentGroupMembers(g.ID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	return members[0].ConvID
}

// A member lands under the sandbox a spawn here would get: the dry run and the
// result say so, and its first resume launches inside it.
func TestGroupImport_MemberAdoptsTheInstallDefaultSandbox(t *testing.T) {
	f := newFlow(t)
	archive := exportClaudeMember(t, f, "paf")
	setSandboxImplDefault(t, f, "tclaude-layer")
	dst := f.TestCwd("dst")
	require.NoError(t, os.MkdirAll(dst, 0o755))
	q := url.Values{"into": {dst}, "as": {"crew-walled"}}

	dry := inspectGroupWith(t, f, archive, q)
	require.Len(t, dry, 1)
	require.NotNil(t, dry[0].Posture)
	assert.Equal(t, "tclaude-layer", dry[0].Posture.SandboxImplementation)
	assert.Contains(t, dry[0].Posture.ResolvedBy, "install default")

	rec, landings := importGroupWith(t, f, archive, q)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, landings, 1)
	assert.Equal(t, "tclaude-layer", landings[0].Posture.SandboxImplementation)

	landed := landedMember(t, "crew-walled")
	g, err := db.GetAgentGroupByName("crew-walled")
	require.NoError(t, err)
	snapshot, err := db.AgentEffectiveSandboxConfigForConv(landed)
	require.NoError(t, err)
	require.NotNil(t, snapshot, "the member has a posture before anything launches it")
	assert.Equal(t, g.ID, snapshot.ResolutionGroupID)

	assert.Equal(t, "resumed", f.Resume(landed).Action)
	got, ok := f.World.SpawnSandboxImplementation(landed)
	require.True(t, ok)
	assert.Equal(t, "tclaude-layer", got)
}

// A posture this node cannot meet refuses the import through the spawn gate's
// own refusal, and writes nothing. The dry run reports it instead of failing.
func TestGroupImport_RefusesAPostureThisNodeCannotMeet(t *testing.T) {
	f := newFlow(t)
	archive := exportClaudeMember(t, f, "paf")
	setSandboxImplDefault(t, f, "tclaude-layer")
	t.Cleanup(agentd.SetTclaudeLayerHostAvailabilityForTest(func() error { return errors.New("no bwrap") }))
	q := url.Values{"into": {f.TestCwd("dst")}, "as": {"crew-walled"}}

	dry := inspectGroupWith(t, f, archive, q)
	require.Len(t, dry, 1)
	assert.Contains(t, dry[0].Posture.Refused, "no bwrap")

	rec, _ := importGroupWith(t, f, archive, q)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	var failure struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &failure))
	assert.Equal(t, "sandbox_implementation_unavailable", failure.Code)
	assert.Contains(t, failure.Error, "no bwrap")
	assert.Contains(t, failure.Error, "sandbox-impl default set harness")
	g, err := db.GetAgentGroupByName("crew-walled")
	require.NoError(t, err)
	assert.Nil(t, g)
}

// A source that ran with no sandbox carries none here. The landing posture is
// the receiver's, whatever the file recorded.
func TestGroupImport_RecordedPostureDoesNotWiden(t *testing.T) {
	f := newFlow(t)
	seedClaudeConversation(t, f, postureMember, "scout", f.TestCwd("src"))
	f.HaveGroup("crew")
	f.HaveMember("crew", postureMember)
	agentID, err := db.AgentIDForConv(postureMember)
	require.NoError(t, err)
	off := "off"
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, db.AgentRelaunchProfile{
		Version: db.RelaunchProfileVersion, SandboxImplementation: &off,
	}))
	archive := exportGroupAs(t, f, "crew", "paf")
	setSandboxImplDefault(t, f, "tclaude-layer")

	rec, landings := importGroupWith(t, f, archive, url.Values{"into": {f.TestCwd("dst")}, "as": {"crew-walled"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "tclaude-layer", landings[0].Posture.SandboxImplementation)
}

// The member's snapshot names its group, so a sandbox profile assigned to the
// group after the import binds the member's next launch.
func TestGroupImport_GroupProfileAssignedLaterBindsTheMember(t *testing.T) {
	f := newFlow(t)
	archive := exportClaudeMember(t, f, "paf")
	rec, _ := importGroupWith(t, f, archive, url.Values{"into": {f.TestCwd("dst")}, "as": {"crew-later"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	landed := landedMember(t, "crew-later")

	_, err := db.CreateSandboxProfile(&db.SandboxProfile{Name: "walled"})
	require.NoError(t, err)
	_, err = db.SetAgentGroupSandboxProfile("crew-later", "walled")
	require.NoError(t, err)
	current, _, err := agentd.ResolveCurrentSandboxChainForConvForTest(landed)
	require.NoError(t, err)
	require.NotNil(t, current)
	var names []string
	for _, applied := range current.Applied {
		names = append(names, applied.Name)
	}
	assert.Contains(t, names, "walled")
}

// Under tclaude's sandbox an OpenCode member's conversation goes in its private
// state, never the shared store, under the agent identity it landed with.
func TestGroupImport_OpenCodeMemberUnderTheSandboxLandsInPrivateState(t *testing.T) {
	f := newFlow(t)
	store := fakeOpenCodeStore(t)
	const source = "ses_0123456789abcdefghijklmnos"
	cwd := testutil.CanonicalTempDir(t)
	seedOpenCodeSession(t, store, source, "scout", cwd)
	f.HaveGroup("crew")
	f.HaveMember("crew", source)
	archive := exportGroupAs(t, f, "crew", "paf")
	setSandboxImplDefault(t, f, "tclaude-layer")
	before := storedOpenCodeSessions(t, store)

	q := url.Values{"into": {filepath.Join(testutil.CanonicalTempDir(t), "landing")}, "as": {"crew-private"}}
	dry := inspectGroupWith(t, f, archive, q)
	require.Len(t, dry, 1)
	assert.Equal(t, "private", dry[0].Posture.OpenCodeState)

	rec, landings := importGroupWith(t, f, archive, q)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, landings, 1)
	assert.Equal(t, "native", landings[0].Rung)
	assert.Equal(t, "private", landings[0].Posture.OpenCodeState)
	assert.Equal(t, before, storedOpenCodeSessions(t, store), "the shared store takes nothing")

	landed := landedMember(t, "crew-private")
	agentID, err := db.AgentIDForConv(landed)
	require.NoError(t, err)
	allocation, err := db.GetOpenCodeAgentStateAllocation(agentID)
	require.NoError(t, err)
	require.NotNil(t, allocation)
	assert.Equal(t, db.OpenCodeStatePrivate, allocation.Mode)
	private := storedOpenCodeSessions(t, filepath.Join(allocation.StateRoot, "data", "fake-store"))
	require.Contains(t, private, landed)
	assert.Contains(t, private[landed], openCodeImportSecret)
}

// An import that fails after installing leaves no private state behind.
func TestGroupImport_FailedImportRemovesPrivateState(t *testing.T) {
	f := newFlow(t)
	store := fakeOpenCodeStore(t)
	const source = "ses_0123456789abcdefghijklmnot"
	cwd := testutil.CanonicalTempDir(t)
	seedOpenCodeSession(t, store, source, "scout", cwd)
	f.HaveGroup("crew")
	f.HaveMember("crew", source)
	archive := exportGroupAs(t, f, "crew", "paf")
	setSandboxImplDefault(t, f, "tclaude-layer")
	var roots []string
	t.Cleanup(agentd.SetGroupImportAfterCollisionCheckForTest(func() {
		var err error
		base := os.Getenv("XDG_DATA_HOME")
		if base == "" {
			home, homeErr := os.UserHomeDir()
			require.NoError(t, homeErr)
			base = filepath.Join(home, ".local", "share")
		}
		roots, err = filepath.Glob(filepath.Join(base, "tclaude", "opencode-agents", "agt_*"))
		require.NoError(t, err)
		f.HaveGroup("crew-raced")
	}))

	rec, _ := importGroupWith(t, f, archive, url.Values{"into": {f.TestCwd("dst")}, "as": {"crew-raced"}})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Len(t, roots, 1)
	assert.NoDirExists(t, roots[0])
	allocation, err := db.GetOpenCodeAgentStateAllocation(filepath.Base(roots[0]))
	require.NoError(t, err)
	assert.Nil(t, allocation)
}

// An OpenCode agent whose conversation is in the shared store cannot move under
// tclaude's sandbox. The assignment says so, instead of every later resume.
func TestSandboxImplAssign_RefusesOpenCodeAgentWithSharedState(t *testing.T) {
	f := newFlow(t)
	store := fakeOpenCodeStore(t)
	const source = "ses_0123456789abcdefghijklmnou"
	cwd := testutil.CanonicalTempDir(t)
	seedOpenCodeSession(t, store, source, "scout", cwd)
	f.HaveGroup("crew")
	f.HaveMember("crew", source)
	archive := exportGroupAs(t, f, "crew", "paf")
	rec, _ := importGroupWith(t, f, archive, url.Values{"into": {testutil.CanonicalTempDir(t)}, "as": {"crew-shared"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	landed := landedMember(t, "crew-shared")

	body, err := json.Marshal(map[string]string{"implementation": "tclaude-layer"})
	require.NoError(t, err)
	r := agentd.AsHumanPeer(httptest.NewRequest(http.MethodPost, "/v1/agent/"+landed+"/sandbox-impl", bytes.NewReader(body)))
	assign := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusConflict, assign.Code, assign.Body.String())
	assert.Contains(t, assign.Body.String(), "opencode_state_unallocated")
}

// An agent file cannot widen the receiving group's posture: its recorded
// implementation is dropped, and the agent launches under the group's.
func TestAgentImport_RecordedPostureDoesNotWiden(t *testing.T) {
	f := newFlow(t)
	seedClaudeConversation(t, f, postureMember, "scout", f.TestCwd("src"))
	agentID, err := db.AgentIDForConv(postureMember)
	if err == nil && agentID == "" {
		f.HaveEnrolledAgent(postureMember)
		agentID, err = db.AgentIDForConv(postureMember)
	}
	require.NoError(t, err)
	off := "off"
	profile, err := db.AgentRelaunchProfileForConv(postureMember)
	require.NoError(t, err)
	if profile == nil {
		profile = &db.AgentRelaunchProfile{Version: db.RelaunchProfileVersion}
	}
	profile.SandboxImplementation = &off
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, *profile))
	f.HaveGroup("receivers")
	export := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+postureMember+"&history=true&format=paf", nil)
	require.Equal(t, http.StatusOK, export.Code, export.Body.String())
	raw := export.Body.Bytes()
	setSandboxImplDefault(t, f, "tclaude-layer")

	dst := f.TestCwd("dst")
	require.NoError(t, os.MkdirAll(dst, 0o755))
	res := importAgentArchive(t, f, raw, url.Values{"apply": {"true"}, "cwd": {dst}, "group": {"receivers"}})
	require.True(t, res.Applied)
	got, ok := f.World.SpawnSandboxImplementation(res.Spawn.ConvID)
	require.True(t, ok)
	assert.Equal(t, "tclaude-layer", got)
	require.NotNil(t, res.Posture)
	assert.Equal(t, "tclaude-layer", res.Posture.SandboxImplementation)
}

// An agent landed offline has its posture before anything launches it.
func TestAgentImport_OfflineLandingResumesInTheReceiverPosture(t *testing.T) {
	f := newFlow(t)
	seedClaudeConversation(t, f, postureMember, "scout", f.TestCwd("src"))
	f.HaveGroup("receivers")
	export := profileReq(t, f, http.MethodGet, "/v1/agent-bundle/export?agent="+postureMember+"&history=true&format=paf", nil)
	require.Equal(t, http.StatusOK, export.Code, export.Body.String())
	setSandboxImplDefault(t, f, "tclaude-layer")

	dst := f.TestCwd("dst")
	require.NoError(t, os.MkdirAll(dst, 0o755))
	res := importAgentArchive(t, f, export.Body.Bytes(), url.Values{
		"apply": {"true"}, "start": {"false"}, "cwd": {dst}, "group": {"receivers"}})
	require.True(t, res.Applied)
	snapshot, err := db.AgentEffectiveSandboxConfigForConv(res.Spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, snapshot)

	assert.Equal(t, "resumed", f.Resume(res.Spawn.ConvID).Action)
	got, ok := f.World.SpawnSandboxImplementation(res.Spawn.ConvID)
	require.True(t, ok)
	assert.Equal(t, "tclaude-layer", got)
}

// The spawn gate on Codex's workspace root applies to an import too.
func TestGroupImport_RefusesCodexLandingOverHome(t *testing.T) {
	f := newFlow(t)
	t.Setenv("CODEX_HOME", "")
	archive := exportClaudeMember(t, f, "paf")
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	q := url.Values{"into": {home}, "as": {"crew-home"}, "harness": {"codex"}, "confirm_history_loss": {"true"}}

	dry := inspectGroupWith(t, f, archive, q)
	require.Len(t, dry, 1)
	assert.Contains(t, dry[0].Posture.Refused, "project subdirectory")

	rec, _ := importGroupWith(t, f, archive, q)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid_cwd")
}

// A member with no conversation has nothing in either store to report.
func TestGroupImport_ConfigOnlyOpenCodeMemberReportsNoState(t *testing.T) {
	f := newFlow(t)
	store := fakeOpenCodeStore(t)
	const source = "ses_0123456789abcdefghijklmnov"
	seedOpenCodeSession(t, store, source, "scout", testutil.CanonicalTempDir(t))
	f.HaveGroup("crew")
	f.HaveMember("crew", source)
	archive := exportGroupAs(t, f, "crew", "zip")
	setSandboxImplDefault(t, f, "tclaude-layer")

	rec, landings := importGroupWith(t, f, archive, url.Values{"into": {f.TestCwd("dst")}, "as": {"crew-zip"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Len(t, landings, 1)
	assert.Equal(t, "tclaude-layer", landings[0].Posture.SandboxImplementation)
	assert.Empty(t, landings[0].Posture.OpenCodeState)
}
