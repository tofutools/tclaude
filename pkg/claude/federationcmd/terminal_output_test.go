package federationcmd

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common/termtext"
)

// hostilePeerText is what a peer can still put in its text after ingress
// strips C0/C1, plus what ingress would have stripped, should a path skip it.
const hostilePeerText = "pay\x1b[2J\x1b]8;;https://evil.example\x1b\\me\x1b]8;;\x1b\\\x9b31m\rinvoice\u202egpj.exe"

func assertTerminalSafe(t *testing.T, out string) {
	t.Helper()
	for _, bad := range []string{"\x1b", "\x9b", "\r", "\u202e"} {
		assert.NotContains(t, out, bad, "output carries %q", bad)
	}
	assert.Equal(t, termtext.Clean(out), out)
}

// The federation commands print through cliStdout/cliStderr, cleaned on a
// terminal; here the remote operator's inbox, whose sender, subject and body
// a peer wrote.
func TestFederationInboxIsTerminalSafeOnATerminal(t *testing.T) {
	available, request := agent.DaemonAvailableImpl, agent.DaemonRequestImpl
	prevOut, prevErr := cliStdout, cliStderr
	t.Cleanup(func() {
		agent.DaemonAvailableImpl, agent.DaemonRequestImpl = available, request
		cliStdout, cliStderr = prevOut, prevErr
	})
	agent.DaemonAvailableImpl = func() bool { return true }
	agent.DaemonRequestImpl = func(_, _ string, _, out any, _ agent.DaemonOpts) error {
		type msg struct {
			ID        int64     `json:"id"`
			From      string    `json:"from"`
			Subject   string    `json:"subject,omitempty"`
			Body      string    `json:"body"`
			CreatedAt time.Time `json:"created_at"`
		}
		return remarshalInto([]msg{{ID: 3, From: hostilePeerText, Subject: hostilePeerText, Body: "line one\n" + hostilePeerText, CreatedAt: time.Now()}}, out)
	}
	var raw, errOut bytes.Buffer
	cliStdout, cliStderr = termtext.NewWriter(&raw), termtext.NewWriter(&errOut)
	cmd := inboxCmd()
	cmd.SetArgs([]string{})
	require.NoError(t, cmd.Execute())
	assertTerminalSafe(t, raw.String())
	assert.Contains(t, raw.String(), "line one\n", "a body keeps its lines")
	assert.Contains(t, raw.String(), "invoicegpj.exe")
}

// remarshalInto fills out, a daemon response, from v as the JSON client would.
func remarshalInto(v, out any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
