package conv

// learnshots_test.go renders the `tclaude conv watch -g` screenshot used in
// the Learn tutorials, from the real watch view over fake conversations (no
// real names, paths or content). Gated so CI never launches a browser:
//
//	TCLAUDE_DOCSHOTS=1 go test ./pkg/claude/conv/ -run TestLearnShots -v -count=1
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

	now := time.Now()
	ago := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339) }
	const project = "/home/dev/code/my-project"
	const site = "/home/dev/code/website"
	m := initialWatchModel(true, "", "")
	m.width, m.height = 136, 16
	m.viewportHeight = 8
	m.entries = []SessionEntry{
		{SessionID: "1ea4a11c-0000-4000-8000-000000000001", ProjectPath: project, Harness: "claude", GitBranch: "main",
			CustomTitle: "alice", FirstPrompt: "Ask your teammate bob what this project's main entry point is.", MessageCount: 14, FileSize: 48_210, Created: ago(26 * time.Hour), Modified: ago(25 * time.Hour)},
		{SessionID: "1ea40b0b-0000-4000-8000-000000000002", ProjectPath: project, Harness: "claude", GitBranch: "main",
			CustomTitle: "bob", FirstPrompt: "When a teammate messages you, answer in one or two sentences.", MessageCount: 9, FileSize: 31_877, Created: ago(26 * time.Hour), Modified: ago(25 * time.Hour)},
		{SessionID: "3c9e2f10-0000-4000-8000-000000000003", ProjectPath: project, Harness: "claude", GitBranch: "main",
			FirstPrompt: "Read this project and tell me in three sentences what it does.", MessageCount: 6, FileSize: 12_404, Created: ago(27 * time.Hour), Modified: ago(27 * time.Hour)},
		{SessionID: "7b21d4e8-0000-4000-8000-000000000004", ProjectPath: site, Harness: "codex", GitBranch: "fix-nav",
			FirstPrompt: "The mobile nav menu does not close after a click. Find out why.", MessageCount: 31, FileSize: 96_530, Created: ago(3 * time.Hour), Modified: ago(40 * time.Minute)},
		{SessionID: "a04f9c62-0000-4000-8000-000000000005", ProjectPath: site, Harness: "gemini", GitBranch: "main",
			FirstPrompt: "Suggest clearer copy for the pricing page headings.", MessageCount: 4, FileSize: 8_912, Created: ago(50 * time.Hour), Modified: ago(49 * time.Hour)},
		{SessionID: "e5d81b37-0000-4000-8000-000000000006", ProjectPath: project, Harness: "claude", GitBranch: "add-tests",
			FirstPrompt: "Add table tests for the config parser.", MessageCount: 22, FileSize: 64_118, Created: ago(5 * time.Hour), Modified: ago(2 * time.Hour)},
	}
	m.sortInPlace(m.entries)
	m.applySearchFilter()

	out, err := dashsnap.CaptureTerm(filepath.Join("..", "..", "..", "dashsnap-out", "learn-images"),
		"learn-conv-watch", "tclaude conv watch -g", m.View().Content, 1152, 330)
	if errors.Is(err, dashsnap.ErrBrowserUnavailable) {
		t.Skipf("environment: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s", out)
}
