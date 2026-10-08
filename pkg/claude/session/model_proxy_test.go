package session

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestModelProxyBridgeHashOnlyAndForeignBearer(t *testing.T) {
	var hash string
	var bearer string
	var observed bool
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models/bind" {
			var b map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&b))
			hash = b["bearer_hash"]
			require.Len(t, hash, 64)
			require.Equal(t, "main@peer", b["reference"])
			_, _ = io.WriteString(w, `{"reference":"main@peer"}`)
			return
		}
		observed = true
		require.Equal(t, "Bearer "+bearer, r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, `{"type":"message"}`)
	}))
	defer daemon.Close()
	client := &http.Client{Transport: httpProxyTestTransport(func(r *http.Request) (*http.Response, error) {
		r.URL.Scheme = "http"
		r.URL.Host = strings.TrimPrefix(daemon.URL, "http://")
		return http.DefaultTransport.RoundTrip(r)
	})}
	bridge, err := newModelProxyBridge(client, "launch", "main@peer")
	require.NoError(t, err)
	require.NotEqual(t, bridge.bearer, hash)
	bearer = bridge.bearer
	for _, auth := range []string{"Bearer foreign", "Bearer " + bridge.bearer} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "http://local/model/v1/messages", strings.NewReader(`{}`))
		req.Header.Set("Authorization", auth)
		bridge.ServeHTTP(rec, req)
		if strings.Contains(auth, "foreign") {
			require.Equal(t, 403, rec.Code)
			require.False(t, observed)
		} else {
			require.Equal(t, 200, rec.Code)
			require.True(t, observed)
		}
	}
}

func TestModelProxyRefusesCompetingClaudeSettings(t *testing.T) {
	home := testutil.CanonicalTempDir(t)
	cwd := filepath.Join(home, "repo")
	require.NoError(t, os.MkdirAll(filepath.Join(cwd, ".claude"), 0700))
	env := []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + filepath.Join(home, "claude-config")}
	settings := filepath.Join(cwd, ".claude", "settings.json")
	for _, body := range []string{`{"env":{"ANTHROPIC_API_KEY":"hidden-test-value"}}`, `{"apiKeyHelper":"private-helper"}`, `{"env":{"CLAUDE_CODE_USE_BEDROCK":""}}`} {
		require.NoError(t, os.WriteFile(settings, []byte(body), 0600))
		err := validateModelProxySettings(cwd, env)
		require.Error(t, err)
		require.NotContains(t, err.Error(), "hidden-test-value")
		require.NotContains(t, err.Error(), "private-helper")
	}
	require.NoError(t, os.WriteFile(settings, []byte(`{"permissions":{"defaultMode":"default"}}`), 0600))
	require.NoError(t, validateModelProxySettings(cwd, env))
}

func TestModelProxyPassThroughSettingsRefused(t *testing.T) {
	for _, args := range [][]string{{"--settings", "private.json"}, {"--settings={}"}} {
		require.Error(t, validateModelProxyExtraArgs(args))
	}
	require.NoError(t, validateModelProxyExtraArgs([]string{"--debug"}))
}
