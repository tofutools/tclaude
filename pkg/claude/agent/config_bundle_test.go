package agent

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigBundleCLIImportsPreviewAndBindings(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(`{"changes":[{"item":"sandbox-profiles/example","action":"create","security":true,"after":{"filesystem":[]}}],"unresolved":[{"name":"path_1","item":"sandbox-profiles/example","field":"value.profiles[0].path"}],"security_changes":1,"applied":[]}`))
	var out, stderr bytes.Buffer
	rc := runConfigImport(&configImportParams{File: "-", Only: []string{"sandbox-profiles"}, Set: []string{"root=/opt/example"}}, strings.NewReader(`{"format":"tclaude-config-bundle","format_version":1,"sections":{}}`), &out, &stderr)
	require.Equal(t, rcOK, rc, stderr.String())
	assert.Contains(t, out.String(), "[security]")
	assert.Contains(t, out.String(), "--set path_1=value")
	assert.Contains(t, out.String(), "Preview only")
	require.Len(t, calls, 1)
	assert.Equal(t, "/v1/config-bundle/import", calls[0].path)
	body := calls[0].body.(map[string]any)
	assert.Equal(t, false, body["apply"])
	assert.Equal(t, map[string]string{"root": "/opt/example"}, body["values"])
}

func TestConfigBundleCLIFlagDetailsAreRedacted(t *testing.T) {
	var out bytes.Buffer
	printConfigBundleError(&out, &DaemonError{Msg: "flagged export", Raw: []byte(`{"flags":[{"item":"roles/worker","field":"value.brief","hint":"suspected credential (value redacted)"}]}`)})
	assert.Contains(t, out.String(), "roles/worker value.brief")
	assert.Contains(t, out.String(), "redacted")
}
