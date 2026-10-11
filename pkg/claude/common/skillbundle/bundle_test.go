package skillbundle

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestSkillExportRejectsUnsafeFilesAndCredentials(t *testing.T) {
	root := testutil.CanonicalTempDir(t)
	dir := filepath.Join(root, "demo")
	require.NoError(t, os.Mkdir(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# Demo\nRun scripts/check.py"), 0600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "scripts"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "scripts", "check.py"), []byte("#!/usr/bin/env python3\nprint('ok')\n"), 0700))
	b, e := Export(root, "demo")
	require.NoError(t, e)
	require.Len(t, b.Files, 2)
	assert.True(t, SecurityRelevant(b.Files[1]))
	assert.True(t, b.Files[1].Executable)
	external := filepath.Join(root, "outside")
	require.NoError(t, os.WriteFile(external, []byte("outside"), 0600))
	require.NoError(t, os.Symlink(external, filepath.Join(dir, "link")))
	_, e = Export(root, "demo")
	require.ErrorContains(t, e, "symlink")
	_, _, e = ReadFile(dir, "link", 1024)
	require.Error(t, e)
	require.NoError(t, os.Remove(filepath.Join(dir, "link")))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret"), []byte("api_key=sk-ant-abcdefghijklmnopqrstuvwxyz123456"), 0600))
	_, e = Export(root, "demo")
	require.ErrorContains(t, e, "credentials")
}

func TestSkillManifestRejectsTraversalAndDuplicatePaths(t *testing.T) {
	for _, bad := range []string{"../outside", "/outside", "a/../outside", "a\\outside", "SKILL.md"} {
		b := Bundle{Format: Format, Name: "demo", Files: []File{{Path: "SKILL.md", Data: []byte("safe")}, {Path: bad, Data: []byte("safe")}}}
		require.Error(t, b.Validate())
	}
	for _, name := range []string{".", "..", "../demo", "demo/name", ""} {
		require.False(t, ValidName(name))
	}
}
