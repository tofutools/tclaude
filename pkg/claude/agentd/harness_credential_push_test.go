package agentd

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/harnessops"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestHarnessCredentialPushCapturesOnlyExplicitLocalSource(t *testing.T) {
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	require.NoError(t, os.Mkdir(filepath.Join(home, ".codex"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte(`{"token":"test-fixture-secret"}`), 0600))
	r := httptest.NewRequest("POST", "/api/peer/pinned/harnesses/operations", nil)
	r.SetPathValue("tail", "harnesses/operations")
	normal := []byte(`{"action":"install","harness":"codex"}`)
	out, err := prepareHarnessCredentialPush(r, normal)
	require.NoError(t, err)
	require.JSONEq(t, string(normal), string(out))
	out, err = prepareHarnessCredentialPush(r, []byte(`{"action":"install","harness":"codex","copy_credentials":true}`))
	require.NoError(t, err)
	var req harnessops.Request
	require.NoError(t, json.Unmarshal(out, &req))
	require.NotNil(t, req.Credentials)
	require.Equal(t, `{"token":"test-fixture-secret"}`, string(req.Credentials.Files[0].Data))
	require.NoError(t, req.Validate(true))
	// A browser/CLI request may opt in, but cannot select contents or a path.
	_, err = prepareHarnessCredentialPush(r, []byte(`{"action":"install","harness":"codex","copy_credentials":true,"credentials":{"harness":"codex","files":[]}}`))
	require.Error(t, err)
	require.NotContains(t, err.Error(), "test-fixture-secret")
	r.SetPathValue("tail", "node/update")
	out, err = prepareHarnessCredentialPush(r, normal)
	require.NoError(t, err)
	require.Equal(t, normal, out)
}

func TestStandaloneCredentialPushRequiresConfirmationAndOwnSource(t *testing.T) {
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	require.NoError(t, os.Mkdir(filepath.Join(home, ".codex"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte(`{"token":"standalone-fixture"}`), 0600))
	r := httptest.NewRequest("POST", "/api/peer/pinned/harnesses/credentials/push", nil)
	r.SetPathValue("tail", "harnesses/credentials/push")
	_, err := prepareHarnessCredentialPush(r, []byte(`{"harness":"codex"}`))
	require.Error(t, err)
	require.Contains(t, err.Error(), "act as you")
	_, err = prepareHarnessCredentialPush(r, []byte(`{"harness":"codex","confirm_share":true,"credentials":{"harness":"codex","files":[]}}`))
	require.Error(t, err)
	out, err := prepareHarnessCredentialPush(r, []byte(`{"harness":"codex","confirm_share":true}`))
	require.NoError(t, err)
	var req credentialRequest
	require.NoError(t, json.Unmarshal(out, &req))
	require.True(t, req.ConfirmShare)
	require.Equal(t, `{"token":"standalone-fixture"}`, string(req.Credentials.Files[0].Data))
}
