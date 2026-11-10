package agentd_test

import (
	"bytes"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// `tclaude config get|set|clear --node bob`, end to end: the CLI goes through
// this node's peer proxy, and the scripted bob answers each request with the
// real receiving side, PeerViewHandler, serving this node's config under the
// grants given to bob.
func TestConfigNodeCLI(t *testing.T) {
	// The command is human-only; this test is the operator's terminal.
	for _, env := range []string{agentipc.SocketEnv, agentipc.CodexPermissionProfileEnv, agentipc.AgentHintEnvVar} {
		t.Setenv(env, "")
	}
	fh := newFedHarness(t)
	seedRemoteConfigSecrets(t)
	h := &peerProcessHarness{fedHarness: fh}
	bridgeAgentClientToMux(t, fh.f.Mux)
	receiver := agentd.PeerViewHandler(fh.peer.id.ID())

	// run runs one CLI invocation, answering each peer view it opens (a read,
	// then a change) with the receiving side.
	opened := 0
	var beforeView func(i int) // runs before the CLI's i-th view is served
	run := func(views int, args ...string) (int, string, string) {
		t.Helper()
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		done := make(chan int, 1)
		go func() { done <- agent.RunConfigCLI(args, stdout, stderr) }()
		for i := range views {
			conn := acceptOutboundPeerView(t, fh.peer, opened)
			if beforeView != nil {
				beforeView(i)
			}
			opened++
			wire := readPeerViewWire(t, conn)
			req := httptest.NewRequest(wire.Method, wire.URI, bytes.NewReader(wire.Body))
			for k, v := range wire.Header {
				req.Header[k] = v
			}
			rec := testharness.Serve(receiver, req)
			writePeerViewWire(t, conn, peerViewWire{Status: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()})
			_ = conn.Close()
		}
		return <-done, stdout.String(), stderr.String()
	}

	// Without node.config.read, the peer refuses the read.
	rc, out, errOut := run(1, "get", "--node", "bob")
	assert.Equal(t, 5, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, errOut, agentd.PermNodeConfigRead)

	// With it: every key shows its class and access, and the secret is
	// withheld, not shown.
	h.grant(t, agentd.PermNodeConfigRead, "")
	rc, out, errOut = run(1, "get", "--node", "bob")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, "trust: restricted")
	assert.Contains(t, out, "federation.invite = (withheld: set)  [local, read]")
	for _, secret := range remoteConfigSecrets {
		assert.NotContains(t, out, secret)
	}

	assert.Contains(t, out, "agent.http_proxies.search.header_value = (withheld: set)  [credentials, read]")

	rc, out, errOut = run(1, "get", "log_level", "--node", "bob")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, `log_level = "info"  [general, read]`)

	// A change under a class bob was not granted names the key and the grant.
	rc, out, errOut = run(2, "set", "log_level", "warn", "--node", "bob")
	assert.Equal(t, 5, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, errOut, "log_level (general): needs "+agentd.PermNodeConfigWrite)

	// With node.config.write it lands, through PATCH with the version.
	h.grant(t, agentd.PermNodeConfigWrite, "")
	rc, out, errOut = run(2, "set", "log_level", "warn", "--node", "bob")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, "Changed on bob: log_level")
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "warn", cfg.LogLevel)

	rc, out, errOut = run(1, "get", "log_level", "--node", "bob")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, `log_level = "warn"  [general, write]`)

	// A change made against a view the node changed since says re-read.
	beforeView = func(i int) {
		if i == 1 {
			_, err := config.Update(func(cfg *config.Config, _ error) error { cfg.LogLevel = "error"; return nil })
			require.NoError(t, err)
		}
	}
	rc, out, errOut = run(2, "set", "log_level", "debug", "--node", "bob")
	beforeView = nil
	assert.Equal(t, 4, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, errOut, "changed while this change was made; re-read it (tclaude config get --node bob)")
	cfg, err = config.Load()
	require.NoError(t, err)
	assert.Equal(t, "error", cfg.LogLevel, "the drifted change is not applied")

	// clear puts a key back to its default.
	rc, out, errOut = run(2, "clear", "log_level", "--node", "bob")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	cfg, err = config.Load()
	require.NoError(t, err)
	assert.Equal(t, "info", cfg.LogLevel, "log_level back to its default")

	// A secret inside a map is replaced by setting the map: empty keeps it,
	// a value replaces it.
	h.grant(t, agentd.PermNodeConfigCredentials, "")
	proxy := func(hv string) string {
		return `{"search":{"url":"https://api.example.com","header":"Authorization","header_value":"` + hv + `"}}`
	}
	rc, out, errOut = run(2, "set", "agent.http_proxies", proxy(""), "--node", "bob")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	cfg, err = config.Load()
	require.NoError(t, err)
	assert.Equal(t, "hv-s3cret", cfg.Agent.HTTPProxies["search"].HeaderValue, "a withheld secret sent back empty is kept")
	rc, out, errOut = run(2, "set", "agent.http_proxies", proxy("hv-new"), "--node", "bob")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, out, "agent.http_proxies")
	cfg, err = config.Load()
	require.NoError(t, err)
	assert.Equal(t, "hv-new", cfg.Agent.HTTPProxies["search"].HeaderValue)

	// The node-only class stays the node's, whatever the grants.
	rc, out, errOut = run(2, "clear", "federation.invite", "--node", "bob")
	assert.Equal(t, 5, rc, "out=%s err=%s", out, errOut)
	assert.Contains(t, errOut, "federation.invite (local): needs the node itself")
	cfg, err = config.Load()
	require.NoError(t, err)
	assert.Equal(t, "inv-s3cret", cfg.Federation.Invite)
}
