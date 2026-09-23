package agentd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestRunNonInteractiveSpawnShell(t *testing.T) {
	p := spawnParams{Harness: harness.ShellName, Cwd: t.TempDir(),
		InitialMessage: "printf 'hello\\n'; printf 'warning\\n' >&2; exit 7",
		GroupContext:   "this is prose and must not enter the shell command"}
	got, fail := runNonInteractiveSpawn(context.Background(), p, 30)
	if fail != nil {
		t.Fatalf("run failed: %+v", fail)
	}
	if got.Stdout != "hello\n" || got.Stderr != "warning\n" || got.ExitCode != 7 {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestRunNonInteractiveSpawnTimeout(t *testing.T) {
	p := spawnParams{Harness: harness.ShellName, Cwd: t.TempDir(), InitialMessage: "printf 'partial stderr\\n' >&2; sleep 5"}
	got, fail := runNonInteractiveSpawn(context.Background(), p, 1)
	if fail != nil {
		t.Fatalf("run failed: %+v", fail)
	}
	if got.ExitCode != 124 || !strings.Contains(got.Stderr, "timed out") || !strings.Contains(got.Stderr, "partial stderr") {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestRunNonInteractiveSpawnRemovesWriteProofBeforeChildStarts(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	const token = "one-shot-proof"
	marker := filepath.Join(real, dirWriteProofFilePrefix+token)
	if err := os.WriteFile(marker, []byte("proof"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := spawnParams{Harness: harness.ShellName, Cwd: real,
		InitialMessage:       "test ! -e " + marker + " && printf 'clean\\n'",
		CleanupDirWriteProof: true, DirWriteProofToken: token,
		DirWriteProofDirs: []string{real}}
	got, fail := runNonInteractiveSpawn(context.Background(), p, 30)
	if fail != nil || got.ExitCode != 0 || got.Stdout != "clean\n" {
		t.Fatalf("run result=%+v failure=%+v", got, fail)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("proof marker remains after run: %v", err)
	}
}

func TestRunNonInteractiveSpawnUsesHarnessInitialPrompt(t *testing.T) {
	bin := t.TempDir()
	path := filepath.Join(bin, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nfor arg do last=$arg; done\nprintf '%s' \"$last\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	p := spawnParams{Harness: harness.DefaultName, Cwd: t.TempDir(),
		InitialMessage: "task brief", GroupContext: "group guidance"}
	got, fail := runNonInteractiveSpawn(context.Background(), p, 30)
	if fail != nil {
		t.Fatalf("run failed: %+v", fail)
	}
	if got.ExitCode != 0 || got.Stdout != "group guidance\n\ntask brief" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestRunNonInteractiveSpawnClaudeLaunchSettings(t *testing.T) {
	bin := t.TempDir()
	path := filepath.Join(bin, "claude")
	const fake = "#!/bin/sh\nprintf '%s\\n' \"${CLAUDE_CODE_DISABLE_AUTO_MEMORY-unset}\"\nfor arg do case $arg in *crossSessionInbound*) printf 'peer-blocked\\n';; esac; done\n"
	if err := os.WriteFile(path, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct {
		autoMemory, peerMessaging bool
		want                      string
	}{
		{false, false, "1\npeer-blocked\n"},
		{true, true, "0\n"},
	} {
		p := spawnParams{Harness: harness.DefaultName, Cwd: t.TempDir(),
			InitialMessage: "task", AutoMemory: tc.autoMemory, PeerMessaging: tc.peerMessaging}
		got, fail := runNonInteractiveSpawn(context.Background(), p, 30)
		if fail != nil || got.ExitCode != 0 || got.Stdout != tc.want {
			t.Fatalf("settings autoMemory=%t peerMessaging=%t result=%+v failure=%+v", tc.autoMemory, tc.peerMessaging, got, fail)
		}
	}
}

func TestRunNonInteractiveSpawnShellTclaudeLayer(t *testing.T) {
	if err := session.TclaudeLayerServerHostAvailability(); err != nil {
		t.Skipf("tclaude layer unavailable: %v", err)
	}
	t.Setenv("HOME", t.TempDir())
	snapshot := sandboxpolicy.NewSnapshot(sandboxpolicy.EffectiveProfile{}, nil)
	p := spawnParams{Harness: harness.ShellName, Cwd: t.TempDir(),
		InitialMessage: "printf 'confined\\n'", SandboxImplementation: "tclaude-layer",
		EffectiveSandbox: &snapshot}
	got, fail := runNonInteractiveSpawn(context.Background(), p, 30)
	if fail != nil {
		t.Fatalf("run failed: %+v", fail)
	}
	if got.ExitCode != 0 || got.Stdout != "confined\n" {
		t.Fatalf("unexpected result: %+v", got)
	}
}
