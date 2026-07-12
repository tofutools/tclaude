//go:build linux

package agentd

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDashboardSandboxProfileDirectoryCreationNeedsNoAncestorReadPermission(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	parent := filepath.Join(t.TempDir(), "write-search-only")
	require.NoError(t, os.Mkdir(parent, 0o300))
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
	missing := filepath.Join(parent, "cache")
	body := `{"filesystem":[{"path":"` + missing + `","access":"write"}]}`

	rec := httptest.NewRecorder()
	serveDashboardSandboxProfiles(rec, dashboardRequest(http.MethodPost, "/api/sandbox-profile-directories/create", body))
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	info, err := os.Stat(missing)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}
