package harness

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestCopilotHistoryNativeProjection(t *testing.T) {
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("COPILOT_HOME", filepath.Join(home, "copilot"))
	cwd := testutil.CanonicalTempDir(t)
	h := copilotHistory{}
	raw := []byte(`{"id":"event-start","type":"session.start","data":{"sessionId":"019fe740-43a4-7023-b8ae-1ee64459f2a1","context":{"cwd":"/old","trustedDirectories":["/"]}}}
{"id":"event-user","parentId":"event-start","type":"user.message","data":{"content":"retain /old and 019fe740-43a4-7023-b8ae-1ee64459f2a1 verbatim"}}
{"id":"event-answer","parentId":"event-user","type":"assistant.message","data":{"content":"portable answer"}}
`)
	require.NoError(t, h.Validate(raw, historySource))
	require.Error(t, h.Validate(raw, historyTarget))
	id, cleanup, err := h.ImportReader(bytes.NewReader(raw), historySource, cwd)
	require.NoError(t, err)
	require.NotEqual(t, historySource, id)
	exported, err := h.Export(id, cwd)
	require.NoError(t, err)
	require.Contains(t, string(exported), "retain /old and "+historySource+" verbatim")
	require.NotContains(t, string(exported), "trustedDirectories")
	require.Contains(t, string(exported), cwd)
	require.NotContains(t, string(exported), `"id":"event-start"`)
	dir := filepath.Join(copilotHome(), copilotSessionStateDirName, id)
	files, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, files, 2)
	cleanup()
	require.NoDirExists(t, dir)
	require.Error(t, h.Validate(append(raw, raw...), historySource))
}
