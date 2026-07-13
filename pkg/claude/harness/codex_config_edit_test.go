package harness

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEditCodexConfigFile_RetriesAfterNonCooperatingWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("model = \"initial\"\n"), 0o600))

	calls := 0
	err := EditCodexConfigFile(path, 0o600, func(data []byte) (bool, []byte, error) {
		calls++
		if calls == 1 {
			// Simulate Codex writing without taking tclaude's advisory lock
			// after our read but before our stale-read check.
			require.NoError(t, os.WriteFile(path, []byte("model = \"external\"\n"), 0o600))
		}
		out := append(bytes.Clone(data), []byte("tclaude_key = true\n")...)
		return true, out, nil
	})
	require.NoError(t, err)
	assert.Equal(t, 2, calls)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "model = \"external\"\ntclaude_key = true\n", string(data))
}

func TestEditCodexConfigFile_RetriesAfterWriterDuringTempStaging(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte("model = \"initial\"\n"), 0o600))

	planCalls := 0
	prepareCalls := 0
	err := editCodexConfigFile(path, 0o600, func(data []byte) (bool, []byte, error) {
		planCalls++
		out := append(bytes.Clone(data), []byte("tclaude_key = true\n")...)
		return true, out, nil
	}, func(target string, data []byte, perm os.FileMode) (*atomicFileReplacement, error) {
		prepareCalls++
		replacement, err := prepareAtomicWriteFile(target, data, perm)
		if err == nil && prepareCalls == 1 {
			// Simulate Codex replacing config after our candidate temp file is
			// complete but before tclaude's final stale-read check.
			require.NoError(t, os.WriteFile(path, []byte("model = \"external\"\n"), 0o600))
		}
		return replacement, err
	})
	require.NoError(t, err)
	assert.Equal(t, 2, planCalls)
	assert.Equal(t, 2, prepareCalls)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "model = \"external\"\ntclaude_key = true\n", string(data))
}
