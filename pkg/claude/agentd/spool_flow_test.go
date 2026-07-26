package agentd_test

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Flow coverage for the experimental file-spool transport
// (TCLAUDE_EXPERIMENTAL_FILE_TRANSPORT): spawn-side provisioning, the
// daemon-side consumer serving the production /v1 mux, directory-binding
// identity, and the restart story (requests written while no consumer runs
// are served by the next consumer's startup scan).

// provisionSpool runs the production spawn-side provisioning for conv and
// returns the minted spool directory.
func provisionSpool(t *testing.T, conv string) string {
	t.Helper()
	t.Setenv(agentipc.FileTransportFlagEnv, "1")
	env := map[string]string{}
	require.NoError(t, session.ApplyAgentSpoolEnv(conv, env))
	dir := env[agentipc.SpoolEnv]
	require.NotEmpty(t, dir, "provisioning must export %s", agentipc.SpoolEnv)
	return dir
}

func spoolGet(t *testing.T, dir, path string) (int, []byte) {
	t.Helper()
	client := agent.NewSpoolHTTPClient(dir, 5*time.Second)
	resp, err := client.Get("http://_" + path)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode, body
}

// Scenario: a conv with a provisioned spool directory reaches the same /v1
// mux the socket serves, and the daemon derives its identity from the
// directory binding — /v1/whoami reports the bound conv, not the human.
func TestSpool_WhoamiCarriesBoundConvIdentity(t *testing.T) {
	f := newFlow(t)
	const conv = "spool-1111-2222-3333-4444"
	f.HaveConvWithTitle(conv, "spool-tester")
	dir := provisionSpool(t, conv)

	stop := agentd.StartSpoolConsumerForTest(agentipc.SpoolRoot(), 25*time.Millisecond)
	defer stop()

	status, body := spoolGet(t, dir, "/v1/whoami")
	require.Equal(t, http.StatusOK, status, "whoami over spool: %s", body)
	var who struct {
		IsHuman bool   `json:"is_human"`
		ConvID  string `json:"conv_id"`
		AgentID string `json:"agent_id"`
	}
	require.NoError(t, json.Unmarshal(body, &who))
	assert.False(t, who.IsHuman, "spool callers are agent-class, never the human")
	assert.Equal(t, conv, who.ConvID)
	assert.NotEmpty(t, who.AgentID, "talking over the spool enrolls the conv as an agent")
}

// Scenario: the restart story. A request written while NO consumer is
// running (daemon down) persists as a file and is served by the next
// consumer's startup scan — the client just keeps waiting.
func TestSpool_RequestSurvivesDaemonRestart(t *testing.T) {
	f := newFlow(t)
	const conv = "spool-5555-6666-7777-8888"
	f.HaveConvWithTitle(conv, "spool-restart")
	dir := provisionSpool(t, conv)

	// Publish a request with no consumer alive — this is "agentd is down".
	env := agentipc.SpoolRequest{Method: http.MethodGet, RequestURI: "/v1/info"}
	data, err := agentipc.EncodeSpoolRequest(env)
	require.NoError(t, err)
	reqPath := agentipc.SpoolEnvelopePath(agentipc.SpoolReqDir(dir), "restart-req")
	require.NoError(t, agentipc.WriteSpoolFile(reqPath, data))

	// "Restart" the daemon: a fresh consumer must pick the file up in its
	// startup scan.
	stop := agentd.StartSpoolConsumerForTest(agentipc.SpoolRoot(), 25*time.Millisecond)
	defer stop()

	respPath := agentipc.SpoolEnvelopePath(agentipc.SpoolRespDir(dir), "restart-req")
	var respData []byte
	require.Eventually(t, func() bool {
		b, err := os.ReadFile(respPath)
		if err != nil {
			return false
		}
		respData = b
		return true
	}, 5*time.Second, 10*time.Millisecond, "queued request must be served after restart")

	resp, err := agentipc.DecodeSpoolResponse(respData)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.Status)
	assert.Contains(t, string(resp.Body), `"idempotency":"v1"`,
		"the spool serves the same /v1/info the socket does")
}

// Scenario: identity comes from the daemon-side binding, not from anything
// inside the (agent-writable) spool directory. A directory with NO binding
// row gets no identity — the mux answers, but as an unconfirmed caller.
func TestSpool_UnboundDirectoryGetsNoIdentity(t *testing.T) {
	newFlow(t)
	t.Setenv(agentipc.FileTransportFlagEnv, "1")

	// Hand-craft a spool dir under the real root WITHOUT a binding row —
	// what an attacker who can create directories but not write SQLite
	// could do.
	id, err := agentipc.NewSpoolID()
	require.NoError(t, err)
	root := agentipc.SpoolRoot()
	dir := root + "/" + id
	require.NoError(t, os.MkdirAll(agentipc.SpoolReqDir(dir), 0o700))
	require.NoError(t, os.MkdirAll(agentipc.SpoolRespDir(dir), 0o700))

	stop := agentd.StartSpoolConsumerForTest(root, 25*time.Millisecond)
	defer stop()

	env := agentipc.SpoolRequest{Method: http.MethodGet, RequestURI: "/v1/whoami"}
	data, err := agentipc.EncodeSpoolRequest(env)
	require.NoError(t, err)
	reqPath := agentipc.SpoolEnvelopePath(agentipc.SpoolReqDir(dir), "rogue-req")
	require.NoError(t, agentipc.WriteSpoolFile(reqPath, data))

	// The consumer only serves BOUND directories, so no response may ever
	// appear; the request file just sits (until swept).
	time.Sleep(200 * time.Millisecond)
	respPath := agentipc.SpoolEnvelopePath(agentipc.SpoolRespDir(dir), "rogue-req")
	_, err = os.Stat(respPath)
	assert.True(t, os.IsNotExist(err), "an unbound spool directory must never be served")
}
