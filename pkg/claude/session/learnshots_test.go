package session

// learnshots_test.go renders the `tclaude session watch` screenshot used in
// the Learn tutorials. It builds the real watch view from fake sessions (no
// tmux, no harness, no real names or paths), converts its ANSI output to a
// terminal-styled page and captures it with dashsnap. Gated so CI never
// launches a browser:
//
//	TCLAUDE_DOCSHOTS=1 go test ./pkg/claude/session/ -run TestLearnShots -v -count=1
//
// Output lands in dashsnap-out/learn-images/ (gitignored); run
// scripts/docs/learn-images.sh to regenerate and install every Learn image.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agentd/dashsnap"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestLearnShots(t *testing.T) {
	if os.Getenv("TCLAUDE_DOCSHOTS") == "" {
		t.Skip("docs screenshot generator — set TCLAUDE_DOCSHOTS=1 to run (needs a local headless Chrome)")
	}
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)

	const project = "/home/dev/code/my-project"
	now := time.Now()
	m := initialModel(false, nil, nil)
	m.width, m.height = 118, 14
	m.allSessions = []*SessionState{
		{ID: "1ea4a11c", ConvID: "1ea40000-0000-4000-8000-00000000a11c", Cwd: project, Harness: "claude",
			Status: StatusWorking, StatusDetail: "Read", Created: now.Add(-6 * time.Minute), Updated: now.Add(-2 * time.Second), LastHook: now.Add(-2 * time.Second)},
		{ID: "1ea40b0b", ConvID: "1ea40000-0000-4000-8000-000000000b0b", Cwd: project, Harness: "claude",
			Status: StatusIdle, Created: now.Add(-7 * time.Minute), Updated: now.Add(-40 * time.Second), LastHook: now.Add(-40 * time.Second)},
		{ID: "1ea4c0de", ConvID: "1ea40000-0000-4000-8000-00000000c0de", Cwd: "/home/dev/code/website", Harness: "codex",
			Status: StatusAwaitingPermission, StatusDetail: "Bash", Created: now.Add(-25 * time.Minute), Updated: now.Add(-15 * time.Second), LastHook: now.Add(-15 * time.Second)},
	}
	m.sessions = m.allSessions
	m.pendingNames = map[string]string{
		"1ea40000-0000-4000-8000-00000000a11c": "alice",
		"1ea40000-0000-4000-8000-000000000b0b": "bob",
		"1ea40000-0000-4000-8000-00000000c0de": "site-fix",
	}

	out, err := dashsnap.CaptureTerm(learnImagesDir(), "learn-session-watch", "tclaude session watch", m.View().Content, 1000, 262)
	if errors.Is(err, dashsnap.ErrBrowserUnavailable) {
		t.Skipf("environment: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", out)
}

// learnImagesDir is where every Learn image generator writes, so
// scripts/docs/learn-images.sh can pick them up in one place.
func learnImagesDir() string {
	return filepath.Join("..", "..", "..", "dashsnap-out", "learn-images")
}
