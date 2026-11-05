//go:build linux || darwin

package agentd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// fakeTclaudeOnPath puts a `tclaude` on PATH that records its argv and
// environment to files and exits with exitCode, writing stderrText first.
func fakeTclaudeOnPath(t *testing.T, exitCode int, stderrText string) (argvFile, envFile string) {
	t.Helper()
	dir := t.TempDir()
	argvFile, envFile = filepath.Join(dir, "argv"), filepath.Join(dir, "env")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argvFile + "'\nenv > '" + envFile + "'\n" +
		"printf '%s' '" + stderrText + "' >&2\nexit " + strconv.Itoa(exitCode) + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tclaude"), []byte(script), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return argvFile, envFile
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

func openCodeWrapperArgs(a clcommon.SpawnArgs) clcommon.SpawnArgs {
	a.OpenCodeServerURL = "http://127.0.0.1:4096"
	a.OpenCodeServerPassword = "pw-secret"
	a.OpenCodeStateIsolation = "isolated"
	return a
}

// Both wrapper paths hand the child the same OpenCode attach environment.
func assertOpenCodeWrapperEnv(t *testing.T, envFile string) {
	t.Helper()
	env := readLines(t, envFile)
	assert.Contains(t, env, "TCLAUDE_OPENCODE_SERVER_URL=http://127.0.0.1:4096")
	assert.Contains(t, env, "OPENCODE_SERVER_PASSWORD=pw-secret")
	assert.Contains(t, env, clcommon.OpenCodeStateIsolationEnv+"=isolated")
}

// A fresh spawn carrying a write proof waits for its wrapper, which runs
// `tclaude session new` with the launch environment.
func TestLiveSpawnNewRunsTheSessionNewWrapper(t *testing.T) {
	resetTestDB(t)
	argvFile, envFile := fakeTclaudeOnPath(t, 0, "")
	require.NoError(t, liveSpawnNew(openCodeWrapperArgs(clcommon.SpawnArgs{
		Label: "spwn-live-new", Cwd: t.TempDir(), CwdWriteProof: "proof",
	})))
	argv := readLines(t, argvFile)
	require.GreaterOrEqual(t, len(argv), 2)
	assert.Equal(t, []string{"session", "new"}, argv[:2])
	assertOpenCodeWrapperEnv(t, envFile)
}

// A failed synchronous wrapper reports its stderr.
func TestLiveSpawnNewReportsAFailedWrapper(t *testing.T) {
	resetTestDB(t)
	fakeTclaudeOnPath(t, 1, "boom")
	err := liveSpawnNew(clcommon.SpawnArgs{Label: "spwn-live-fail", Cwd: t.TempDir(), CwdWriteProof: "proof"})
	require.ErrorContains(t, err, "spawn session wrapper failed")
	assert.ErrorContains(t, err, "boom")
}

// A resume always waits for its wrapper, which runs `tclaude session new -r`
// with the same launch environment.
func TestLiveSpawnResumeRunsTheResumeWrapper(t *testing.T) {
	resetTestDB(t)
	argvFile, envFile := fakeTclaudeOnPath(t, 0, "")
	require.NoError(t, liveSpawnResume(openCodeWrapperArgs(clcommon.SpawnArgs{
		ConvID: "conv-live-resume", Cwd: t.TempDir(), Harness: harness.DefaultName,
	})))
	argv := readLines(t, argvFile)
	assert.Contains(t, argv, "conv-live-resume")
	assertOpenCodeWrapperEnv(t, envFile)
}

// A failed resume wrapper reports its stderr.
func TestLiveSpawnResumeReportsAFailedWrapper(t *testing.T) {
	resetTestDB(t)
	fakeTclaudeOnPath(t, 1, "boom")
	err := liveSpawnResume(clcommon.SpawnArgs{ConvID: "conv-live-fail", Cwd: t.TempDir(), Harness: harness.DefaultName})
	require.ErrorContains(t, err, "resume session wrapper failed")
	assert.ErrorContains(t, err, "boom")
}

// Regression pin: once a sandboxed OpenCode resume has started its managed
// server, an allocation that is gone by the post-start check (the launch-spec
// build read it moments earlier; here a concurrent delete removes it as the
// server starts) must stop that server again rather than leave it running with
// no pane. The started runtime is recorded the way a real start records it;
// stopping it deletes the record.
func TestLiveSpawnResumeStopsOpenCodeWhenTheStateAllocationIsMissing(t *testing.T) {
	resetTestDB(t)
	const convID = "conv-live-oc-orphan"
	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	_, err = allocatePrivateOpenCodeState(agentID)
	require.NoError(t, err)

	exited := exec.Command("true")
	require.NoError(t, exited.Run()) // a PID that is no longer alive
	started := ""
	restore := SetOpenCodeRuntimeForTest(func(sessionID, cwd, _, resumeID string) (OpenCodeRuntimeFixture, error) {
		started = sessionID
		require.NoError(t, db.DeleteOpenCodeAgentStateAllocation(agentID))
		require.NoError(t, db.UpsertOpenCodeRuntime(db.OpenCodeRuntime{
			SessionID: sessionID, ConvID: resumeID, ServerURL: "http://127.0.0.1:1",
			PID: exited.ProcessState.Pid(), Cwd: cwd,
		}))
		return OpenCodeRuntimeFixture{SessionID: sessionID, ConvID: resumeID, PID: exited.ProcessState.Pid()}, nil
	})
	t.Cleanup(restore)

	err = liveSpawnResume(clcommon.SpawnArgs{
		ConvID: convID, Cwd: t.TempDir(), Harness: harness.OpenCodeName, Sandbox: harness.OpenCodeSandboxTclaudeLayer, Approval: harness.OpenCodeApprovalAsk,
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
	})
	require.ErrorContains(t, err, "no durable state allocation")
	require.NotEmpty(t, started, "the resume must have reached the started runtime")
	runtime, err := db.GetOpenCodeRuntime(started)
	require.NoError(t, err)
	assert.Nil(t, runtime, "the OpenCode runtime started for the failed resume must be stopped")
}
