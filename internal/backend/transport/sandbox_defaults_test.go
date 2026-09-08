package transport

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestPublicSandboxDefaultsRequireExplicitAssignments(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	handler := testHandler(t, app.New(store, providers.NewRegistry()))
	profile := request(handler, "POST", "/v2/sandbox-profiles", `{"request_id":"profile","id":"sandbox_profile","name":"Shared sandbox","policy":{}}`, testCredential)
	require.Equal(t, 200, profile.Code, profile.Body.String())
	saved := request(handler, "POST", "/v2/sandbox-defaults", `{"request_id":"assign","expected_revision":0,"global":"sandbox_profile","groups":{}}`, testCredential)
	require.Equal(t, 200, saved.Code, saved.Body.String())
	before := request(handler, "GET", "/v2/sandbox-defaults", "", testCredential)
	require.Equal(t, 200, before.Code)
	for _, body := range []string{
		`{"request_id":"clear","expected_revision":1,"groups":{}}`,
		`{"request_id":"clear","expected_revision":1,"global":null,"groups":{}}`,
		`{"request_id":"clear","expected_revision":1,"global":""}`,
		`{"request_id":"clear","expected_revision":1,"global":"","groups":null}`,
	} {
		rejected := request(handler, "POST", "/v2/sandbox-defaults", body, testCredential)
		require.Equal(t, 422, rejected.Code, rejected.Body.String())
		after := request(handler, "GET", "/v2/sandbox-defaults", "", testCredential)
		require.JSONEq(t, before.Body.String(), after.Body.String())
	}
	clear := request(handler, "POST", "/v2/sandbox-defaults", `{"request_id":"clear","expected_revision":1,"global":"","groups":{}}`, testCredential)
	require.Equal(t, 200, clear.Code, clear.Body.String())
	require.JSONEq(t, `{"Global":"","Groups":{},"Revision":2}`, clear.Body.String())
}
