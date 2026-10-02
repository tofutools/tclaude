package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readGeminiTrustForTest(t *testing.T, path string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var out map[string]string
	require.NoError(t, json.Unmarshal(data, &out))
	return out
}

func TestGeminiDirTrustSeedsTheLaunchStore(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(t.TempDir(), "proj")
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }

	require.NoError(t, EnsureGeminiDirTrustedForLaunch(getenv, home, project))
	path := filepath.Join(home, ".gemini", "trustedFolders.json")
	assert.Equal(t, map[string]string{project: "TRUST_FOLDER"}, readGeminiTrustForTest(t, path))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// Idempotent, and other rules survive.
	require.NoError(t, os.WriteFile(path, []byte(`{"/elsewhere":"DO_NOT_TRUST","`+project+`":"TRUST_FOLDER"}`), 0o600))
	require.NoError(t, EnsureGeminiDirTrustedForLaunch(getenv, home, project))
	assert.Equal(t, map[string]string{"/elsewhere": "DO_NOT_TRUST", project: "TRUST_FOLDER"},
		readGeminiTrustForTest(t, path))

	// GEMINI_CLI_HOME moves the store; an explicit path override wins.
	alt := t.TempDir()
	env[GeminiHomeEnvVar] = alt
	require.NoError(t, EnsureGeminiDirTrustedForLaunch(getenv, home, project))
	assert.FileExists(t, filepath.Join(alt, ".gemini", "trustedFolders.json"))
	explicit := filepath.Join(t.TempDir(), "trust.json")
	env[GeminiTrustedFoldersPathEnvVar] = explicit
	require.NoError(t, EnsureGeminiDirTrustedForLaunch(getenv, home, project))
	assert.FileExists(t, explicit)

	env[GeminiTrustedFoldersPathEnvVar] = "relative.json"
	assert.Error(t, EnsureGeminiDirTrustedForLaunch(getenv, home, project))
	assert.Error(t, EnsureGeminiDirTrustedForLaunch(nil, home, "relative/dir"))
}

func TestGeminiDirTrustRefusesWhatItCannotSafelyEdit(t *testing.T) {
	const dir = "/work/proj"
	for name, body := range map[string]string{
		"comments":       "{\n // mine\n \"/a\": \"TRUST_FOLDER\"\n}",
		"bad level":      `{"/a":"MAYBE"}`,
		"not an object":  `["/a"]`,
		"explicit block": `{"/work/proj":"DO_NOT_TRUST"}`,
	} {
		_, _, err := planGeminiDirTrust([]byte(body), dir)
		assert.Error(t, err, name)
	}
	changed, _, err := planGeminiDirTrust([]byte(`{"/work/proj/":"TRUST_PARENT"}`), dir)
	require.NoError(t, err)
	assert.False(t, changed, "an existing trusting rule for the same path is a no-op")

	changed, out, err := planGeminiDirTrust(nil, dir)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.JSONEq(t, `{"/work/proj":"TRUST_FOLDER"}`, string(out))
}

func TestGeminiDirTrustIsWiredThroughTheSeam(t *testing.T) {
	h, ok := Get(GeminiName)
	require.True(t, ok)
	trust, err := ResolveTrustDir(h, true)
	require.NoError(t, err)
	assert.True(t, trust)
	assert.Equal(t, "~/.gemini/trustedFolders.json", DirTrustStore(h))

	home := t.TempDir()
	project := t.TempDir()
	require.NoError(t, EnsureDirTrustedForLaunch(h, project, func(string) string { return "" }, home))
	assert.FileExists(t, filepath.Join(home, ".gemini", "trustedFolders.json"))
}
