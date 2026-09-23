package claude

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestRunShellOutputWorkdirAndExitStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell pseudo-harness targets Unix")
	}
	dir := t.TempDir()
	var out, errOut bytes.Buffer
	code, err := runOnce(runParams{Harness: "shell", Workdir: dir}, []string{"pwd"}, &out, &errOut)
	require.NoError(t, err)
	require.Equal(t, 0, code)
	require.Equal(t, dir+"\n", out.String())
	require.Empty(t, errOut.String())

	out.Reset()
	code, err = runOnce(runParams{Harness: "shell"}, []string{"printf 'done\\n'; exit 17"}, &out, &errOut)
	require.NoError(t, err)
	require.Equal(t, 17, code)
	require.Equal(t, "done\n", out.String())
}

func TestRunShellReceivesPipedStdin(t *testing.T) {
	var out, errOut bytes.Buffer
	code, err := runOnceInput(runParams{Harness: "shell"}, []string{"cat"},
		strings.NewReader("line one\nline two\n"), &out, &errOut)
	require.NoError(t, err)
	require.Equal(t, 0, code)
	require.Equal(t, "line one\nline two\n", out.String())
	require.Empty(t, errOut.String())
}

func TestRunHarnessUsesFreshPrintArgv(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out, errOut bytes.Buffer
	code, err := runOnce(runParams{Harness: "claude", Workdir: dir}, []string{"--version"}, &out, &errOut)
	require.NoError(t, err)
	require.Equal(t, 0, code)
	require.Equal(t, "-p\n--\n--version\n", out.String())
	require.Empty(t, errOut.String())
}

func TestRunHarnessFoldsPipedInputIntoPrompt(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out, errOut bytes.Buffer
	code, err := runOnceInput(runParams{Harness: "claude"}, []string{"summarize"},
		strings.NewReader("first\nsecond\n"), &out, &errOut)
	require.NoError(t, err)
	require.Equal(t, 0, code)
	require.Contains(t, out.String(), "summarize\n\n--- piped input (stdin) ---\nfirst\nsecond")
	out.Reset()
	code, err = runOnceInput(runParams{Harness: "claude"}, nil,
		strings.NewReader("piped-only\n"), &out, &errOut)
	require.NoError(t, err)
	require.Equal(t, 0, code)
	require.Equal(t, "-p\n--\npiped-only\n", out.String())
}

func TestRunPipedPromptTimeoutAndSizeLimit(t *testing.T) {
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = reader.Close(); _ = writer.Close() })
	var out, errOut bytes.Buffer
	code, err := runOnceInput(runParams{Harness: "claude", Timeout: "30ms"},
		nil, reader, &out, &errOut)
	require.Equal(t, 124, code)
	require.ErrorContains(t, err, "timed out")

	code, err = runOnceInput(runParams{Harness: "claude"}, nil,
		strings.NewReader(strings.Repeat("x", maxRunPipedPromptBytes+1)), &out, &errOut)
	require.Equal(t, 1, code)
	require.ErrorContains(t, err, "piped prompt exceeds")
}

func TestRunOpenCodeGetsCaptureStdout(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "opencode")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nif [ -t 1 ]; then echo tty >&2; else echo clean; fi\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out, errOut bytes.Buffer
	code, err := runOnce(runParams{Harness: "opencode"}, []string{"hello"}, &out, &errOut)
	require.NoError(t, err)
	require.Equal(t, 0, code)
	require.Equal(t, "clean\n", out.String())
	require.Empty(t, errOut.String())
}

func TestRunCodexReceivesSandboxAndExitStatus(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "codex")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\necho diagnostic >&2\nexit 9\n"), 0o755))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out, errOut bytes.Buffer
	code, err := runOnce(runParams{Harness: "codex", Sandbox: "read-only"}, []string{"fix it"}, &out, &errOut)
	require.NoError(t, err)
	require.Equal(t, 9, code)
	require.Contains(t, out.String(), "--sandbox\nread-only\nexec\n")
	require.Contains(t, errOut.String(), "diagnostic")
}

func TestRunTimeoutKillsShellChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group cancellation targets Unix")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "late")
	var out, errOut bytes.Buffer
	start := time.Now()
	code, err := runOnce(runParams{Harness: "shell", Timeout: "50ms"},
		[]string{"sleep 1; touch " + marker}, &out, &errOut)
	require.Equal(t, 124, code)
	require.ErrorContains(t, err, "timed out")
	require.Less(t, time.Since(start), time.Second)
	time.Sleep(1100 * time.Millisecond)
	_, statErr := os.Stat(marker)
	require.True(t, os.IsNotExist(statErr), "timed-out shell left a child running")
}

func TestRunInterruptKillsShellChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process-group cancellation targets Unix")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "late")
	ready := filepath.Join(dir, "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestRunInterruptHelper$")
	cmd.Env = append(os.Environ(), "TCLAUDE_RUN_TEST_MARKER="+marker, "TCLAUDE_RUN_TEST_READY="+ready)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, time.Second, 10*time.Millisecond)
	require.NoError(t, cmd.Process.Signal(syscall.SIGINT))
	require.NoError(t, cmd.Wait())
	time.Sleep(1100 * time.Millisecond)
	_, err := os.Stat(marker)
	require.True(t, os.IsNotExist(err), "interrupted shell left a child running")
}

func TestRunInterruptHelper(t *testing.T) {
	marker := os.Getenv("TCLAUDE_RUN_TEST_MARKER")
	if marker == "" {
		return
	}
	ready := os.Getenv("TCLAUDE_RUN_TEST_READY")
	var out, errOut bytes.Buffer
	code, err := runOnce(runParams{Harness: "shell"},
		[]string{"touch " + ready + "; (sleep 1; touch " + marker + ") & wait"}, &out, &errOut)
	if code != 130 || err == nil {
		t.Fatalf("interrupt returned code %d, error %v", code, err)
	}
}

func TestRunWaitDelayKeepsSuccessfulChildStatus(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell pseudo-harness targets Unix")
	}
	t.Setenv("SHELL", "/bin/sh")
	var out, errOut bytes.Buffer
	code, err := runOnce(runParams{Harness: "shell"},
		[]string{"echo hi; (sleep 5; echo late) &"}, &out, &errOut)
	require.NoError(t, err)
	require.Equal(t, 0, code)
	require.Equal(t, "hi\n", out.String())
	require.Empty(t, errOut.String())
}

func TestRunRejectsInvalidOptions(t *testing.T) {
	for _, p := range []runParams{
		{Harness: "shell", Timeout: "0s"},
		{Harness: "shell", Sandbox: "read-only"},
		{Harness: "shell", CPU: "abc"},
		{Harness: "shell", CPU: "0"},
		{Harness: "shell", Memory: "lots"},
		{Harness: "shell", PIDs: -1},
	} {
		code, err := runOnce(p, []string{"true"}, &bytes.Buffer{}, &bytes.Buffer{})
		require.Equal(t, 1, code)
		require.Error(t, err)
	}
	code, err := runOnce(runParams{Harness: "shell"}, []string{"  "}, &bytes.Buffer{}, &bytes.Buffer{})
	require.Equal(t, 1, code)
	require.True(t, strings.Contains(err.Error(), "required"))
}

func TestRunCgroupJoinsBeforeChildStarts(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("run cgroups are Linux only")
	}
	t.Setenv("SHELL", "/bin/sh")
	marker := filepath.Join(t.TempDir(), "started")
	var joined sandboxpolicy.ResourceLimits
	old := joinRunCgroup
	joinRunCgroup = func(limits sandboxpolicy.ResourceLimits, stderr io.Writer) (*agent.RunCgroupHold, error) {
		_, err := os.Stat(marker)
		require.True(t, os.IsNotExist(err), "child started before the cgroup was joined")
		joined = limits
		return nil, nil
	}
	t.Cleanup(func() { joinRunCgroup = old })
	var out, errOut bytes.Buffer
	code, err := runOnce(runParams{Harness: "shell", CPU: "1.5", Memory: "512MiB", PIDs: 64},
		[]string{"touch " + marker}, &out, &errOut)
	require.NoError(t, err)
	require.Equal(t, 0, code)
	require.NotNil(t, joined.CPU)
	require.Equal(t, 1.5, *joined.CPU)
	require.Equal(t, uint64(512<<20), joined.MemoryBytes)
	require.NotNil(t, joined.PIDs)
	require.Equal(t, uint64(64), *joined.PIDs)
}

func TestRunCgroupFailureStopsRun(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("run cgroups are Linux only")
	}
	t.Setenv("SHELL", "/bin/sh")
	marker := filepath.Join(t.TempDir(), "started")
	old := joinRunCgroup
	joinRunCgroup = func(sandboxpolicy.ResourceLimits, io.Writer) (*agent.RunCgroupHold, error) {
		return nil, errors.New("no delegation")
	}
	t.Cleanup(func() { joinRunCgroup = old })
	code, err := runOnce(runParams{Harness: "shell", Cgroup: true},
		[]string{"touch " + marker}, &bytes.Buffer{}, &bytes.Buffer{})
	require.Equal(t, 1, code)
	require.ErrorContains(t, err, "no delegation")
	_, statErr := os.Stat(marker)
	require.True(t, os.IsNotExist(statErr))
}
