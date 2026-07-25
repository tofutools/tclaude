package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The owner-implied run-read boundary (TCL-722). A group owner coordinating a
// process validation reads run status and evidence without a per-read human
// approval; nobody gains mutation authority from ownership, and an explicit
// deny still wins. These tests drive the production mux, so they pin the
// boundary at the daemon's real gates rather than at the registry flag.

// runOwnerRead is the parked run fixture id these tests seed.
const runOwnerRead = "run_owner_read"

// processRunReadRoutes are the three read-only run surfaces gated on
// process.runs.read.
var processRunReadRoutes = []struct {
	name string
	path string
}{
	{"list", "/v1/process/runs"},
	{"show", "/v1/process/runs/" + runOwnerRead},
	{"events", "/v1/process/runs/" + runOwnerRead + "/events"},
}

// seedProcessRunForOwnerRead installs a template plus one parked run fixture
// so show/events have a real row to answer for. No sweep is triggered, so the
// run never dispatches a program.
func seedProcessRunForOwnerRead(t *testing.T) *testharness.Flow {
	t.Helper()
	f, root := processRuntimeFlow(t)
	tmpl := processRuntimeTemplate("owner-read", 1)
	record := putProcessRuntimeTemplate(t, root, tmpl)
	createRunnableProcessRunFixture(t, runOwnerRead, record.Ref, tmpl)
	return f
}

// Scenario: an agent that owns a group reads every run surface with no
// process.runs.read grant anywhere — ownership alone carries it, the same
// structural bypass human.notify uses.
func TestProcessRunsGroupOwnerReadsWithoutGrant(t *testing.T) {
	f := seedProcessRunForOwnerRead(t)

	const ownerConv = "prro-1111-2222-3333-4444"
	g := f.HaveGroup("process-owners")
	f.HaveMember("process-owners", ownerConv)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, ownerConv, "test"))

	for _, route := range processRunReadRoutes {
		rec := agentReq(t, f, ownerConv, http.MethodGet, route.path, nil)
		require.Equalf(t, http.StatusOK, rec.Code,
			"%s: a group owner should read runs without the slug; body=%s", route.name, rec.Body.String())
	}

	list := agentReq(t, f, ownerConv, http.MethodGet, "/v1/process/runs", nil)
	var listed struct {
		Runs []processRuntimeRunView `json:"runs"`
	}
	testharness.DecodeJSON(t, list, &listed)
	require.Len(t, listed.Runs, 1, "the owner sees the seeded run")
	assert.Equal(t, runOwnerRead, listed.Runs[0].ID)
}

// Scenario: ownership confers the READ only. Every mutating run verb still
// demands process.runs.manage, and template authoring still demands
// process.templates.manage — an owner gets neither for free.
func TestProcessRunsGroupOwnerGainsNoMutationAuthority(t *testing.T) {
	f := seedProcessRunForOwnerRead(t)

	const ownerConv = "prrm-1111-2222-3333-4444"
	g := f.HaveGroup("process-owners")
	f.HaveMember("process-owners", ownerConv)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, ownerConv, "test"))

	for _, route := range []struct {
		name string
		path string
		body any
	}{
		{"create", "/v1/process/runs", map[string]any{"templateId": "owner-read"}},
		{"resume", "/v1/process/runs/" + runOwnerRead + "/resume", map[string]any{}},
		{"reissue", "/v1/process/runs/" + runOwnerRead + "/reissue", map[string]any{}},
		{"record-outcome", "/v1/process/runs/" + runOwnerRead + "/record-outcome",
			map[string]any{"outcome": "succeeded"}},
		{"decide", "/v1/process/runs/" + runOwnerRead + "/decide",
			map[string]any{"nodeId": "choose", "verdict": "approve"}},
	} {
		rec := agentReq(t, f, ownerConv, http.MethodPost, route.path, route.body)
		require.Equalf(t, http.StatusForbidden, rec.Code,
			"%s must stay gated on %s for an owner; body=%s",
			route.name, agentd.PermProcessRunsManage, rec.Body.String())
		assert.Containsf(t, rec.Body.String(), agentd.PermProcessRunsManage,
			"%s: the 403 should name the missing mutation slug", route.name)
	}

	save := agentReq(t, f, ownerConv, http.MethodPost, "/v1/process/templates",
		map[string]any{"id": "owner-authored", "yaml": "apiVersion: v1\n"})
	require.Equal(t, http.StatusForbidden, save.Code,
		"template authoring must stay gated for an owner; body=%s", save.Body.String())
	assert.Contains(t, save.Body.String(), agentd.PermProcessTemplatesManage)
}

// Scenario: a plain group member is NOT an owner and gets no run read. The
// ordinary defaults carry process.templates.read, never process.runs.read.
func TestProcessRunsPlainMemberForbidden(t *testing.T) {
	f := seedProcessRunForOwnerRead(t)

	const memberConv = "prrp-1111-2222-3333-4444"
	f.HaveGroup("process-workers")
	f.HaveMember("process-workers", memberConv) // a plain member, not an owner

	for _, route := range processRunReadRoutes {
		rec := agentReq(t, f, memberConv, http.MethodGet, route.path, nil)
		require.Equalf(t, http.StatusForbidden, rec.Code,
			"%s: a non-owner member must not read runs; body=%s", route.name, rec.Body.String())
		assert.Containsf(t, rec.Body.String(), agentd.PermProcessRunsRead,
			"%s: the 403 should name the missing slug", route.name)
	}
}

// Scenario: an owner carrying an explicit DENY override on process.runs.read
// is refused on every read surface — deny is authoritative and suppresses the
// structural owner grant, which only fills the undecided gap.
func TestProcessRunsDenyOverrideBeatsGroupOwner(t *testing.T) {
	f := seedProcessRunForOwnerRead(t)

	const ownerConv = "prrd-1111-2222-3333-4444"
	g := f.HaveGroup("process-owners")
	f.HaveMember("process-owners", ownerConv)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, ownerConv, "test"))
	require.NoError(t,
		db.SetAgentPermissionOverride(ownerConv, agentd.PermProcessRunsRead, db.PermEffectDeny, "test"),
		"seed deny override")

	for _, route := range processRunReadRoutes {
		rec := agentReq(t, f, ownerConv, http.MethodGet, route.path, nil)
		require.Equalf(t, http.StatusForbidden, rec.Code,
			"%s: a deny override must beat the owner grant; body=%s", route.name, rec.Body.String())
	}
}

// Scenario: the effective-permission listing an agent or the human
// introspects reports process.runs.read as held via ownership, and reports it
// gone once a deny override lands. The daemon's gate and its own reporting of
// that gate must agree, or the dashboard/CLI lies about what an owner can do.
func TestProcessRunsOwnerEffectiveListingAnnotatesRunRead(t *testing.T) {
	f := seedProcessRunForOwnerRead(t)

	const ownerConv = "prre-1111-2222-3333-4444"
	g := f.HaveGroup("process-owners")
	f.HaveMember("process-owners", ownerConv)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, ownerConv, "test"))

	granted := effectivePermsForConv(t, f, ownerConv)
	assert.Contains(t, granted.Effective, agentd.PermProcessRunsRead,
		"an owner effectively holds the run read")
	assert.Contains(t, granted.OwnerImplied, agentd.PermProcessRunsRead,
		"and it is annotated as coming from ownership")
	assert.NotContains(t, granted.Effective, agentd.PermProcessRunsManage,
		"ownership never widens into run mutation")
	assert.NotContains(t, granted.Effective, agentd.PermProcessTemplatesManage,
		"ownership never widens into template authoring")

	require.NoError(t,
		db.SetAgentPermissionOverride(ownerConv, agentd.PermProcessRunsRead, db.PermEffectDeny, "test"))
	denied := effectivePermsForConv(t, f, ownerConv)
	assert.NotContains(t, denied.Effective, agentd.PermProcessRunsRead,
		"a deny override drops it from the effective set")
	assert.NotContains(t, denied.OwnerImplied, agentd.PermProcessRunsRead,
		"and from the owner-conferred projection")
}

type effectivePermsView struct {
	Effective    []string `json:"effective"`
	OwnerImplied []string `json:"owner_implied"`
}

func effectivePermsForConv(t *testing.T, f *testharness.Flow, convID string) effectivePermsView {
	t.Helper()
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(
		testharness.JSONRequest(t, http.MethodGet, "/v1/permissions?target="+convID, nil)))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var out effectivePermsView
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "decode effective permissions")
	return out
}
