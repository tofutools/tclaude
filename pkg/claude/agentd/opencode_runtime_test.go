package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestOpenCodeControlPlaneUsesBasicAuthAndMintsSession(t *testing.T) {
	const password = "private-password"
	var sawCreate bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != openCodeServerUsername || pass != password {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/global/health":
			_, _ = w.Write([]byte(`{"healthy":true}`))
		case "/session":
			sawCreate = true
			assert.Equal(t, "/tmp/project", r.URL.Query().Get("directory"))
			var body map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, "worker", body["title"])
			_, _ = w.Write([]byte(`{"id":"ses_test123"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	runtime := db.OpenCodeRuntime{
		ServerURL: server.URL, Password: password, Cwd: "/tmp/project",
	}
	assert.True(t, openCodeHealthy(runtime))
	convID, err := createOpenCodeSession(runtime, "worker")
	require.NoError(t, err)
	assert.Equal(t, "ses_test123", convID)
	assert.True(t, sawCreate)
}

func TestOpenCodeLaunchPromptCarriesModelAndVariant(t *testing.T) {
	const password = "private-password"
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		require.True(t, ok)
		assert.Equal(t, openCodeServerUsername, user)
		assert.Equal(t, password, pass)
		assert.Equal(t, "/session/ses_test/prompt_async", r.URL.Path)
		assert.Equal(t, "/tmp/project", r.URL.Query().Get("directory"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	err := sendOpenCodePrompt(&openCodeLaunch{
		ConvID: "ses_test", ServerURL: server.URL, Password: password,
	}, "/tmp/project", "startup brief", "openai/gpt-5.6-terra", "high")
	require.NoError(t, err)
	assert.Equal(t, "high", body["variant"])
	assert.Equal(t, map[string]any{
		"providerID": "openai", "modelID": "gpt-5.6-terra",
	}, body["model"])
	parts := body["parts"].([]any)
	assert.Equal(t, "startup brief", parts[0].(map[string]any)["text"])
}

func TestRandomOpenCodePassword(t *testing.T) {
	first, err := randomOpenCodePassword()
	require.NoError(t, err)
	second, err := randomOpenCodePassword()
	require.NoError(t, err)
	assert.Len(t, first, 43)
	assert.NotEqual(t, first, second)
}

func TestOpenCodeCredentialHandoffNeverEntersWrapperArgv(t *testing.T) {
	args := sessionNewArgs(clcommon.SpawnArgs{
		Label:                  "spwn-test",
		Harness:                "opencode",
		OpenCodeServerURL:      "http://127.0.0.1:43210",
		OpenCodeServerPassword: "top-secret",
	})
	joined := strings.Join(args, " ")
	assert.NotContains(t, joined, "top-secret")
	assert.NotContains(t, joined, "43210")
}
