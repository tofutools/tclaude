package sandboxassumptions

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const assumptionsGateEnv = "TCLAUDE_SANDBOX_ASSUMPTIONS"

func runCommand(t *testing.T, cmd *exec.Cmd, timeout time.Duration) {
	t.Helper()
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", cmd.Path, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s failed: %v\n%s", cmd.Path, err, output.String())
		}
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("%s timed out after %s\n%s", cmd.Path, timeout, output.String())
	}
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("canonicalize temp dir: %v", err)
	}
	return canonical
}
