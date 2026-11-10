package agentd

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// These pin startOpenCodeProcessWithAuthority's direct (non-tmux, unsandboxed)
// launch: a server that comes up healthy is returned, registered and recorded;
// one that exits during startup is reported with its stderr and leaves no
// registered process; and the endpoint and resource-cgroup refusals happen
// before anything is started. The Unix-relay authority handshake and the
// 12-second health deadline move as blocks: the first needs a sandboxed
// launcher, the second a server that listens without ever turning healthy.

// openCodeProcessPin writes an `opencode` stand-in and returns a loopback
// runtime for it, launched directly as CI does (no delegated tmux).
func openCodeProcessPin(t *testing.T, script string) (db.OpenCodeRuntime, openCodeLaunchAuthority) {
	t.Helper()
	setupTestDB(t)
	t.Setenv(session.ResourceDelegationDirEnv, "")
	t.Setenv("TMUX", "")
	executable := filepath.Join(testutil.CanonicalTempDir(t), "opencode")
	require.NoError(t, os.WriteFile(executable, []byte(script), 0o755))
	serverURL, err := allocateOpenCodeServerURL()
	require.NoError(t, err)
	return db.OpenCodeRuntime{
		SessionID: "spwn-pin-process", ServerURL: serverURL, Password: "pin-password",
		Cwd: testutil.CanonicalTempDir(t), Transport: db.OpenCodeTransportLoopbackTCP,
	}, openCodeLaunchAuthority{Executable: executable}
}

func registeredOpenCodeProcess(sessionID string) *openCodeProcess {
	openCodeProcesses.Lock()
	defer openCodeProcesses.Unlock()
	return openCodeProcesses.bySession[sessionID]
}

// healthyOpenCodeScript serves OpenCode's health endpoint on the --port it is
// given, from the launched PID itself, so the ownership proof holds. It serves
// from socketserver.TCPServer, not http.server.HTTPServer: HTTPServer looks up
// its own fully qualified name between bind and listen, and on a macOS runner
// that lookup can outlast the startup deadline, leaving a bound socket that
// never listens (tcl-tsa6ou).
const healthyOpenCodeScript = `#!/bin/sh
exec python3 -c '
import http.server, socketserver, sys
port = int(sys.argv[sys.argv.index("--port") + 1])
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = b"{\"healthy\":true}" if self.path == "/global/health" else b"{}"
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a):
        pass
socketserver.TCPServer(("127.0.0.1", port), H).serve_forever()
' "$@"
`

func TestStartOpenCodeProcess_AHealthyServerIsReturnedAndRecorded(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 serves the stand-in OpenCode health endpoint")
	}
	runtime, authority := openCodeProcessPin(t, healthyOpenCodeScript)

	process, err := startOpenCodeProcessWithAuthority(&runtime, nil, authority)

	require.NoError(t, err)
	require.NotNil(t, process)
	t.Cleanup(func() { stopOpenCodeProcess(runtime, process) })
	assert.Equal(t, process.rootPID(), runtime.PID, "the launched PID is recorded on the runtime")
	assert.Same(t, process, registeredOpenCodeProcess(runtime.SessionID), "the process is registered")
	assert.True(t, openCodeHealthy(runtime), "the returned server answers as healthy")
}

func TestStartOpenCodeProcess_AServerThatExitsDuringStartupIsReported(t *testing.T) {
	runtime, authority := openCodeProcessPin(t,
		"#!/bin/sh\necho 'pinned startup refusal' >&2\nexit 3\n")

	process, err := startOpenCodeProcessWithAuthority(&runtime, nil, authority)

	require.ErrorContains(t, err, "OpenCode server failed during startup: exit status 3: ")
	assert.Contains(t, err.Error(), "pinned startup refusal")
	assert.Nil(t, process)
	deadline := time.Now().Add(5 * time.Second)
	for registeredOpenCodeProcess(runtime.SessionID) != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	assert.Nil(t, registeredOpenCodeProcess(runtime.SessionID), "the exited process is deregistered")
}

func TestStartOpenCodeProcess_RefusesBeforeStarting(t *testing.T) {
	for _, tc := range []struct {
		name    string
		break_  func(*db.OpenCodeRuntime)
		refusal string
		is      error
	}{
		{"an endpoint without a port", func(r *db.OpenCodeRuntime) { r.ServerURL = "http://127.0.0.1" },
			"parse OpenCode server endpoint", nil},
		{"an unusable resource cgroup", func(r *db.OpenCodeRuntime) {
			r.ResourceCgroupDir = filepath.Join(t.TempDir(), "no-such-cgroup")
		}, "", errOpenCodeResourceCgroup},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launches := filepath.Join(t.TempDir(), "launches")
			runtime, authority := openCodeProcessPin(t,
				"#!/bin/sh\necho launch >> '"+launches+"'\nexit 3\n")
			tc.break_(&runtime)

			process, err := startOpenCodeProcessWithAuthority(&runtime, nil, authority)

			require.Error(t, err)
			if tc.refusal != "" {
				assert.ErrorContains(t, err, tc.refusal)
			}
			if tc.is != nil {
				assert.True(t, errors.Is(err, tc.is), "error %v is %v", err, tc.is)
			}
			assert.Nil(t, process)
			assert.NoFileExists(t, launches, "nothing was started")
			assert.Nil(t, registeredOpenCodeProcess(runtime.SessionID))
		})
	}
}
