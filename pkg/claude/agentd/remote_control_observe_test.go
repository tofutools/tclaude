package agentd

import (
	"strings"
	"testing"
)

// esc helpers mirror the bytes Claude Code emits in its footer so the fixtures
// read like real captures.
func rcOSC8(url, label string) string {
	return "\x1b]8;;" + url + "\x1b\\" + label + "\x1b]8;;\x1b\\"
}
func rcRed(s string) string { return "\x1b[31m" + s + "\x1b[0m" }

// rcPad makes a row at least n columns wide so the width-confidence check treats
// an absent pill as a real "off" rather than "too narrow to tell".
func rcPad(s string, n int) string {
	for len([]rune(s)) < n {
		s += " "
	}
	return s
}

func TestParseRemoteControlFooter(t *testing.T) {
	const url = "https://claude.ai/code/019ec010-1111-2222-3333-444444444444"
	tests := []struct {
		name        string
		raw         string
		wantState   rcObserved
		wantURL     string
		wantNoteSub string // substring the note must contain ("" = skip)
	}{
		{
			name: "off: wide footer, no pill",
			raw: strings.Join([]string{
				"> waiting for input",
				"",
				rcPad("claude-opus  12%  7d", 60),
				"auto mode on (shift+tab to cycle)",
			}, "\n"),
			wantState: rcSeenOff,
		},
		{
			name: "on: plain /rc pill",
			raw: strings.Join([]string{
				"> waiting for input",
				"",
				rcPad("claude-opus  12%  7d", 55) + "/rc",
				"auto mode on (shift+tab to cycle)",
			}, "\n"),
			wantState: rcSeenOn,
		},
		{
			name: "on: /rc pill as OSC 8 hyperlink yields the session URL",
			raw: strings.Join([]string{
				"> waiting for input",
				"",
				rcPad("claude-opus  12%  7d", 55) + rcOSC8(url, "/rc"),
				"auto mode on (shift+tab to cycle)",
			}, "\n"),
			wantState: rcSeenOn,
			wantURL:   url,
		},
		{
			name: "failed: red /rc pill",
			raw: strings.Join([]string{
				"> waiting for input",
				"",
				rcPad("claude-opus  12%  7d", 55) + rcRed(rcOSC8(url, "/rc")),
				"auto mode on (shift+tab to cycle)",
			}, "\n"),
			wantState: rcSeenFailed,
			wantURL:   url,
		},
		{
			name: "failed: textual '/rc failed'",
			raw: strings.Join([]string{
				"> waiting for input",
				"",
				rcPad("claude-opus  12%  7d", 50) + "/rc failed",
				"auto mode on (shift+tab to cycle)",
			}, "\n"),
			wantState: rcSeenFailed,
		},
		{
			name: "unknown: narrow pane with no pill",
			raw: strings.Join([]string{
				"> input",
				"",
				"opus 12%",
				"auto mode",
			}, "\n"),
			wantState:   rcUnknown,
			wantNoteSub: "narrow",
		},
		{
			name:        "unknown: empty capture (mid-redraw)",
			raw:         "",
			wantState:   rcUnknown,
			wantNoteSub: "empty",
		},
		{
			name:        "unknown: all-blank capture",
			raw:         "\n\n\n",
			wantState:   rcUnknown,
			wantNoteSub: "empty",
		},
		{
			name: "off: a left-aligned '/rc' in the input box (within the band) is not the pill",
			raw: strings.Join([]string{
				"> /rc",
				rcPad("claude-opus  12%  7d", 60), // wide status row, no pill
				"auto mode on (shift+tab to cycle)",
			}, "\n"),
			wantState: rcSeenOff,
		},
		{
			name: "off: '/rc' mid-line on a wide row (not right-aligned) is not the pill",
			raw: strings.Join([]string{
				"> waiting for input",
				"the agent said /rc somewhere in the middle of this otherwise wide line of text",
				rcPad("claude-opus  12%  7d", 60),
				"auto mode on (shift+tab to cycle)",
			}, "\n"),
			wantState: rcSeenOff,
		},
		{
			name: "on: healthy pill survives a red usage bar earlier on the status row",
			raw: strings.Join([]string{
				"> waiting for input",
				"",
				"claude-opus  " + rcRed("████") + "  92%  " + rcPad("7d", 40) + "/rc",
				"auto mode on (shift+tab to cycle)",
			}, "\n"),
			wantState: rcSeenOn,
		},
		{
			name: "off: a '/rc' mention far ABOVE the footer is ignored",
			raw: strings.Join([]string{
				"user: please run /rc when you can", // decoy, well above the band
				"assistant: sure, toggling /rc now", // decoy
				"...",
				"...",
				"...",
				"...",
				"> waiting for input",
				"",
				rcPad("claude-opus  12%  7d", 60),
				"auto mode on (shift+tab to cycle)",
			}, "\n"),
			wantState: rcSeenOff,
		},
		{
			name: "off: '/rc' as a path fragment is not the pill",
			raw: strings.Join([]string{
				"> editing src/rc/config.go",
				"",
				rcPad("claude-opus  12%  7d", 60),
				"auto mode on (shift+tab to cycle)",
			}, "\n"),
			wantState: rcSeenOff,
		},
		{
			name: "on: trailing blank rows below the footer don't hide the pill",
			raw: strings.Join([]string{
				"> waiting for input",
				"",
				rcPad("claude-opus  12%  7d", 55) + "/rc",
				"auto mode on (shift+tab to cycle)",
				"",
				"",
			}, "\n"),
			wantState: rcSeenOn,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseRemoteControlFooter(tc.raw)
			if got.state != tc.wantState {
				t.Fatalf("state = %v, want %v (note=%q)", got.state, tc.wantState, got.note)
			}
			if got.sessionURL != tc.wantURL {
				t.Errorf("sessionURL = %q, want %q", got.sessionURL, tc.wantURL)
			}
			if tc.wantNoteSub != "" && !strings.Contains(got.note, tc.wantNoteSub) {
				t.Errorf("note = %q, want it to contain %q", got.note, tc.wantNoteSub)
			}
		})
	}
}

func TestStripANSI(t *testing.T) {
	const url = "https://claude.ai/code/abc"
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"\x1b[31mred\x1b[0m", "red"},
		{rcOSC8(url, "/rc"), "/rc"},
		{rcRed(rcOSC8(url, "/rc")), "/rc"},
		{"a\x1b[1;32mb\x1b[0mc", "abc"},
	}
	for _, c := range cases {
		if got := stripANSI(c.in); got != c.want {
			t.Errorf("stripANSI(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestClaudeSessionURL(t *testing.T) {
	const good = "https://claude.ai/code/xyz"
	if got := claudeSessionURL("status  " + rcOSC8(good, "/rc")); got != good {
		t.Errorf("claudeSessionURL = %q, want %q", got, good)
	}
	if got := claudeSessionURL("no hyperlink here /rc"); got != "" {
		t.Errorf("claudeSessionURL on plain text = %q, want empty", got)
	}
	// A hyperlink to any other origin is NOT trusted/surfaced — the captured
	// pane is agent-influenceable, so a crafted link must be ignored.
	if got := claudeSessionURL("evil " + rcOSC8("https://evil.example/phish", "/rc")); got != "" {
		t.Errorf("claudeSessionURL on a non-claude origin = %q, want empty", got)
	}
}

func TestPillRed(t *testing.T) {
	// Scoped to the pill: a red bar BEFORE a reset + healthy pill is not red.
	cases := []struct {
		name string
		line string
		want bool
	}{
		{"31m pill", "\x1b[31m/rc\x1b[0m", true},
		{"91m bright-red pill", "\x1b[1;91m/rc\x1b[0m", true},
		{"256-colour red pill", "\x1b[38;5;196m/rc\x1b[0m", true},
		{"truecolour red pill", "\x1b[38;2;220;30;30m/rc\x1b[0m", true},
		{"green pill", "\x1b[32m/rc\x1b[0m", false},
		{"uncoloured pill", "/rc", false},
		{"red elsewhere, healthy pill", "\x1b[31m####\x1b[0m  status  /rc", false},
		{"red bar then reset then linked pill", "\x1b[38;2;200;10;10m▓▓\x1b[0m " + rcOSC8("https://claude.ai/code/x", "/rc"), false},
	}
	for _, c := range cases {
		if got := pillRed(c.line); got != c.want {
			t.Errorf("%s: pillRed = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRCObservedHelpers(t *testing.T) {
	if !rcSeenOn.armed() || !rcSeenFailed.armed() {
		t.Error("on and failed must both count as armed")
	}
	if rcSeenOff.armed() || rcUnknown.armed() {
		t.Error("off and unknown must not count as armed")
	}
	for st, want := range map[rcObserved]string{
		rcSeenOn: "on", rcSeenFailed: "failed", rcSeenOff: "off", rcUnknown: "unknown",
	} {
		if got := st.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", st, got, want)
		}
	}
}
