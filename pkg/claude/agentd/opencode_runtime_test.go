package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

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
		PID: os.Getpid(), ServerURL: server.URL,
		Password: password, Cwd: "/tmp/project",
	}
	assert.True(t, openCodeHealthy(runtime))
	convID, err := createOpenCodeSession(runtime, "worker")
	require.NoError(t, err)
	assert.Equal(t, "ses_test123", convID)
	assert.True(t, sawCreate)
}

func TestOpenCodeHealthRequiresManagedListenerAndHealthyBody(t *testing.T) {
	const password = "private-password"
	healthCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		healthCalls++
		user, pass, ok := r.BasicAuth()
		require.True(t, ok)
		assert.Equal(t, openCodeServerUsername, user)
		assert.Equal(t, password, pass)
		if healthCalls < 3 {
			http.Error(w, "warming up", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"healthy":true}`))
	}))
	defer server.Close()

	runtime := db.OpenCodeRuntime{
		PID: os.Getpid(), ServerURL: server.URL, Password: password,
	}
	assert.True(t, openCodeProcessOwnsEndpoint(runtime.PID, runtime.ServerURL))
	const foreignPID = 99_999_999
	assert.False(t, openCodeProcessOwnsEndpoint(foreignPID, runtime.ServerURL))
	foreignRuntime := runtime
	foreignRuntime.PID = foreignPID
	assert.False(t, openCodeHealthyAfterRetries(foreignRuntime, 1, 0))
	_, err := createOpenCodeSession(foreignRuntime, "must-not-send")
	require.Error(t, err)
	err = sendOpenCodePrompt(&openCodeLaunch{
		ConvID: "ses_test", ServerURL: foreignRuntime.ServerURL,
		Password: foreignRuntime.Password, PID: foreignPID,
	}, "/tmp/project", "must-not-send", "", "")
	require.Error(t, err)
	assert.Zero(t, healthCalls, "credentials must not be sent to a listener owned by another PID")
	assert.True(t, openCodeHealthyAfterRetries(runtime, 3, time.Millisecond))
	assert.Equal(t, 3, healthCalls)

	bareOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer bareOK.Close()
	assert.False(t, openCodeHealthy(db.OpenCodeRuntime{
		PID: os.Getpid(), ServerURL: bareOK.URL, Password: password,
	}))
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
		ConvID: "ses_test", ServerURL: server.URL,
		Password: password, PID: os.Getpid(),
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
