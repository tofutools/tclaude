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
		name       string
		raw        string
		wantState  rcObserved
		wantURL    string
		wantNarrow bool // expect the "too narrow" note on an unknown
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
			wantState:  rcUnknown,
			wantNarrow: true,
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
			if tc.wantNarrow && !strings.Contains(got.note, "narrow") {
				t.Errorf("expected a 'narrow' note on unknown, got %q", got.note)
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

func TestFirstOSC8URL(t *testing.T) {
	const url = "https://claude.ai/code/xyz"
	if got := firstOSC8URL("status  " + rcOSC8(url, "/rc")); got != url {
		t.Errorf("firstOSC8URL = %q, want %q", got, url)
	}
	if got := firstOSC8URL("no hyperlink here /rc"); got != "" {
		t.Errorf("firstOSC8URL on plain text = %q, want empty", got)
	}
}

func TestLineMarksRed(t *testing.T) {
	if !lineMarksRed("\x1b[31m/rc\x1b[0m") {
		t.Error("31m foreground should read as red")
	}
	if !lineMarksRed("\x1b[1;91m/rc\x1b[0m") {
		t.Error("91m bright-red foreground should read as red")
	}
	if lineMarksRed("\x1b[32m/rc\x1b[0m") {
		t.Error("32m (green) must not read as red")
	}
	if lineMarksRed("/rc") {
		t.Error("uncoloured text must not read as red")
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
