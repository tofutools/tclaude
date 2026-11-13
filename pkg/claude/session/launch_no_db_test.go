package session

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// launcherDatabaseUse records every database access made by the launcher's
// own code in this test process.
//
// A brokered launch test runs the launcher and a stand-in daemon in one
// process, so "who opened the database" is answered from the call stack: an
// access under the stand-in daemon's request handler is the daemon doing its
// job, and any other is the launcher reaching for a database it must not
// assume exists.
type launcherDatabaseUse struct {
	mu      sync.Mutex
	callers []string
}

func watchLauncherDatabaseUse(t *testing.T) *launcherDatabaseUse {
	t.Helper()
	use := &launcherDatabaseUse{}
	t.Cleanup(db.SetOpenGuard(func() error {
		buf := make([]byte, 32<<10)
		stack := string(buf[:runtime.Stack(buf, false)])
		if strings.Contains(stack, "(*fakeLaunchDaemon).ServeHTTP") {
			return nil
		}
		var frames []string
		for _, line := range strings.Split(stack, "\n") {
			if !strings.HasPrefix(line, "github.com/tofutools/tclaude/") ||
				strings.Contains(line, "db.Open(") || strings.Contains(line, "watchLauncherDatabaseUse") {
				continue
			}
			frames = append(frames, strings.TrimPrefix(
				strings.SplitN(line, "(", 2)[0], "github.com/tofutools/tclaude/pkg/claude/"))
			if len(frames) == 4 {
				break
			}
		}
		use.mu.Lock()
		use.callers = append(use.callers, strings.Join(frames, " < "))
		use.mu.Unlock()
		return nil
	}))
	return use
}

func (u *launcherDatabaseUse) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.callers...)
}

// A managed launch runs where there may be no database. From the moment it
// is handed a ticket, every database step it takes is a request to the
// daemon: this drives the real runNew, up to the tmux launch and through the
// unwind of a launch tmux refuses, and fails on any step it takes itself.
func TestManagedLaunchNeverOpensTheDatabaseItself(t *testing.T) {
	cases := []struct {
		name   string
		params NewParams
		// wantOp is a daemon request this kind of launch must make, so a case
		// cannot pass by quietly skipping the step it is here for.
		wantOp string
		// resume seeds a resumable conversation and resumes it.
		resume bool
	}{
		{name: "claude", wantOp: opExitMarkPending.name},
		{name: "codex", params: NewParams{Harness: harness.CodexName}, wantOp: opExitMarkPending.name},
		{name: "codex with a generated permission profile", params: NewParams{
			Harness: harness.CodexName, PermissionProfile: harness.CodexAgentProfile,
		}, wantOp: opCodexRegisterLaunchProfile.name},
		{name: "gemini with a launch name", params: NewParams{Harness: harness.GeminiName, Name: "named"},
			wantOp: opSetConvTitle.name},
		{name: "copilot", params: NewParams{Harness: harness.CopilotName}, wantOp: opExitMarkPending.name},
		{name: "claude resuming a conversation", resume: true, wantOp: opSessionsForConv.name},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(ResourceDelegationDirEnv, "")
			// A short home: the daemon listens on its canonical socket under
			// it, which a sandboxed launch insists on, and a unix socket path
			// has a short length limit.
			home, err := os.MkdirTemp("/tmp", "tcl-nodb-")
			require.NoError(t, err)
			t.Cleanup(func() { _ = os.RemoveAll(home) })
			t.Setenv("HOME", home)
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			t.Setenv(agentipc.SocketEnv, "")
			db.ResetForTest()
			t.Cleanup(db.ResetForTest)
			rec := &launchRecordingTmux{failNewSession: true}
			swapTmux(t, rec)
			prevCheck := ClaudeAncestorCheck
			ClaudeAncestorCheck = func() bool { return false }
			t.Cleanup(func() { ClaudeAncestorCheck = prevCheck })
			sock := agentipc.CanonicalSocketPath()
			require.NoError(t, os.MkdirAll(filepath.Dir(sock), 0o700))
			daemon := startFakeLaunchDaemon(t, sock)
			if tc.resume {
				daemon.convID = carryoverConvID
			}
			// The ticket arrives the way agentd hands it over; runNew adopts it.
			handTicketOnPipe(t, daemon.ticket)
			use := watchLauncherDatabaseUse(t)

			params := tc.params
			params.Label = "spwn-nodb-" + string(rune('a'+i))
			params.Dir = t.TempDir()
			if tc.resume {
				// Seeded by the daemon's side of the world, before the watch.
				restore := db.SetOpenGuard(nil)
				seedResumableConv(t, params.Dir, nil)
				restore()
				params.Resume = carryoverConvID
			}
			params.Detached = true
			params.ManagedLaunch = true
			err = runNew(&params)
			require.ErrorContains(t, err, "failed to create tmux session",
				"the launch must get as far as tmux, which this test's tmux refuses")

			var unexpected []string
			for _, caller := range use.seen() {
				// Known and still to be brokered: resolving the conversation
				// a resume names reads the conversation index. It belongs with
				// the harness conversation stores, which read harness files
				// in place as well, and moves with them.
				if tc.resume && strings.Contains(caller, "common.ResolveConvID") {
					continue
				}
				unexpected = append(unexpected, caller)
			}
			assert.Empty(t, unexpected, "the launcher touched the database itself")
			ops := daemon.seen()
			assert.Contains(t, ops, LaunchBeginOp)
			assert.Contains(t, ops, opSaveLaunchRow.name)
			assert.Contains(t, ops, opDeleteLaunchRow.name, "the refused launch's row was taken back through the daemon")
			assert.Contains(t, ops, tc.wantOp)
		})
	}
}
