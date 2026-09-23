package claude

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

func TestRunRejectsInvalidOptions(t *testing.T) {
	for _, p := range []runParams{
		{Harness: "shell", Timeout: "0s"},
		{Harness: "shell", SandboxProfile: "x"},
		{Harness: "shell", SandboxImpl: "invalid"},
		{Harness: "shell", Sandbox: "read-only"},
		{Harness: "opencode", SandboxImpl: "tclaude-layer"},
	} {
		code, err := runOnce(p, []string{"true"}, &bytes.Buffer{}, &bytes.Buffer{})
		require.Equal(t, 1, code)
		require.Error(t, err)
	}
	code, err := runOnce(runParams{Harness: "shell"}, []string{"  "}, &bytes.Buffer{}, &bytes.Buffer{})
	require.Equal(t, 1, code)
	require.True(t, strings.Contains(err.Error(), "required"))
}
