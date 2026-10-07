package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

func TestHTTPProxyCLI(t *testing.T) {
	oldAvail := agent.DaemonAvailableImpl
	agent.DaemonAvailableImpl = func() bool { return true }
	old := agent.DaemonRequestImpl
	t.Cleanup(func() { agent.DaemonRequestImpl = old; agent.DaemonAvailableImpl = oldAvail })
	calls := 0
	agent.DaemonRequestImpl = func(method, path string, in, out any, opts agent.DaemonOpts) error {
		calls++
		require.Equal(t, http.MethodPost, method)
		require.Equal(t, "/v1/http/request", path)
		assert.True(t, opts.NoRetry)
		req := in.(map[string]any)
		assert.Equal(t, "service", req["name"])
		assert.Equal(t, "items", req["path"])
		assert.Equal(t, []byte{0, 255, 10}, req["body"])
		assert.Equal(t, "text/plain", req["headers"].(map[string]string)["Content-Type"])
		return json.Unmarshal([]byte(`{"status":429,"headers":{"X-Result":["limited"]},"body":"AP8K"}`), out)
	}
	p := &httpParams{Name: "service", Path: "items", Method: "POST", Header: []string{"Content-Type: text/plain"}, BodyFile: "-"}
	var stdout, stderr bytes.Buffer
	assert.Equal(t, rcIOFailure, httpProxyCall(p, bytes.NewReader([]byte{0, 255, 10}), &stdout, &stderr))
	assert.Equal(t, []byte{0, 255, 10}, stdout.Bytes())
	assert.Empty(t, stderr.String())
	assert.Greater(t, calls, 0)
	p.JSON = true
	stdout.Reset()
	assert.Equal(t, rcIOFailure, httpProxyCall(p, bytes.NewReader([]byte{0, 255, 10}), &stdout, &stderr))
	var result struct {
		Status  int
		Headers http.Header
		Body    []byte
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &result))
	assert.Equal(t, 429, result.Status)
	assert.Equal(t, "limited", result.Headers.Get("X-Result"))
	assert.Equal(t, []byte{0, 255, 10}, result.Body)
}
