package agentd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/cli"
	"github.com/tofutools/tclaude/pkg/selfupdate"
)

// The self-updater's pre-flight check parses `serve` flags with the real
// command tree and never runs it.
func TestCheckArgsParsesServeFlagsWithoutRunning(t *testing.T) {
	for _, tc := range []struct {
		args   []string
		code   int
		reason string
	}{
		{[]string{"serve", "--no-tray", "--dashboard-port", "8123", "--log-level=debug"}, 0, ""},
		{[]string{"serve", "--dashboard-port=8123", "-s", "/tmp/x.sock"}, 0, ""},
		{[]string{"serve", "--no-such-flag"}, selfupdate.CheckArgsRejected, "unknown flag: --no-such-flag"},
		{[]string{"serve", "--dashboard-port", "not-a-port"}, selfupdate.CheckArgsRejected, "invalid argument"},
		{[]string{"serve", "stray"}, selfupdate.CheckArgsRejected, "received 1"},
		{[]string{"no-such-command"}, selfupdate.CheckArgsRejected, "no-such-command"},
	} {
		var stderr bytes.Buffer
		code := cli.CheckArgs(&stderr, RootCmd(), tc.args)
		assert.Equal(t, tc.code, code, "%v: %s", tc.args, stderr.String())
		assert.Contains(t, stderr.String(), tc.reason, "%v", tc.args)
	}
}
