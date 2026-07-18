package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// These tests exercise group-addressed sends — the extended `group:`
// multicast grammar on `tclaude agent message`:
//
//   - group:<name>        broadcast to a named group (the baseline).
//   - group:<id>          broadcast to a group by numeric id (fallback
//                         when no group name matches the token).
//   - group:              broadcast to the sender's own group.
//   - --role <role>       narrow a multicast to members holding a role.
//
// handleMulticast's member-or-owner authorisation gate is unchanged;
// the role filter only ever shrinks the recipient set after the gate.

// mcastRecipientView is the per-recipient subset of the /v1/messages
// multicast response the tests assert on. AgentID is the stable id the
// receipt names each recipient by (JOH-27 PR3b-2).
type mcastRecipientView struct {
	ConvID    string `json:"conv_id"`
	AgentID   string `json:"agent_id"`
	MessageID int64  `json:"message_id"`
	Delivered bool   `json:"delivered"`
}

// mcastRespView is the multicast-shaped subset of the /v1/messages
// response: via_group plus one recipient entry per fanned-out member.
type mcastRespView struct {
	ViaGroup   string               `json:"via_group"`
	Recipients []mcastRecipientView `json:"recipients"`
}

// decodeMcast decodes a successful multicast response.
func decodeMcast(t *testing.T, rec *httptest.ResponseRecorder) mcastRespView {
	t.Helper()
	var resp mcastRespView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp),
		"decode multicast body=%s", rec.Body.String())
	return resp
}

// recipientConvIDs lists the conv-ids a multicast fanned out to.
func recipientConvIDs(resp mcastRespView) []string {
	out := make([]string, 0, len(resp.Recipients))
	for _, r := range resp.Recipients {
		out = append(out, r.ConvID)
	}
	return out
}

// Scenario 1: group:<name> regression — a multicast to a named group
// still fans out to every non-sender member, each getting a row, and
// an alive recipient is nudged.
func TestMulticast_ByName_FansOutToAllMembers(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "mc01-send-bbbb-cccc-000000000001"
	const memberA = "mc01-aaaa-bbbb-cccc-000000000002"
	const memberB = "mc01-bbbb-bbbb-cccc-000000000003"
	f.HaveMember("team", sender)
	f.HaveMember("team", memberA)
	f.HaveMember("team", memberB)
	f.HaveAliveSession(memberA, "spwn-mc01-a", "tclaude-spwn-mc01-a", f.TestCwd("work"))

	rec := postMessage(t, f, sender, map[string]any{"to": "group:team", "body": "ship it"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeMcast(t, rec)
	assert.Equal(t, "team", resp.ViaGroup)
	assert.ElementsMatch(t, []string{memberA, memberB}, recipientConvIDs(resp),
		"every non-sender member receives the broadcast")

	for _, m := range []string{memberA, memberB} {
		rows, err := db.ListAgentMessagesForConv(m, 100)
		require.NoError(t, err)
		require.Len(t, rows, 1, "member %s got a row", m)
		assert.Equal(t, "ship it", rows[0].Body)
	}
	f.AssertSentContains("tclaude-spwn-mc01-a:0.0", "new agent message", 2*time.Second)
}

// Scenario 2: group:<id> — a numeric token resolves to the group with
// that id, fanning out identically to the by-name form.
func TestMulticast_ByNumericID(t *testing.T) {
	f := newFlow(t)

	g := f.HaveGroup("team")
	const sender = "mc02-send-bbbb-cccc-000000000001"
	const memberA = "mc02-aaaa-bbbb-cccc-000000000002"
	f.HaveMember("team", sender)
	f.HaveMember("team", memberA)

	rec := postMessage(t, f, sender, map[string]any{"to": "group:" + itoa64(g.ID), "body": "hi by id"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeMcast(t, rec)
	assert.Equal(t, "team", resp.ViaGroup, "numeric id resolves to the group")
	assert.ElementsMatch(t, []string{memberA}, recipientConvIDs(resp))

	rows, err := db.ListAgentMessagesForConv(memberA, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, g.ID, rows[0].GroupID, "row carries the resolved group's id")

	// A signed token is not all-digits — the numeric-id fallback is
	// skipped (the documented grammar is all-digits only), so it
	// resolves to no group rather than to the same id.
	signed := postMessage(t, f, sender, map[string]any{"to": "group:+" + itoa64(g.ID), "body": "x"})
	require.Equal(t, http.StatusNotFound, signed.Code, "body=%s", signed.Body.String())
}

// Scenario 3: bare group: with the sender in exactly one group resolves
// to that group.
func TestMulticast_OwnGroup_SingleGroup(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "mc03-send-bbbb-cccc-000000000001"
	const memberA = "mc03-aaaa-bbbb-cccc-000000000002"
	f.HaveMember("team", sender)
	f.HaveMember("team", memberA)

	rec := postMessage(t, f, sender, map[string]any{"to": "group:", "body": "to my group"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeMcast(t, rec)
	assert.Equal(t, "team", resp.ViaGroup, "bare group: resolves to the sender's own group")
	assert.ElementsMatch(t, []string{memberA}, recipientConvIDs(resp))
}

// Scenario 4: bare group: with the sender in no group is a 400.
func TestMulticast_OwnGroup_NoGroup_BadRequest(t *testing.T) {
	f := newFlow(t)

	const sender = "mc04-send-bbbb-cccc-000000000001"
	f.HaveEnrolledAgent(sender)

	rec := postMessage(t, f, sender, map[string]any{"to": "group:", "body": "nowhere to go"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "not a member of any group")
}

// Scenario 5: bare group: with the sender in more than one group is a
// 400, and the error names every candidate group.
func TestMulticast_OwnGroup_MultipleGroups_BadRequest(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team-a")
	f.HaveGroup("team-b")
	const sender = "mc05-send-bbbb-cccc-000000000001"
	f.HaveMember("team-a", sender)
	f.HaveMember("team-b", sender)

	rec := postMessage(t, f, sender, map[string]any{"to": "group:", "body": "which group?"})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	body := rec.Body.String()
	assert.Contains(t, body, "ambiguous")
	assert.Contains(t, body, "team-a", "error names the first candidate group")
	assert.Contains(t, body, "team-b", "error names the second candidate group")
}

// Scenario 6: --role narrows the fan-out to members holding the role;
// members with a different role get no row.
func TestMulticast_RoleFilter_NarrowsRecipients(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "mc06-send-bbbb-cccc-000000000001"
	const po = "mc06-popo-bbbb-cccc-000000000002"
	const dev1 = "mc06-dev1-bbbb-cccc-000000000003"
	const dev2 = "mc06-dev2-bbbb-cccc-000000000004"
	f.HaveMember("team", sender)
	f.HaveMemberWithRole("team", po, "PO")
	f.HaveMemberWithRole("team", dev1, "dev")
	f.HaveMemberWithRole("team", dev2, "dev")

	rec := postMessage(t, f, sender, map[string]any{
		"to": "group:team", "body": "devs only", "role": "dev",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeMcast(t, rec)
	assert.ElementsMatch(t, []string{dev1, dev2}, recipientConvIDs(resp),
		"only members with role dev receive the broadcast")

	poRows, err := db.ListAgentMessagesForConv(po, 100)
	require.NoError(t, err)
	assert.Empty(t, poRows, "the PO member is filtered out — no row")
	for _, m := range []string{dev1, dev2} {
		rows, err := db.ListAgentMessagesForConv(m, 100)
		require.NoError(t, err)
		assert.Len(t, rows, 1, "dev member %s got a row", m)
	}
}

// Scenario 7: a --role that matches nobody is a non-error 200 with an
// empty recipient set and no rows written — consistent with an
// empty-group multicast.
func TestMulticast_RoleFilter_MatchesNobody(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "mc07-send-bbbb-cccc-000000000001"
	const dev1 = "mc07-dev1-bbbb-cccc-000000000002"
	f.HaveMember("team", sender)
	f.HaveMemberWithRole("team", dev1, "dev")

	rec := postMessage(t, f, sender, map[string]any{
		"to": "group:team", "body": "anyone?", "role": "qa",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeMcast(t, rec)
	assert.Empty(t, resp.Recipients, "no member matched role qa")

	rows, err := db.ListAgentMessagesForConv(dev1, 100)
	require.NoError(t, err)
	assert.Empty(t, rows, "a filtered-out member gets no row")
}

// Scenario 8: --role on a 1:1 (non-group:) target is a 400 — there is
// no member set to filter.
func TestMulticast_RoleOnDirectTarget_BadRequest(t *testing.T) {
	f := newFlow(t)

	const sender = "mc08-send-bbbb-cccc-000000000001"
	const recip = "mc08-recp-bbbb-cccc-000000000002"
	f.HaveEnrolledAgent(sender)
	f.HaveEnrolledAgent(recip)

	rec := postMessage(t, f, sender, map[string]any{
		"to": recip, "body": "hi", "role": "dev",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "--role")

	rows, err := db.ListAgentMessagesForConv(recip, 100)
	require.NoError(t, err)
	assert.Empty(t, rows, "the rejected send writes no row")
}

// Scenario 9: the --role match is case-insensitive — --role po reaches
// a member whose stored role is PO.
func TestMulticast_RoleFilter_CaseInsensitive(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "mc09-send-bbbb-cccc-000000000001"
	const po = "mc09-popo-bbbb-cccc-000000000002"
	f.HaveMember("team", sender)
	f.HaveMemberWithRole("team", po, "PO")

	rec := postMessage(t, f, sender, map[string]any{
		"to": "group:team", "body": "po, lowercase request", "role": "po",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeMcast(t, rec)
	assert.ElementsMatch(t, []string{po}, recipientConvIDs(resp),
		"--role po matches the member whose stored role is PO")
}

// Scenario 10: a numeric token equal to a real group *name* resolves to
// the named group — name lookup wins over the numeric-id fallback.
func TestMulticast_NumericToken_NamePrecedence(t *testing.T) {
	f := newFlow(t)

	// "other" exists purely so its id collides with the digit-name.
	gOther := f.HaveGroup("other")
	digitName := itoa64(gOther.ID) // a group LITERALLY named e.g. "1"
	f.HaveGroup(digitName)

	const sender = "mc10-send-bbbb-cccc-000000000001"
	const memberNamed = "mc10-name-bbbb-cccc-000000000002"
	const memberOther = "mc10-othr-bbbb-cccc-000000000003"
	f.HaveMember(digitName, sender)
	f.HaveMember(digitName, memberNamed)
	f.HaveMember("other", memberOther)

	rec := postMessage(t, f, sender, map[string]any{"to": "group:" + digitName, "body": "by name"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeMcast(t, rec)
	assert.Equal(t, digitName, resp.ViaGroup, "the digit token resolved by name, not by id")
	assert.ElementsMatch(t, []string{memberNamed}, recipientConvIDs(resp))

	otherRows, err := db.ListAgentMessagesForConv(memberOther, 100)
	require.NoError(t, err)
	assert.Empty(t, otherRows, "the id-collision group was not addressed")
}

// Scenario 11: an owner who is not a member may multicast (the existing
// gate), and --role still narrows the fan-out.
func TestMulticast_OwnerNotMember_WithRoleFilter(t *testing.T) {
	f := newFlow(t)

	g := f.HaveGroup("team")
	const owner = "mc11-ownr-bbbb-cccc-000000000001"
	const dev1 = "mc11-dev1-bbbb-cccc-000000000002"
	const po = "mc11-popo-bbbb-cccc-000000000003"
	f.HaveEnrolledAgent(owner)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, owner, "test"))
	f.HaveMemberWithRole("team", dev1, "dev")
	f.HaveMemberWithRole("team", po, "PO")

	rec := postMessage(t, f, owner, map[string]any{
		"to": "group:team", "body": "owner pings the devs", "role": "dev",
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeMcast(t, rec)
	assert.ElementsMatch(t, []string{dev1}, recipientConvIDs(resp),
		"the owner's role-filtered broadcast reaches only the dev member")

	poRows, err := db.ListAgentMessagesForConv(po, 100)
	require.NoError(t, err)
	assert.Empty(t, poRows, "the PO member is filtered out")
}

// Scenario 12: a token that matches no group name and no group id is a
// 404.
func TestMulticast_UnknownGroup_NotFound(t *testing.T) {
	f := newFlow(t)

	const sender = "mc12-send-bbbb-cccc-000000000001"
	f.HaveEnrolledAgent(sender)

	byName := postMessage(t, f, sender, map[string]any{"to": "group:does-not-exist", "body": "x"})
	require.Equal(t, http.StatusNotFound, byName.Code, "body=%s", byName.Body.String())

	byID := postMessage(t, f, sender, map[string]any{"to": "group:99999", "body": "x"})
	require.Equal(t, http.StatusNotFound, byID.Code, "body=%s", byID.Body.String())
}
