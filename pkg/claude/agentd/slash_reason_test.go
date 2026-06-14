package agentd

import "testing"

// slashReason is the audit label that lands in output.log next to every
// lifecycle-slash injection (most importantly /compact). These cases pin
// the three "where did that come from?" branches so the log stays a
// reliable record of who drove a compaction.
func TestSlashReason(t *testing.T) {
	const target = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const caller = "12345678-aaaa-bbbb-cccc-dddddddddddd"

	cases := []struct {
		name          string
		label, caller string
		want          string
	}{
		{"human/dashboard has no calling conv", "compact", "", "compact (human/dashboard)"},
		{"agent compacting itself", "compact", target, "self-compact"},
		{"cross-agent names the (8-char) caller", "compact", caller, "compact (caller=12345678)"},
		{"label is not hardcoded to compact", "rename", "", "rename (human/dashboard)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := slashReason(tc.label, tc.caller, target); got != tc.want {
				t.Errorf("slashReason(%q, %q, target) = %q, want %q",
					tc.label, tc.caller, got, tc.want)
			}
		})
	}
}
