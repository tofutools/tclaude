//go:build unix

package session

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// A danger-full-access Codex launch keeps no verdict only when Codex reports
// no managed requirements. Requirements that leave danger-full-access out
// record a verified "on"; any other requirement, or a failed read, an
// unverified one. Other launches never run Codex.
func TestVerifyCodexSandboxOff(t *testing.T) {
	codex, ok := harness.Get(harness.CodexName)
	require.True(t, ok)
	claude, ok := harness.Get(harness.DefaultName)
	require.True(t, ok)
	off, err := harness.SandboxOffMode(codex)
	require.NoError(t, err)

	var reply json.RawMessage
	var replyErr error
	calls := 0
	t.Cleanup(SetCodexRequirementsProbeForTest(func(string, []sandboxpolicy.EnvironmentEntry) (json.RawMessage, error) {
		calls++
		return reply, replyErr
	}))
	builtin := sandboxpolicy.ImplementationHarnessBuiltin
	verify := func(h *harness.Harness, impl sandboxpolicy.Implementation, mode string) harness.LaunchOSSandbox {
		return VerifyCodexSandboxOff(h, impl, mode, "/work", nil, harness.LaunchOSSandbox{})
	}

	reply = json.RawMessage(`{"requirements":null}`)
	assert.Equal(t, harness.LaunchOSSandbox{}, verify(codex, builtin, off), "no managed requirements: the mode stands")
	reply = json.RawMessage(`{}`)
	assert.Equal(t, harness.LaunchOSSandbox{}, verify(codex, builtin, off), "an older server that omits the field")

	reply = json.RawMessage(`{"requirements":{"allowedSandboxModes":["read-only","workspace-write"]}}`)
	assert.Equal(t, harness.LaunchOSSandbox{State: "on",
		Source: "Codex managed requirements (allowed sandbox modes [read-only, workspace-write])"},
		verify(codex, builtin, off), "Codex must fall back to a sandbox")

	reply = json.RawMessage(`{"requirements":{"allowedSandboxModes":["read-only","danger-full-access","<b>"]}}`)
	assert.Equal(t, harness.LaunchOSSandbox{State: "on", Unverified: true,
		Source: "Codex managed requirements (allowed sandbox modes [read-only, danger-full-access])"},
		verify(codex, builtin, off), "an allowed mode may still meet a requirement not on the wire (deny_read)")

	reply = json.RawMessage(`{"requirements":{"modelProvider":"openai"}}`)
	v := verify(codex, builtin, off)
	assert.Equal(t, "on", v.State)
	assert.True(t, v.Unverified)

	reply, replyErr = nil, errors.New("no codex")
	assert.Equal(t, harness.LaunchOSSandbox{State: "on", Unverified: true,
		Source: "Codex managed requirements could not be read: no codex"}, verify(codex, builtin, off))

	calls = 0
	verify(codex, builtin, "workspace-write")
	verify(codex, sandboxpolicy.ImplementationTclaudeLayer, off)
	verify(claude, builtin, off)
	assert.Zero(t, calls, "only a harness-builtin Codex launch with no sandbox is checked")
}

// The probe sends configRequirements/read, without params, over the same
// app-server exchange as config/read.
func TestCodexRequirementsReadOverTheAppServer(t *testing.T) {
	dir := t.TempDir()
	writeCodexStub(t, dir, 0o755, `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *'"id":2'*'"method":"configRequirements/read"'*)
      case "$line" in *params*) echo '{"id":2,"error":{"message":"unexpected params"}}';;
      *) echo '{"id":2,"result":{"requirements":{"allowedSandboxModes":["read-only"]}}}';; esac;;
    *'"id":2'*) echo '{"id":2,"error":{"message":"wrong method"}}';;
  esac
done
`)
	raw, err := readCodexRequirements(dir, []sandboxpolicy.EnvironmentEntry{{Name: "PATH", Value: filepath.Clean(dir)}})
	require.NoError(t, err)
	assert.JSONEq(t, `{"requirements":{"allowedSandboxModes":["read-only"]}}`, string(raw))
}
