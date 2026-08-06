package agent

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sandboxImplAssignedBody = `{
	"conv_id":"5f8c1e2a-1111-2222-3333-444455556666",
	"agent_id":"agt_abc","harness":"claude",
	"sandbox_implementation":"resource-only",
	"previous_sandbox_implementation":"harness-builtin",
	"sandbox":"off","sandbox_source":"operator sandbox assignment",
	"online":false,"resource_cgroup":true
}`

func TestRunSandboxImplSet_PostsTheSelectorAndBody(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(method, path string) (int, string, string) {
		assert.Equal(t, http.MethodPost, method)
		assert.Equal(t, "/v1/agent/legacy-worker/sandbox-impl", path)
		return 200, "", sandboxImplAssignedBody
	})

	var stdout, stderr bytes.Buffer
	rc := runSandboxImplSet(&sandboxImplSetParams{
		Agent: "legacy-worker", Implementation: "resource-only",
	}, &stdout, &stderr)

	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	require.Len(t, calls, 1)
	body, ok := calls[0].body.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "resource-only", body["implementation"])
	assert.Equal(t, "", body["sandbox"], "an unpinned mode is sent empty, not omitted")

	out := stdout.String()
	assert.Contains(t, out, "harness-builtin → resource-only",
		"the operator needs to see what the assignment replaced")
	assert.Contains(t, out, "per-agent cgroup: yes")
	assert.Contains(t, out, "wake the agent")
}

func TestRunSandboxImplSet_PinsAnExplicitMode(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(sandboxImplAssignedBody))

	var stdout, stderr bytes.Buffer
	rc := runSandboxImplSet(&sandboxImplSetParams{
		Agent: "legacy-worker", Implementation: "harness-builtin", Sandbox: "on",
	}, &stdout, &stderr)

	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	require.Len(t, calls, 1)
	body, ok := calls[0].body.(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "on", body["sandbox"])
}

// A misspelling is caught locally against the closed set, so the operator gets
// the valid values back instead of a round-trip. The daemon still validates
// against the resolved harness and host, which is the authoritative refusal.
func TestRunSandboxImplSet_RejectsAnUnknownImplementationWithoutCallingTheDaemon(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(sandboxImplAssignedBody))

	var stdout, stderr bytes.Buffer
	rc := runSandboxImplSet(&sandboxImplSetParams{
		Agent: "legacy-worker", Implementation: "resource_only",
	}, &stdout, &stderr)

	assert.Equal(t, rcInvalidArg, rc)
	assert.Contains(t, stderr.String(), "invalid sandbox implementation")
	assert.Empty(t, calls, "a value the closed set rejects never reaches the daemon")
}

func TestRunSandboxImplSet_RequiresAnAgentSelector(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(sandboxImplAssignedBody))

	var stdout, stderr bytes.Buffer
	rc := runSandboxImplSet(&sandboxImplSetParams{
		Agent: "  ", Implementation: "resource-only",
	}, &stdout, &stderr)

	assert.Equal(t, rcInvalidArg, rc)
	assert.Empty(t, calls)
}

func TestRunSandboxImplShow_GetsAndRenders(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, func(method, path string) (int, string, string) {
		assert.Equal(t, http.MethodGet, method)
		assert.Equal(t, "/v1/agent/some%20agent/sandbox-impl", path)
		return 200, "", `{
			"conv_id":"5f8c1e2a-1111-2222-3333-444455556666",
			"sandbox_implementation":"harness-builtin",
			"sandbox":"on","sandbox_source":"group default profile \"confined\"",
			"online":true,"resource_cgroup":false
		}`
	})

	var stdout, stderr bytes.Buffer
	rc := runSandboxImplShow(&sandboxImplShowParams{Agent: "some agent"}, &stdout, &stderr)

	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	out := stdout.String()
	assert.Contains(t, out, "sandbox implementation harness-builtin")
	assert.NotContains(t, out, "→", "a read replaced nothing")
	assert.Contains(t, out, `harness sandbox mode: on (chosen by group default profile "confined")`)
	assert.Contains(t, out, "the agent is running")
	assert.NotContains(t, out, "per-agent cgroup")
}

func TestRunSandboxImplShow_JSONEmitsTheWireShape(t *testing.T) {
	var calls []capturedReq
	stubDaemon(t, &calls, ok(sandboxImplAssignedBody))

	var stdout, stderr bytes.Buffer
	rc := runSandboxImplShow(&sandboxImplShowParams{Agent: "legacy-worker", JSON: true}, &stdout, &stderr)

	require.Equal(t, rcOK, rc, "stderr=%s", stderr.String())
	assert.True(t, strings.HasPrefix(strings.TrimSpace(stdout.String()), "{"))
	assert.Contains(t, stdout.String(), `"sandbox_implementation": "resource-only"`)
}
