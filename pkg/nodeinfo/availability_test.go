package nodeinfo

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
	"os"
	"path/filepath"
	"testing"
)

func TestHarnessAvailabilityProbe(t *testing.T) {
	dir := testutil.CanonicalTempDir(t)
	for _, name := range harness.Names() {
		h, _ := harness.Get(name)
		for _, key := range h.AvailabilityCredentialEnv {
			t.Setenv(key, "")
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("OPENAI_API_KEY", "never-expose-this-secret")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\n[ \"$1\" = --version ] || exit 1\nprintf 'codex 1.2.3\\n'\n"), 0700))
	out := ProbeAvailability(context.Background())
	require.Len(t, out.Harnesses, len(harness.Names()))
	require.True(t, out.RefreshAfter.After(out.ObservedAt))
	rows := map[string]HarnessAvailability{}
	for _, row := range out.Harnesses {
		rows[row.Name] = row
	}
	require.True(t, rows["codex"].Installed)
	require.Equal(t, filepath.Join(dir, "codex"), rows["codex"].Path)
	require.Equal(t, "codex 1.2.3", rows["codex"].Version)
	require.Equal(t, "known", rows["codex"].VersionStatus)
	require.NotNil(t, rows["codex"].CredentialPresent)
	require.True(t, *rows["codex"].CredentialPresent)
	require.Nil(t, rows["codex"].Usable)
	require.False(t, rows["claude"].Installed)
	require.False(t, *rows["claude"].Usable)
	require.Nil(t, rows["claude"].CredentialPresent)
}
