package notify

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The osascript fallback must never let notification text become
// AppleScript source. Agent-authored values reach it (a notify-human body,
// a present-pr summary), so a value that closes the string literal would be
// arbitrary code execution as the daemon user — `do shell script` included.
func TestDarwinNotifyCommand_PassesUntrustedTextThroughArgv(t *testing.T) {
	// The historical breakout: escaping `"` without first escaping `\` let a
	// trailing backslash consume the added escape and terminate the string.
	const attack = `pr summary\" & (do shell script "touch /tmp/pwned") & "`

	cmd := darwinNotifyCommand("Claude: pull request", attack)

	require.Equal(t, "osascript", cmd.Path[strings.LastIndex(cmd.Path, "/")+1:])
	require.Equal(t,
		[]string{"osascript", "-e", darwinNotifyScript, darwinNotifySentinel, attack, "Claude: pull request"},
		cmd.Args,
		"title and body are separate argv entries, verbatim and unescaped")

	assert.NotContains(t, darwinNotifyScript, attack,
		"the script is a constant; untrusted text must not appear in it")
	assert.NotContains(t, darwinNotifyScript, "do shell script")
}

// osascript parses its own flags up to the first non-option argument, so a
// body that begins with "-e" must not sit directly behind the script or it
// would be read as another statement to execute.
func TestDarwinNotifyCommand_SentinelShieldsFlagLikeText(t *testing.T) {
	cmd := darwinNotifyCommand("-l JavaScript", `-e do shell script "touch /tmp/pwned"`)

	require.Len(t, cmd.Args, 6)
	assert.Equal(t, darwinNotifySentinel, cmd.Args[3],
		"a fixed non-flag argument ends option parsing before the untrusted values")
	assert.False(t, strings.HasPrefix(darwinNotifySentinel, "-"),
		"a sentinel that itself looks like a flag would defeat the purpose")
	assert.Equal(t, `-e do shell script "touch /tmp/pwned"`, cmd.Args[4])
	assert.Equal(t, "-l JavaScript", cmd.Args[5])
}
