package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tmux client refuses an initial command whose packed argv exceeds
// ~16KB ("command too long", client.c). The launch bootstrap therefore rides
// in a private script — these tests pin the property that matters: the tmux
// argv stays O(1) no matter how large the bootstrap command grows (env
// exports, sandbox rules, worktree paths, launch prompts), and the guard
// trips with an actionable error rather than tmux's opaque one.

func launchArgv(tmuxSession, cwd, scriptPath string) []string {
	return []string{"new-session", "-d", "-s", tmuxSession, "-c", cwd, "sh", scriptPath}
}

func TestLaunchScriptKeepsTmuxArgvConstant(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	smallCmd := "exec claude"
	// ~1.2MB bootstrap — vastly beyond anything a real spawn assembles, and
	// two orders of magnitude over tmux's limit if it were inlined.
	hugeCmd := strings.Repeat("export SOME_VAR=some-value; ", 40000) + "exec claude"

	smallPath, cleanupSmall, err := writeLaunchScript(smallCmd)
	if err != nil {
		t.Fatalf("writeLaunchScript(small): %v", err)
	}
	defer cleanupSmall()
	hugePath, cleanupHuge, err := writeLaunchScript(hugeCmd)
	if err != nil {
		t.Fatalf("writeLaunchScript(huge): %v", err)
	}
	defer cleanupHuge()

	cwd := "/Users/example/git/some-repo-with-a-fairly-long-worktree-name"
	smallArgv := tmuxArgvBytes(launchArgv("spwn-abc123", cwd, smallPath))
	hugeArgv := tmuxArgvBytes(launchArgv("spwn-abc123", cwd, hugePath))

	// os.CreateTemp's random suffix length can differ by a few digits; the
	// point is the argv must not scale with the bootstrap size at all.
	if diff := hugeArgv - smallArgv; diff < -16 || diff > 16 {
		t.Fatalf("tmux argv scales with bootstrap size: small=%d huge=%d", smallArgv, hugeArgv)
	}
	if hugeArgv > 1024 {
		t.Fatalf("tmux argv unexpectedly large: %d bytes", hugeArgv)
	}
	if hugeArgv > tmuxClientArgvLimit {
		t.Fatalf("tmux argv %d over the %d-byte pre-flight bound", hugeArgv, tmuxClientArgvLimit)
	}

	// The script itself must carry the full command, self-delete first, and
	// stay private to the owner.
	raw, err := os.ReadFile(hugePath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	content := string(raw)
	if !strings.Contains(content, hugeCmd) {
		t.Fatalf("script does not carry the bootstrap command")
	}
	if !strings.Contains(content, "rm -f -- \"$0\"") {
		t.Fatalf("script is missing its self-delete line:\n%.200s", content)
	}
	if idx := strings.Index(content, "rm -f -- \"$0\""); idx > strings.Index(content, "exec claude") {
		t.Fatalf("self-delete must precede the bootstrap command")
	}
	info, err := os.Stat(hugePath)
	if err != nil {
		t.Fatalf("stat script: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("script permissions = %o, want 600", perm)
	}
}

func TestLaunchDetachedTmuxSessionPreflightRejectsOversizedArgv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// A pathological cwd alone can exceed the client limit; the pre-flight
	// must refuse before tmux is ever invoked, naming the sizes.
	hugeCwd := "/" + strings.Repeat("d", tmuxClientArgvLimit)
	err := launchDetachedTmuxSession("spwn-abc123", hugeCwd, "exec claude")
	if err == nil {
		t.Fatalf("expected pre-flight error for oversized argv")
	}
	if !strings.Contains(err.Error(), "command too long") || !strings.Contains(err.Error(), "launch dir") {
		t.Fatalf("pre-flight error not actionable: %v", err)
	}
	// The refused launch must not leak its script.
	dir := filepath.Join(os.Getenv("HOME"), ".tclaude", "data", "launch-scripts")
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("refused launch leaked %d script(s)", len(entries))
	}
}

func TestWriteLaunchScriptSweepsStaleScripts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Seed one stale script (a pane that died before its self-delete ran)
	// and one fresh one; the next launch sweeps only the stale one.
	first, cleanupFirst, err := writeLaunchScript("exec claude")
	if err != nil {
		t.Fatalf("writeLaunchScript: %v", err)
	}
	defer cleanupFirst()
	stale := time.Now().Add(-launchScriptStaleAfter - time.Minute)
	if err := os.Chtimes(first, stale, stale); err != nil {
		t.Fatalf("backdate script: %v", err)
	}

	second, cleanupSecond, err := writeLaunchScript("exec claude")
	if err != nil {
		t.Fatalf("writeLaunchScript: %v", err)
	}
	defer cleanupSecond()

	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("stale script not swept: %v", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("fresh script must survive the sweep: %v", err)
	}
}
