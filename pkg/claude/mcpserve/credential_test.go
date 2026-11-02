package mcpserve

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

const testCredential = "tcls_abc_secretsecret"

func writeCredential(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(testutil.CanonicalTempDir(t), "credential")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

func TestLoadCredential_FromTheEnvironmentAndClearsIt(t *testing.T) {
	t.Setenv(CredentialEnv, testCredential)
	credential, err := LoadCredential("")
	require.NoError(t, err)
	assert.Equal(t, testCredential, credential)
	assert.Empty(t, os.Getenv(CredentialEnv), "nothing this process starts may inherit it")
}

func TestLoadCredential_FromAPrivateFile(t *testing.T) {
	t.Setenv(CredentialEnv, "")
	credential, err := LoadCredential(writeCredential(t, testCredential+"\n", 0o600))
	require.NoError(t, err)
	assert.Equal(t, testCredential, credential)
}

func TestLoadCredential_Refusals(t *testing.T) {
	for name, mode := range map[string]os.FileMode{"group": 0o640, "world": 0o604} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(CredentialEnv, "")
			_, err := LoadCredential(writeCredential(t, testCredential, mode))
			require.ErrorContains(t, err, "readable by others")
		})
	}
	t.Run("not a file", func(t *testing.T) {
		t.Setenv(CredentialEnv, "")
		_, err := LoadCredential(testutil.CanonicalTempDir(t))
		require.ErrorContains(t, err, "not a regular file")
	})
	t.Run("a FIFO does not hang startup", func(t *testing.T) {
		t.Setenv(CredentialEnv, "")
		fifo := filepath.Join(testutil.CanonicalTempDir(t), "fifo")
		require.NoError(t, syscall.Mkfifo(fifo, 0o600))
		_, err := LoadCredential(fifo)
		require.ErrorContains(t, err, "not a regular file")
	})
	t.Run("both sources", func(t *testing.T) {
		t.Setenv(CredentialEnv, testCredential)
		_, err := LoadCredential(writeCredential(t, testCredential, 0o600))
		require.ErrorContains(t, err, "use one")
	})
	t.Run("neither source", func(t *testing.T) {
		t.Setenv(CredentialEnv, "")
		_, err := LoadCredential("")
		require.ErrorContains(t, err, "no credential")
	})
	t.Run("the operator token by mistake", func(t *testing.T) {
		t.Setenv(CredentialEnv, "tclo_operatortoken")
		_, err := LoadCredential("")
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "operatortoken")
	})
}
