package transport

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestProcessSnippetHTTPRejectsMalformedAndUnauthenticatedWrites(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	defer store.Close()
	h := testHandler(t, app.New(store, providers.NewRegistry()))
	body := `{"request_id":"create","action":"create","name":"Fragment","selection":{"version":1,"nodes":[{"ID":"n","Kind":"end"}],"edges":[],"positions":{}},"expected_revision":0}`
	require.Equal(t, 401, request(h, "POST", "/v2/process-snippets/snippet", body, "").Code)
	require.Equal(t, 401, request(h, "GET", "/v2/process-snippets", "", "").Code)
	r := request(h, "POST", "/v2/process-snippets/snippet", body, testCredential)
	require.Equal(t, 200, r.Code, r.Body.String())
	r = request(h, "POST", "/v2/process-snippets/snippet", body, testCredential)
	require.Equal(t, 200, r.Code, r.Body.String())
	r = request(h, "POST", "/v2/process-snippets/snippet", `{"request_id":"rename","action":"rename","name":"Renamed","expected_revision":4}`, testCredential)
	require.Equal(t, 409, r.Code, r.Body.String())
	r = request(h, "POST", "/v2/process-snippets/invalid", `{"request_id":"invalid","action":"create","name":"Fragment","selection":{"version":1,"nodes":[{"ID":"n","Kind":"end"}],"edges":null,"positions":{}}}`, testCredential)
	require.Equal(t, 422, r.Code, r.Body.String())
}
