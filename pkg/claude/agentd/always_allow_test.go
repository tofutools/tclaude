package agentd

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// resetTestDB points the DB at a fresh temp home and resets the singleton,
// mirroring the setup the internal audit test uses.
func resetTestDB(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	db.ResetForTest()
}

// TestPermissionRegistry_AutoGrantableSet pins the EXACT set of slugs the
// popup's "Always allow for this agent" button may persist. Keep it small
// and low-blast-radius: only the human-machine-surface channels. A drift
// here means the popup would offer a one-click permanent grant for a slug
// it shouldn't (or hide it for one it should).
//
// If you add/remove an AutoGrantable slug, update BOTH the registry flag
// and this list. Destructive / fleet-affecting slugs (agent.delete,
// groups.rm, permissions.*) must never appear here.
func TestPermissionRegistry_AutoGrantableSet(t *testing.T) {
	want := []string{PermHumanClipboard, PermHumanNotify}
	got := AutoGrantableSlugs()
	assert.ElementsMatch(t, want, got, "auto-grantable slug set drifted")

	for _, s := range got {
		assert.True(t, IsKnownPermSlug(s), "auto-grantable slug %q is not registered", s)
		assert.True(t, IsAutoGrantableSlug(s), "IsAutoGrantableSlug(%q) = false, want true", s)
	}
	// A clearly-destructive slug must never be auto-grantable.
	assert.False(t, IsAutoGrantableSlug(PermAgentDelete),
		"agent.delete must never be auto-grantable")
	assert.False(t, IsAutoGrantableSlug("groups.rm"),
		"groups.rm must never be auto-grantable")
	// An unknown slug is not auto-grantable.
	assert.False(t, IsAutoGrantableSlug("no.such.slug"))
}

// TestRenderApprovalPage_AlwaysButtonGatedOnEligibility proves the popup
// renders the "Always allow for this agent" button ONLY when the requested
// slug is auto-grantable.
func TestRenderApprovalPage_AlwaysButtonGatedOnEligibility(t *testing.T) {
	// newReq builds a fresh request (approvalRequest carries a sync.Mutex,
	// so it must not be copied by value).
	newReq := func(perm string, autoGrantable bool) *approvalRequest {
		return &approvalRequest{
			id:            "appr01",
			convID:        "cccc-1111",
			convTitle:     "worker",
			method:        "POST",
			path:          "/v1/clipboard",
			perm:          perm,
			autoGrantable: autoGrantable,
			timeout:       30 * time.Second,
		}
	}

	// Eligible slug → the always form + label are present.
	rec := httptest.NewRecorder()
	renderApprovalPage(rec, newReq(PermHumanClipboard, true))
	body := rec.Body.String()
	assert.Contains(t, body, "/approve/appr01/always", "eligible slug must render the always form")
	assert.Contains(t, body, "Always allow for this agent")

	// Ineligible slug → no always affordance at all.
	rec = httptest.NewRecorder()
	renderApprovalPage(rec, newReq("agent.delete", false))
	body = rec.Body.String()
	assert.NotContains(t, body, "/approve/appr01/always", "ineligible slug must NOT render the always form")
	assert.NotContains(t, body, "Always allow for this agent")
}

// TestPersistAlwaysAllowGrant_RefusesIneligible is the defense-in-depth
// check: even if something drives an always-outcome for an ineligible slug
// (bypassing the button + the POST gate), the persist itself refuses, so
// no override is written.
func TestPersistAlwaysAllowGrant_RefusesIneligible(t *testing.T) {
	resetTestDB(t)

	const conv = "aaaa-1111-2222-3333-4444"
	req := &approvalRequest{perm: PermAgentDelete, convID: conv}
	persistAlwaysAllowGrant(req)

	effect, ok, err := db.AgentPermissionOverride(conv, PermAgentDelete)
	assert.NoError(t, err)
	assert.False(t, ok, "an ineligible slug must not be persisted (effect=%q)", effect)
}

// TestPersistAlwaysAllowGrant_WritesEligible confirms the happy path: an
// eligible slug is persisted as an allow override keyed on the agent.
func TestPersistAlwaysAllowGrant_WritesEligible(t *testing.T) {
	resetTestDB(t)

	const conv = "bbbb-1111-2222-3333-4444"
	req := &approvalRequest{perm: PermHumanClipboard, convID: conv}
	persistAlwaysAllowGrant(req)

	effect, ok, err := db.AgentPermissionOverride(conv, PermHumanClipboard)
	assert.NoError(t, err)
	assert.True(t, ok, "an eligible slug must be persisted")
	assert.Equal(t, "grant", effect)
}
