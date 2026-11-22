package agentd_test

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const ifMatchOriginal = "The operator's context."

func haveContextGroup(t *testing.T, f *testharness.Flow) {
	t.Helper()
	f.HaveGroup("alpha")
	_, err := db.SetAgentGroupDefaultContext("alpha", ifMatchOriginal)
	require.NoError(t, err)
}

func groupContextNow(t *testing.T) string {
	t.Helper()
	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	return g.DefaultContext
}

func setContextAs(t *testing.T, f *testharness.Flow, conv, context, ifMatch string) (int, string, string) {
	t.Helper()
	if conv == "" {
		bridgeAgentClientToMux(t, f.Mux)
	} else {
		bridgeAgentClientToMuxAsAgent(t, f.Mux, conv)
	}
	var stdout, stderr bytes.Buffer
	rc := agent.RunGroupsSetContext("alpha", context, ifMatch, &stdout, &stderr)
	return rc, stdout.String(), stderr.String()
}

func TestGroupContextIfMatch_WritesOnlyOverTheContextItWasRead(t *testing.T) {
	f := newFlow(t)
	haveContextGroup(t, f)
	read := db.GroupContextHash(ifMatchOriginal)

	rc, _, stderr := setContextAs(t, f, "", "Edited once.", read)
	require.Zero(t, rc, stderr)
	assert.Equal(t, "Edited once.", groupContextNow(t))

	// The same read hash is now stale: nothing is written, and the current
	// hash is given to merge against.
	rc, _, stderr = setContextAs(t, f, "", "Blind overwrite.", read)
	assert.NotZero(t, rc)
	assert.Equal(t, "Edited once.", groupContextNow(t))
	assert.Contains(t, stderr, "changed since you read it")
	assert.Contains(t, stderr, db.GroupContextHash("Edited once."))

	// Without --if-match the write is as it always was.
	rc, _, stderr = setContextAs(t, f, "", "Unchecked.", "")
	require.Zero(t, rc, stderr)
	assert.Equal(t, "Unchecked.", groupContextNow(t))
}

func TestGroupContextIfMatch_TheAPIAnswersAConflictWithTheCurrentHash(t *testing.T) {
	f := newFlow(t)
	haveContextGroup(t, f)
	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPatch, "/v1/groups/alpha",
		map[string]any{"default_context": "x", "default_context_if_match": "0000"}))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"code":"group_context_conflict"`)
	assert.Contains(t, rec.Body.String(), db.GroupContextHash(ifMatchOriginal))

	r = agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPatch, "/v1/groups/alpha",
		map[string]any{"descr": "d", "default_context_if_match": db.GroupContextHash(ifMatchOriginal)}))
	rec = testharness.Serve(f.Mux, r)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "the check goes with a context write")

	// A stale check stops the whole update: no other field is written first.
	r = agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPatch, "/v1/groups/alpha",
		map[string]any{"descr": "changed", "default_context": "x", "default_context_if_match": "0000"}))
	rec = testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusConflict, rec.Code)
	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	assert.NotEqual(t, "changed", g.Descr)
}

// --if-match grants nothing: a member without groups.settings.default-context
// is refused with and without it, with the right hash or a wrong one, before
// any hash is compared or revealed.
func TestGroupContextIfMatch_AMemberWithoutTheGrantIsStillRefused(t *testing.T) {
	f := newFlow(t)
	haveContextGroup(t, f)
	const member = "if-match-member-aaaa-1111"
	f.HaveConvWithTitle(member, "member")
	f.HaveMember("alpha", member)

	for _, ifMatch := range []string{"", db.GroupContextHash(ifMatchOriginal), "0000"} {
		rc, _, stderr := setContextAs(t, f, member, "Overwritten by a member.", ifMatch)
		assert.NotZero(t, rc, "if-match %q", ifMatch)
		assert.Contains(t, stderr, agentd.PermGroupsSettingsDefaultContext, "if-match %q", ifMatch)
		assert.NotContains(t, stderr, db.GroupContextHash(ifMatchOriginal), "a refusal reveals no hash")
		assert.Equal(t, ifMatchOriginal, groupContextNow(t))
	}

	// The same holds on the API, as a 403 rather than a 409.
	r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPatch, "/v1/groups/alpha",
		map[string]any{"default_context": "x", "default_context_if_match": "0000"}), member)
	rec := testharness.Serve(f.Mux, r)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestGroupContextIfMatch_AnOwnerWritesWithAFreshHash(t *testing.T) {
	f := newFlow(t)
	haveContextGroup(t, f)
	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	const owner = "if-match-owner-bbbb-2222"
	f.HaveConvWithTitle(owner, "owner")
	require.NoError(t, db.AddAgentGroupOwner(g.ID, owner, "test"))

	rc, _, stderr := setContextAs(t, f, owner, "The owner's edit.", "0000")
	assert.NotZero(t, rc)
	assert.Contains(t, stderr, db.GroupContextHash(ifMatchOriginal))
	rc, _, stderr = setContextAs(t, f, owner, "The owner's edit.", db.GroupContextHash(ifMatchOriginal))
	require.Zero(t, rc, stderr)
	assert.Equal(t, "The owner's edit.", groupContextNow(t))
}

// A caller who may write the context but not read it learns of a conflict,
// never the current hash: --if-match reveals nothing groups show withholds.
func TestGroupContextIfMatch_AConflictHidesTheHashFromAWriterWhoCannotRead(t *testing.T) {
	f := newFlow(t)
	haveContextGroup(t, f)
	const writer = "if-match-writer-cccc-3333"
	f.HaveConvWithTitle(writer, "writer")
	require.NoError(t, db.GrantAgentPermission(writer, agentd.PermGroupsSettingsDefaultContext, "test"))

	rc, _, stderr := setContextAs(t, f, writer, "Blind.", "0000")
	assert.NotZero(t, rc)
	assert.Contains(t, stderr, "changed since you read it")
	assert.NotContains(t, stderr, db.GroupContextHash(ifMatchOriginal))
	assert.Equal(t, ifMatchOriginal, groupContextNow(t))

	r := agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPatch, "/v1/groups/alpha",
		map[string]any{"default_context": "x", "default_context_if_match": "0000"}), writer)
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "current_hash")
}
