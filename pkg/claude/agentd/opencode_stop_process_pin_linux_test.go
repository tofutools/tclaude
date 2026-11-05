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

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
)

// The Linux-only stopOpenCodeProcess pins: an escaped child is found through
// the /proc subtree recorded before the interrupt (macOS has no subtree
// enumeration), and the Unix control socket, a Linux-only transport, is
// removed only when the stopped tree is gone.

// stubbornOpenCodeScript serves health like healthyOpenCodeScript, but ignores
// the interrupt and keeps a child in its own session (outside the server's
// process group, as an escaped descendant would be), whose PID it writes to
// the file named by PIN_CHILD_FILE.
const stubbornOpenCodeScript = `#!/bin/sh
exec python3 -c '
import http.server, os, signal, subprocess, sys
signal.signal(signal.SIGINT, signal.SIG_IGN)
child = subprocess.Popen(["sleep", "60"], start_new_session=True)
open(os.environ["PIN_CHILD_FILE"], "w").write(str(child.pid))
port = int(sys.argv[sys.argv.index("--port") + 1])
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = b"{\"healthy\":true}"
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a):
        pass
http.server.HTTPServer(("127.0.0.1", port), H).serve_forever()
' "$@"
`

func TestStopOpenCodeProcess_AServerIgnoringInterruptIsKilledWithItsChild(t *testing.T) {
	requirePython(t)
	childFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("PIN_CHILD_FILE", childFile)
	runtime, authority := openCodeProcessPin(t, stubbornOpenCodeScript)
	process, err := startOpenCodeProcessWithAuthority(&runtime, nil, authority)
	require.NoError(t, err)
	raw, err := os.ReadFile(childFile)
	require.NoError(t, err)
	child, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	require.NoError(t, err)
	killOnCleanup(t, runtime.PID, child)

	stopOpenCodeProcess(runtime, process)

	eventuallyDead(t, runtime.PID, "the server that ignored the interrupt was killed")
	eventuallyDead(t, child, "the escaped child recorded before the interrupt was killed too")
	assert.Nil(t, registeredOpenCodeProcess(runtime.SessionID))
}

// unixRelayPinRuntime is a recovered Unix-relay runtime for pid, with a real
// control socket whose identity it records.
func unixRelayPinRuntime(t *testing.T, pid int) db.OpenCodeRuntime {
	t.Helper()
	short, err := os.MkdirTemp("/tmp", "tcl-stop-pin-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	parent := filepath.Join(short, "agt_cccccccccccccccccccccccccccccccc")
	require.NoError(t, os.Mkdir(parent, 0o700))
	path := filepath.Join(parent, "control.sock")
	listener, device, inode, err := opencodeapi.CreateUnixListener(path)
	require.NoError(t, err)
	listener.SetUnlinkOnClose(false)
	require.NoError(t, listener.Close())
	return db.OpenCodeRuntime{
		SessionID: "spwn-pin-unix-stop", PID: pid, ServerURL: "http://opencode.invalid",
		Transport: db.OpenCodeTransportUnixRelay, ControlSocketPath: path,
		ControlSocketDevice: device, ControlSocketInode: inode,
	}
}

// The control socket is the authority a later stop or relaunch needs; it is
// released only once the stopped process tree is gone.
func TestStopOpenCodeProcess_KeepsTheControlSocketOnlyWhileTheTreeSurvives(t *testing.T) {
	for _, tc := range []struct {
		name     string
		survives bool
	}{
		{"the tree is gone", false},
		{"the tree survives", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			useStopPinTmux(t, &stopPinTmux{})
			previous := openCodeRuntimeVerified
			openCodeRuntimeVerified = func(db.OpenCodeRuntime) bool { return true }
			t.Cleanup(func() { openCodeRuntimeVerified = previous })
			var pid int
			if tc.survives {
				// Killed but never reaped: a zombie still answers as alive, so
				// the stop cannot see the tree exit.
				cmd := exec.Command("sleep", "60")
				require.NoError(t, cmd.Start())
				t.Cleanup(func() { _ = cmd.Wait() })
				require.NoError(t, cmd.Process.Kill())
				pid = cmd.Process.Pid
			} else {
				pid = reapedSleeper(t)
			}
			runtime := unixRelayPinRuntime(t, pid)

			stopOpenCodeProcess(runtime, nil)

			_, err := os.Lstat(runtime.ControlSocketPath)
			if tc.survives {
				assert.NoError(t, err, "the control socket is kept while the tree survives")
			} else {
				assert.True(t, os.IsNotExist(err), "the control socket is removed: %v", err)
			}
		})
	}
}
