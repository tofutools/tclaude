package agentd

import "testing"

// The screen samples are trimmed from real captures: the stdin-dead pane and
// the healthy teardown card were both logged by logSoftExitPaneState during
// the 2026-08-09 Copilot retire incident, and the typed-command screen is the
// state a healthy pane shows between the text send and its submit.
func TestCopilotExitCmdUnconsumed(t *testing.T) {
	const stdinDead = `● Startup context read and acknowledged. Ready to proceed in this group. | ❯ | ctrl+c again to exit Auto → gpt-5-mini`
	const teardownCard = `╭─╮╭─╮ Changes +0 -0 | ╰─╯╰─╯ AI Credits 0.51 (4m 37s) | ▔▔▔▔ Resume copilot --resume=7aef4edf-5e2e-4a24-972e-d1cb08d35dd5`
	const typedUnsubmitted = `❯ /exit [print] | @ files · # issues Auto`

	cases := []struct {
		name   string
		screen string
		want   bool
	}{
		// The signature this exists to catch: prompt on screen, typed
		// command nowhere — Copilot's keypress reader dropped the bytes.
		{"stdin-dead pane", stdinDead, true},
		// Teardown card has no input prompt; poking it with signal-exit taps
		// would SIGINT Copilot mid-write of its durable shutdown state.
		{"healthy teardown card", teardownCard, false},
		// The command visibly reached the input box: consumption is proven,
		// submission problems belong to the bounded re-injections.
		{"typed but unsubmitted", typedUnsubmitted, false},
		// Failed or non-Copilot captures must never arm the fallback.
		{"empty capture", "", false},
	}
	for _, tc := range cases {
		if got := copilotExitCmdUnconsumed(tc.screen, "/exit"); got != tc.want {
			t.Errorf("%s: copilotExitCmdUnconsumed = %v, want %v", tc.name, got, tc.want)
		}
	}
	if copilotExitCmdUnconsumed(stdinDead, "") {
		t.Errorf("empty exit command must never arm the fallback")
	}
}
