package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These tests pin the property that `tclaude agent permissions ls
// <target>` and the daemon's actual permission gate cannot disagree:
// both now run through resolvePermissionVerdict, so every scenario below
// asserts the LISTING and a REAL gated endpoint together. The listing
// previously re-derived its own union of (defaults ∪ per-conv grants ∪
// owner-implied) and therefore silently omitted two standing sources the
// gate honours — group grants and sudo elevations — so an agent could
// hold a slug that `permissions ls` swore it did not have.
//
// groups.create is the probe slug throughout: it is gated by
// requirePermission on POST /v1/groups and is NOT owner-implied, so the
// structural owner bypass cannot mask a wrong answer.

// effectiveFor reads the daemon's effective view for convID as that same
// agent — the sandboxed-caller shape the CLI uses.
func effectiveViewFor(t *testing.T, f *testharness.Flow, convID string) effectiveViewWithProvenance {
	t.Helper()
	r := getPermissionsTarget(t, f, convID, convID)
	require.Equal(t, http.StatusOK, r.Code, "GET /v1/permissions?target: %s", r.Body)
	var v effectiveViewWithProvenance
	require.NoError(t, json.Unmarshal([]byte(r.Body), &v), "decode effective view: %s", r.Body)
	return v
}

// effectiveViewWithProvenance extends the wire view with the per-slug
// provenance map the daemon now returns.
type effectiveViewWithProvenance struct {
	Effective    []string          `json:"effective"`
	Source       string            `json:"source"`
	OwnerImplied []string          `json:"owner_implied"`
	OwnedGroups  []string          `json:"owned_groups"`
	Provenance   map[string]string `json:"provenance"`
}

// Scenario: the slug is granted by a GROUP the agent belongs to — no
// default, no per-agent override. The gate allows it; the listing must
// say so too, and name the group as the source.
func TestEffectivePerms_GroupGrantedSlugIsListedAndGated(t *testing.T) {
	f := newFlow(t)

	const member = "epgs-aaaa-bbbb-cccc-0001"
	f.HaveConvWithTitle(member, "group-member")
	f.HaveEnrolledAgent(member)
	group := f.HaveGroup("perm-src-group")
	f.HaveMember("perm-src-group", member)

	require.NoError(t,
		db.ReplaceAgentGroupPermissions(group.ID, []string{agentd.PermGroupsCreate}, "test"),
		"grant groups.create to the group")

	// The gate allows it — this is the ground truth the listing must match.
	assert.Equal(t, http.StatusCreated, agentCreatesGroup(t, f, member, "made-by-member"),
		"group-granted slug must pass the real gate")

	view := effectiveViewFor(t, f, member)
	assert.Contains(t, view.Effective, agentd.PermGroupsCreate,
		"a slug the gate allows must appear in the effective listing")
	assert.Equal(t, "group", view.Provenance[agentd.PermGroupsCreate],
		"provenance must name the group as the granting source")
	assert.Contains(t, view.Source, "+group", "source label must note the group input")
	assert.NotContains(t, view.OwnerImplied, agentd.PermGroupsCreate,
		"a group grant is not the owner bypass")
}

// Scenario: a per-agent deny sits on top of a group grant. Deny is
// authoritative for the gate, so the listing must drop the slug too —
// the two must not disagree in either direction.
func TestEffectivePerms_DenyOverGroupGrantAgreesWithGate(t *testing.T) {
	f := newFlow(t)

	const member = "epds-aaaa-bbbb-cccc-0001"
	f.HaveConvWithTitle(member, "denied-member")
	f.HaveEnrolledAgent(member)
	group := f.HaveGroup("perm-deny-group")
	f.HaveMember("perm-deny-group", member)
	require.NoError(t,
		db.ReplaceAgentGroupPermissions(group.ID, []string{agentd.PermGroupsCreate}, "test"))

	permMutate(t, f, "deny", member, agentd.PermGroupsCreate)

	assert.Equal(t, http.StatusForbidden, agentCreatesGroup(t, f, member, "should-not-exist"),
		"an explicit deny must beat the group grant at the gate")

	view := effectiveViewFor(t, f, member)
	assert.NotContains(t, view.Effective, agentd.PermGroupsCreate,
		"a slug the gate refuses must not be listed as effective")
	assert.Contains(t, view.Source, "−denies", "source label must note the deny")
}

// Scenario: an active sudo elevation outranks even a standing deny. The
// gate honours it, so the listing must show the slug — and attribute it
// to sudo, not to a permanent grant the operator never made.
func TestEffectivePerms_SudoElevationIsListedAndGated(t *testing.T) {
	f := newFlow(t)

	const elevated = "epsu-aaaa-bbbb-cccc-0001"
	f.HaveConvWithTitle(elevated, "elevated-agent")
	f.HaveEnrolledAgent(elevated)

	// A standing deny that sudo must outrank, proving the listing follows
	// the resolver's precedence rather than a re-derived one.
	permMutate(t, f, "deny", elevated, agentd.PermGroupsCreate)

	now := time.Now()
	_, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID:    elevated,
		Slug:      agentd.PermGroupsCreate,
		GrantedAt: now,
		ExpiresAt: now.Add(10 * time.Minute),
		GrantedBy: "human:test",
	})
	require.NoError(t, err, "insert sudo grant")

	assert.Equal(t, http.StatusCreated, agentCreatesGroup(t, f, elevated, "made-under-sudo"),
		"sudo must outrank the deny at the gate")

	view := effectiveViewFor(t, f, elevated)
	assert.Contains(t, view.Effective, agentd.PermGroupsCreate,
		"a slug the gate allows via sudo must appear in the effective listing")
	assert.Equal(t, "sudo", view.Provenance[agentd.PermGroupsCreate],
		"provenance must attribute the slug to the elevation, not a standing grant")
	assert.Contains(t, view.Source, "+sudo", "source label must note the elevation")
}

// Scenario: the ordinary paths keep working — a config default and a
// per-agent grant each land in the listing with the right provenance,
// and both pass the gate.
func TestEffectivePerms_DefaultAndPerAgentGrantProvenance(t *testing.T) {
	f := newFlow(t)

	const byDefault = "epdf-aaaa-bbbb-cccc-0001"
	const byGrant = "epdf-aaaa-bbbb-cccc-0002"
	f.HaveConvWithTitle(byDefault, "default-holder")
	f.HaveEnrolledAgent(byDefault)
	f.HaveConvWithTitle(byGrant, "grant-holder")
	f.HaveEnrolledAgent(byGrant)

	permMutate(t, f, "grant", "default", agentd.PermSelfRename)
	permMutate(t, f, "grant", byGrant, agentd.PermGroupsCreate)

	defaultView := effectiveViewFor(t, f, byDefault)
	assert.Contains(t, defaultView.Effective, agentd.PermSelfRename)
	assert.Equal(t, "default", defaultView.Provenance[agentd.PermSelfRename],
		"a config default must be attributed to the defaults list")

	assert.Equal(t, http.StatusCreated, agentCreatesGroup(t, f, byGrant, "made-by-grantee"),
		"a per-agent grant must pass the gate")
	grantView := effectiveViewFor(t, f, byGrant)
	assert.Contains(t, grantView.Effective, agentd.PermGroupsCreate)
	assert.Equal(t, "override", grantView.Provenance[agentd.PermGroupsCreate],
		"a per-agent grant must be attributed to the override row")
	assert.Contains(t, grantView.Source, "+grants:", "source label must name the grant input")
}

// Scenario: the dashboard roster's Effective column is the surface whose
// semantics changed most — it used to be its own union that omitted sudo
// elevations. Pin it to the gate too, or it can drift back.
func TestDashboardEffectiveColumn_MatchesGate(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)

	const elevated = "dsef-aaaa-bbbb-cccc-0001"
	f.HaveConvWithTitle(elevated, "snapshot-agent")
	f.HaveEnrolledAgent(elevated)

	now := time.Now()
	_, err := db.InsertSudoGrant(&db.SudoGrant{
		ConvID:    elevated,
		Slug:      agentd.PermGroupsCreate,
		GrantedAt: now,
		ExpiresAt: now.Add(10 * time.Minute),
		GrantedBy: "human:test",
	})
	require.NoError(t, err, "insert sudo grant")

	require.Equal(t, http.StatusCreated, agentCreatesGroup(t, f, elevated, "made-under-sudo"),
		"precondition: the gate honours the elevation")

	snap := fetchPermSnapshot(t, agentd.BuildDashboardHandlerForTest())
	assert.Contains(t, effectiveFor(snap, elevated), agentd.PermGroupsCreate,
		"the roster's Effective column must show a slug the gate currently allows")
}

// Scenario: the untargeted roster must disclose group grants. Without
// them the operator reads DEFAULTS + PER-AGENT OVERRIDES as the whole
// permission picture while a group quietly confers more.
func TestPermissionsRoster_DisclosesGroupGrants(t *testing.T) {
	f := newFlow(t)

	group := f.HaveGroup("roster-grant-group")
	require.NoError(t,
		db.ReplaceAgentGroupPermissions(group.ID, []string{agentd.PermHumanNotify}, "test"))

	rec := testharness.Serve(f.Mux,
		agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/permissions", nil)))
	require.Equal(t, http.StatusOK, rec.Code, "GET /v1/permissions: %s", rec.Body.String())

	var state struct {
		GroupGrants map[string][]string `json:"group_grants"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &state), "decode roster")
	assert.Equal(t, []string{agentd.PermHumanNotify}, state.GroupGrants["roster-grant-group"],
		"the roster must name the slugs a group confers on its members")
}

// Scenario: an owner-conferred slug must say WHERE it applies. The gate
// scopes the owner bypass to owned groups (or their members) for most
// slugs, and not at all for the ownsAnyGroup pair — so a listing that
// says only "via ownership" claims authority the gate refuses.
func TestEffectivePerms_OwnerConferredSlugsCarryTheirScope(t *testing.T) {
	f := newFlow(t)

	const owner = "epos-aaaa-bbbb-cccc-0001"
	f.HaveConvWithTitle(owner, "squad-lead")
	f.HaveEnrolledAgent(owner)
	for _, name := range []string{"squad-b", "squad-a"} {
		g := f.HaveGroup(name)
		require.NoError(t, db.AddAgentGroupOwner(g.ID, owner, "test"), "seed owner of "+name)
	}

	view := effectiveViewFor(t, f, owner)

	assert.Equal(t, []string{"squad-a", "squad-b"}, view.OwnedGroups,
		"the response names the owned groups, sorted, so a client need not read the DB")

	// groups.spawn reaches the owned groups themselves...
	assert.Equal(t, "owner:group", view.Provenance[agentd.PermGroupsSpawn],
		"a requireGroupPermission slug is group-scoped")
	// ...agent.rename reaches their members...
	assert.Equal(t, "owner:member", view.Provenance[agentd.PermAgentRename],
		"a requireCrossAgentPermission slug is member-scoped")
	// ...and human.notify is genuinely unscoped (ownsAnyGroup).
	assert.Equal(t, "owner:any", view.Provenance[agentd.PermHumanNotify],
		"an ownsAnyGroup slug must not claim a per-group scope")
}

// Scenario: an archived group confers nothing — its endpoints reject
// mutation — so naming it as the scope would promise reach the owner
// does not have.
func TestEffectivePerms_OwnedGroupsExcludeArchived(t *testing.T) {
	f := newFlow(t)

	const owner = "epoa-aaaa-bbbb-cccc-0001"
	f.HaveConvWithTitle(owner, "archive-lead")
	f.HaveEnrolledAgent(owner)
	live := f.HaveGroup("still-live")
	gone := f.HaveGroup("archived-squad")
	require.NoError(t, db.AddAgentGroupOwner(live.ID, owner, "test"))
	require.NoError(t, db.AddAgentGroupOwner(gone.ID, owner, "test"))
	require.NoError(t, db.ArchiveAgentGroup(gone.Name), "archive the group")

	view := effectiveViewFor(t, f, owner)
	assert.Equal(t, []string{"still-live"}, view.OwnedGroups,
		"an archived group must not be named as an owner scope")
}
