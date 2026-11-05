package agentd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The operator grants sudo proactively from two surfaces: the CLI
// (`tclaude agent sudo request --target <conv>`, POST /v1/sudo with a target)
// and the dashboard (POST /api/sudo with an agent_id). Both apply the same
// grant policy before writing anything; they differ in how the target is
// named, in the dashboard-only scopes/uses/carry options, and in wire format.

type sudoSurface struct {
	name  string
	grant func(t *testing.T, convID, agentID string, slugs []string) *httptest.ResponseRecorder
}

func sudoSurfaces() []sudoSurface {
	return []sudoSurface{
		{
			name: "socket",
			grant: func(t *testing.T, convID, _ string, slugs []string) *httptest.ResponseRecorder {
				body := `{"target":"` + convID + `","slugs":["` + strings.Join(slugs, `","`) + `"],"duration":"5m"}`
				r := httptest.NewRequest(http.MethodPost, "/v1/sudo", strings.NewReader(body))
				r = r.WithContext(context.WithValue(r.Context(), peerKey{}, &peer{PID: 999, HumanTokenValid: true}))
				w := httptest.NewRecorder()
				handleSudoRequest(w, r)
				return w
			},
		},
		{
			name: "dashboard",
			grant: func(t *testing.T, _, agentID string, slugs []string) *httptest.ResponseRecorder {
				body := `{"agent_id":"` + agentID + `","slugs":["` + strings.Join(slugs, `","`) + `"],"duration":"5m"}`
				w := httptest.NewRecorder()
				handleDashboardSudoAPI(w, dashboardRequest(http.MethodPost, "/api/sudo", body))
				return w
			},
		},
	}
}

func seedSudoTarget(t *testing.T, convID, title string) string {
	t.Helper()
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{ConvID: convID, CustomTitle: title, IndexedAt: time.Now()}))
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	return agentID
}

func TestSudoProactive_BothSurfacesApplyTheSamePolicy(t *testing.T) {
	for _, s := range sudoSurfaces() {
		t.Run(s.name, func(t *testing.T) {
			setupTestDB(t)
			withDashboardAuth(t)
			const conv = "sudo-aaaa-bbbb-cccc-1111"
			agentID := seedSudoTarget(t, conv, "alice")

			// A blocklisted slug refuses the whole grant.
			rec := s.grant(t, conv, agentID, []string{"groups.members.spawn", "permissions.grant"})
			require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "permissions.grant")
			rows, err := db.ListActiveSudoGrants(conv)
			require.NoError(t, err)
			require.Empty(t, rows)

			// An unknown slug would save a grant nothing ever checks.
			rec = s.grant(t, conv, agentID, []string{"groups.members.spawn", "no.such.slug"})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "no.such.slug")
			rows, err = db.ListActiveSudoGrants(conv)
			require.NoError(t, err)
			require.Empty(t, rows)

			rec = s.grant(t, conv, agentID, []string{"groups.members.spawn"})
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			rows, err = db.ListActiveSudoGrants(conv)
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Equal(t, "groups.members.spawn", rows[0].Slug)
		})
	}
}

// An external steward cannot hold sudo: the permission evaluator ignores its
// sudo rows, so a grant would report success and never take effect. Both
// surfaces refuse it instead of saving it.
func TestSudoProactive_BothSurfacesRefuseAnExternalSteward(t *testing.T) {
	for _, s := range sudoSurfaces() {
		t.Run(s.name, func(t *testing.T) {
			setupTestDB(t)
			withDashboardAuth(t)
			conv := db.ExternalStewardConvPrefix + "sudo-target"
			agentID := seedSudoTarget(t, conv, "outside-helper")

			rec := s.grant(t, conv, agentID, []string{"groups.members.spawn"})
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), "external steward")
			rows, err := db.ListActiveSudoGrants(conv)
			require.NoError(t, err)
			require.Empty(t, rows)
		})
	}
}
