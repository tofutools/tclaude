package agent

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/common/termtext"
)

// hostileText carries every shape of terminal control an agent- or
// peer-authored string can smuggle: ESC CSI, an OSC 8 hyperlink, OSC 52, a
// raw 8-bit C1 CSI, a UTF-8 encoded C1 CSI, a carriage return and a bidi
// override.
const hostileText = "pay\x1b[2J\x1b]8;;https://evil.example\x1b\\me\x1b]8;;\x1b\\" +
	"\x1b]52;c;cm0=\x07\x9b31m\u009b0m\rlater\u202egpj.exe"

// assertTerminalSafe fails when out still carries a byte or rune a terminal
// would interpret.
func assertTerminalSafe(t *testing.T, out string) {
	t.Helper()
	for _, bad := range []string{"\x1b", "\x9b", "\u009b", "\r", "\u202e", "\x07"} {
		assert.NotContains(t, out, bad, "output carries %q", bad)
	}
	assert.Equal(t, termtext.Clean(out), out)
}

func TestInboxListingIsTerminalSafe(t *testing.T) {
	var out bytes.Buffer
	rc := renderInbox(&inboxLsParams{}, []inboxEntry{
		{ID: 1, FromShort: hostileText, Group: hostileText, Subject: hostileText},
		{ID: 2, FromShort: "peer", Group: "g", Preview: hostileText, ParentID: 1},
	}, &out)
	require.Equal(t, rcOK, rc)
	assertTerminalSafe(t, out.String())
	assert.Contains(t, out.String(), "pay", "the readable text is kept")
}

func TestInboxReadIsTerminalSafeOnATerminal(t *testing.T) {
	prev := DaemonRequestImpl
	t.Cleanup(func() { DaemonRequestImpl = prev })
	DaemonRequestImpl = func(_, _ string, _, out any, _ DaemonOpts) error {
		return remarshal(map[string]any{
			"id": 42, "from": "conv", "from_title": hostileText, "group": hostileText,
			"subject": hostileText, "body": "line one\n" + hostileText + "\nline three",
		}, out)
	}
	// cliStdout on a terminal.
	var raw, errOut bytes.Buffer
	rc := runInboxReadDaemon(&inboxReadParams{ID: "42"}, 42, termtext.NewWriter(&raw), &errOut)
	require.Equal(t, rcOK, rc, errOut.String())
	assertTerminalSafe(t, raw.String())
	assert.Contains(t, raw.String(), "line one\n", "a body keeps its lines")
}

func TestInboxWatchMessageViewIsTerminalSafe(t *testing.T) {
	prev := DaemonRequestImpl
	t.Cleanup(func() { DaemonRequestImpl = prev })
	DaemonRequestImpl = func(_, _ string, _, out any, _ DaemonOpts) error {
		return remarshal(map[string]any{
			"id": 7, "from": hostileText, "subject": hostileText, "body": hostileText,
		}, out)
	}
	m := &inboxWatchModel{}
	loaded, ok := m.loadMessageCmd(7)().(inboxMessageLoadedMsg)
	require.True(t, ok)
	require.NoError(t, loaded.err)
	assertTerminalSafe(t, loaded.body)
}

// The inbox TUI quits on an inline message nudge and echoes it to the raw
// terminal it was given; the nudge carries the sender's subject and body.
func TestInboxWatchInlineNudgeEchoIsTerminalSafe(t *testing.T) {
	nudge := "[system: new agent message #9 from peer; delivery: inline] " + hostileText
	require.True(t, isInlineAgentMessageNudge(nudge))
	var out bytes.Buffer
	writeInboxWatchInlineNudge(&out, &inboxWatchModel{inlineNudge: nudge})
	assertTerminalSafe(t, out.String())
	assert.Contains(t, out.String(), "[system: new agent message #9")
}

// remarshal fills out, a daemon response, from payload as the JSON client
// would.
func remarshal(payload map[string]any, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
