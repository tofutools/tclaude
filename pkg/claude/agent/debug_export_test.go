package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunDebugExport_TargetAndOpaqueEnvelope(t *testing.T) {
	var gotPath string
	stubDaemonGetRaw(t, &gotPath, `{"format":"tclaude-agent-debug","format_version":1,"future":{"kept":true}}`)
	var stdout, stderr bytes.Buffer
	rc := runDebugExport(&debugExportParams{Target: "worker one"}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	assert.Equal(t, "/v1/agent/worker%20one/debug-export", gotPath)
	var result map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
	assert.Equal(t, true, result["future"].(map[string]any)["kept"])
}

func TestRunDebugExport_SelfToPrivateFile(t *testing.T) {
	var gotPath string
	stubDaemonGetRaw(t, &gotPath, `{"format":"tclaude-agent-debug"}`)
	path := filepath.Join(t.TempDir(), "debug.json")
	var stdout, stderr bytes.Buffer
	rc := runDebugExport(&debugExportParams{File: path}, &stdout, &stderr)
	require.Equal(t, rcOK, rc, "stderr=%q", stderr.String())
	assert.Equal(t, "/v1/whoami/debug-export", gotPath)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}
