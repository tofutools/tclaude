package agentd

import (
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Remote-control READBACK (the observe path). Claude Code exposes no
// programmatic readback of whether its built-in Remote Access is on, so the
// rest of tclaude tracks a best-known boolean (sessions.remote_control) and
// uses it to decide the toggle DIRECTION. That flag drifts whenever a human
// types /remote-control directly in the pane.
//
// This file closes that gap by OBSERVING the live pane instead of guessing.
// Claude Code renders a persistent footer pill — the literal "/rc" — in its
// status row while Remote Access is armed (operator-confirmed), as a clickable
// claude.ai/code hyperlink, and in red when the connection has failed. We
// capture the pane on demand and scan the footer band for that pill. The result
// answers the question tclaude could not before: not "what did we last inject",
// but "can I actually connect right now".
//
// Cost discipline: capturing a pane is a tmux subprocess + a parse, so it runs
// ONLY on explicit, rare actions — a `remote-control status` call, or just
// before a toggle picks its direction. It is NEVER on a poll path; the
// dashboard and every refresh loop keep reading the cheap tracked flag, which
// the observe path self-heals whenever it runs.
//
// CC-TUI coupling: reading the footer couples us to Claude Code's rendering
// (the pill token, its hyperlink, the red failed variant) — the same class of
// coupling as remoteControlDisableMenuKeys in remote_control.go. If a future CC
// build changes the footer, parseRemoteControlFooter is the single place to
// adjust. The load-bearing assumption is that the "/rc" pill is shown ONLY when
// Remote Access is armed (not as an always-present shortcut affordance); verify
// with a toggle-off check before trusting an `off` reading on a fresh CC build.

// rcObserved is the remote-control state read directly from a live pane's
// footer — distinct from tclaude's tracked best-known flag.
type rcObserved int

const (
	// rcUnknown: the pane couldn't be read (no live pane / capture error) or
	// the footer is indeterminate (e.g. the pane is too narrow for CC to draw
	// the pill, so its absence proves nothing). Callers keep the tracked flag.
	rcUnknown rcObserved = iota
	// rcSeenOff: the footer was read cleanly and wide enough, and no "/rc"
	// pill is present → Remote Access is off.
	rcSeenOff
	// rcSeenOn: the "/rc" pill is present in its normal (non-failed) form →
	// armed and reachable. This is the "yes, I can connect" answer.
	rcSeenOn
	// rcSeenFailed: the "/rc" pill is present but in its failed (red) form →
	// armed, but the connection isn't established, so you can't connect yet.
	rcSeenFailed
)

func (s rcObserved) String() string {
	switch s {
	case rcSeenOff:
		return "off"
	case rcSeenOn:
		return "on"
	case rcSeenFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// armed reports whether the observation says Remote Access is toggled ON,
// regardless of whether the connection succeeded. This is the bit that drives
// the toggle direction and the tracked-flag self-heal (a failed-but-armed pane
// is still "on" for the purpose of "which way should the next toggle go").
func (s rcObserved) armed() bool { return s == rcSeenOn || s == rcSeenFailed }

// rcObservation is the full result of reading a pane's footer.
type rcObservation struct {
	state rcObserved
	// sessionURL is the claude.ai/code link parsed from the pill's hyperlink
	// when armed — where you actually connect. Best-effort (empty if the
	// terminal didn't emit an OSC 8 hyperlink, or tmux didn't capture it).
	sessionURL string
	// note is a human-readable caveat, set mainly on rcUnknown / rcSeenFailed.
	note string
}

// rcFooterScanLines is how many trailing pane rows form the "footer band" we
// scan for the pill. Kept small so conversation text — which can itself mention
// "/rc" or "/remote-control" (this very investigation does) — well above the
// status row is never mistaken for the live indicator.
const rcFooterScanLines = 6

// rcMinConfidentWidth is the footer width (in columns, inferred from the widest
// captured row) below which an ABSENT pill is reported as `unknown` rather than
// `off`. Claude Code hides the pill when the pane is too narrow, so on a narrow
// (or empty, width 0) pane "no pill" doesn't prove "off". Heuristic — tune once
// CC's real hide-threshold is known; erring toward `unknown` keeps us from
// confidently reporting `off` for a pane that simply couldn't draw the pill.
const rcMinConfidentWidth = 50

// rcPillRightZone is how many columns from the right edge of a (wide) status row
// the "/rc" token must start within to count as the pill when it carries no
// trusted hyperlink. The real pill is right-aligned in a full-width status bar;
// this rejects a left-aligned "/rc" typed into the input box, or a transcript
// line that merely mentions it, when such a row falls inside the footer band.
const rcPillRightZone = 16

// rcSessionURLPrefix is the origin the footer pill links to. A captured pane
// includes agent-influenceable text, so we only TRUST (as the definitive armed
// signal) and only SURFACE to the operator a hyperlink under this exact origin —
// a crafted link to anywhere else is ignored.
const rcSessionURLPrefix = "https://claude.ai/code/"

// rcPillRe matches the "/rc" pill as a bounded token — preceded by whitespace
// or start-of-line and followed by whitespace, the failed suffix, or
// end-of-line — so a path fragment like "src/rc" or "doc/rce" never matches.
var rcPillRe = regexp.MustCompile(`(^|\s)/rc(\s|$)`)

// rcFailedTextRe matches the textual failed variant if CC spells it out
// ("/rc failed"). The colour-based failed variant is caught by pillRed.
var rcFailedTextRe = regexp.MustCompile(`/rc\s+failed\b`)

// osc8Re extracts the target URL of an OSC 8 hyperlink: ESC ] 8 ; ; URL ST,
// where ST is BEL (\x07) or ESC \. The pill is rendered as such a hyperlink to
// the claude.ai/code session.
var osc8Re = regexp.MustCompile("\x1b]8;;([^\x07\x1b]*)(?:\x07|\x1b\\\\)")

// oscStripRe removes any OSC sequence (ESC ] … ST) — used by stripANSI so the
// hyperlink wrapper doesn't survive into the plain-text scan while its visible
// label ("/rc") does.
var oscStripRe = regexp.MustCompile("\x1b][^\x07\x1b]*(?:\x07|\x1b\\\\)")

// csiStripRe removes CSI sequences (ESC [ … final), covering SGR colour codes
// and cursor moves, leaving the visible glyphs behind.
var csiStripRe = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)

// stripANSI removes OSC and CSI escape sequences, returning the visible text.
func stripANSI(s string) string {
	s = oscStripRe.ReplaceAllString(s, "")
	s = csiStripRe.ReplaceAllString(s, "")
	return s
}

// claudeSessionURL returns the first OSC 8 hyperlink target on the raw line
// whose origin is rcSessionURLPrefix, or "". Gating on the origin keeps a
// hyperlink crafted elsewhere in the (agent-influenceable) pane from being
// surfaced to the operator as a connect link, and makes "carries a claude.ai
// hyperlink" a trustworthy definitive armed signal.
func claudeSessionURL(rawLine string) string {
	for _, m := range osc8Re.FindAllStringSubmatch(rawLine, -1) {
		if len(m) >= 2 && strings.HasPrefix(m[1], rcSessionURLPrefix) {
			return m[1]
		}
	}
	return ""
}

// pillRightAligned reports whether the "/rc" token sits in the right margin of a
// WIDE status row — the geometry of the real footer pill. The row must be at
// least rcMinConfidentWidth wide (ignoring trailing blanks) and the token must
// start within rcPillRightZone columns of that trimmed right edge. Used to
// reject a "/rc" that landed in the footer band but is really input-box or
// transcript text (left-aligned, or on a short line). Callers gate this behind
// "has no trusted hyperlink", so a properly-linked pill is accepted regardless
// of alignment.
func pillRightAligned(plain string) bool {
	trimmed := strings.TrimRight(plain, " ")
	w := len([]rune(trimmed))
	if w < rcMinConfidentWidth {
		return false
	}
	before, _, found := strings.Cut(plain, "/rc")
	if !found {
		return false
	}
	col := len([]rune(before))
	return col >= w-rcPillRightZone
}

// pillRed reports whether the visible "/rc" pill on the raw line is drawn with a
// red foreground — SCOPED to the pill, by replaying the line's SGR state up to
// the point the "/rc" glyphs appear. Scoping matters: a red element elsewhere on
// the status row (e.g. a high-context usage bar) must NOT make a healthy pill
// read as failed. Recognises standard (31/91), 256-colour and truecolour reds.
func pillRed(rawLine string) bool {
	red := false
	s := rawLine
	for len(s) > 0 {
		if s[0] == 0x1b {
			n := escSeqLen(s)
			if n <= 0 {
				n = 1
			}
			if params, ok := sgrParams(s[:n]); ok {
				red = foldRed(red, params)
			}
			s = s[n:]
			continue
		}
		if strings.HasPrefix(s, "/rc") {
			return red
		}
		s = s[1:]
	}
	return red
}

// escSeqLen returns the byte length of the terminal escape sequence beginning at
// s[0] (assumed ESC): a CSI (ESC [ … final byte 0x40-0x7e), an OSC (ESC ] … ST,
// where ST is BEL or ESC \), or a 2-byte ESC-x. Returns 0 when s doesn't start
// with ESC. Used to step pillRed over escapes without scanning inside them (so a
// "/rc" inside an OSC hyperlink URL is never mistaken for the visible pill).
func escSeqLen(s string) int {
	if len(s) == 0 || s[0] != 0x1b {
		return 0
	}
	if len(s) < 2 {
		return 1
	}
	switch s[1] {
	case '[': // CSI
		i := 2
		for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
			i++
		}
		if i < len(s) {
			i++ // include the final byte
		}
		return i
	case ']': // OSC, terminated by BEL or ESC \
		i := 2
		for i < len(s) {
			if s[i] == 0x07 {
				return i + 1
			}
			if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
			i++
		}
		return len(s)
	default:
		return 2 // ESC x
	}
}

// sgrParams returns the parameter list of an SGR sequence (ESC [ … m), and ok.
func sgrParams(seq string) (string, bool) {
	if len(seq) >= 3 && seq[0] == 0x1b && seq[1] == '[' && seq[len(seq)-1] == 'm' {
		return seq[2 : len(seq)-1], true
	}
	return "", false
}

// foldRed applies one SGR parameter list to the running red-foreground state:
// 0/39/"" reset it, 31/91 set it, and 38;5;N / 38;2;R;G;B set it when the
// extended colour is red-ish. Other params leave it unchanged.
func foldRed(cur bool, params string) bool {
	parts := strings.Split(params, ";")
	if params == "" {
		parts = []string{"0"} // bare ESC[m is a reset
	}
	for i := 0; i < len(parts); i++ {
		switch parts[i] {
		case "", "0", "39":
			cur = false
		case "31", "91":
			cur = true
		case "38":
			if i+2 < len(parts) && parts[i+1] == "5" {
				cur = isRed256(parts[i+2])
				i += 2
			} else if i+4 < len(parts) && parts[i+1] == "2" {
				cur = isRedRGB(parts[i+2], parts[i+3], parts[i+4])
				i += 4
			}
		}
	}
	return cur
}

// isRed256 reports whether a 256-colour palette index is a recognisable red
// (standard reds + the pure-red column of the 6×6×6 cube + common light reds).
func isRed256(s string) bool {
	switch s {
	case "1", "9", "52", "88", "124", "160", "196", "197", "203", "210", "167":
		return true
	}
	return false
}

// isRedRGB reports whether a truecolour triple reads as red: a high red channel
// with low green and blue.
func isRedRGB(rs, gs, bs string) bool {
	r, errR := strconv.Atoi(rs)
	g, errG := strconv.Atoi(gs)
	b, errB := strconv.Atoi(bs)
	if errR != nil || errG != nil || errB != nil {
		return false
	}
	return r >= 150 && g <= 100 && b <= 100
}

// capturePane snapshots the visible content of a tmux pane target
// ("session:0.0"), preserving escape sequences (-e, for colour + the OSC 8
// hyperlink) and joining wrapped lines (-J, so the pill/link isn't split across
// a soft wrap). Goes through the same clcommon.Default seam every other tmux
// call uses, so flow tests intercept it via TmuxSim. A dead/missing pane
// surfaces as a non-nil error (the command exits non-zero).
func capturePane(paneTarget string) (string, error) {
	out, err := clcommon.Default.Command("capture-pane", "-p", "-e", "-J", "-t", clcommon.ExactTarget(paneTarget)).Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// observeRemoteControl captures the target pane on demand and scans its footer
// for Claude Code's "/rc" pill, returning the OBSERVED state. Callers use it to
// answer `status` truthfully, self-heal the tracked flag, and pick the toggle
// direction. NEVER call this on a poll path — it forks a tmux subprocess.
func observeRemoteControl(paneTarget string) rcObservation {
	raw, err := capturePane(paneTarget)
	if err != nil {
		return rcObservation{state: rcUnknown, note: "could not read the pane (capture-pane failed): " + err.Error()}
	}
	return parseRemoteControlFooter(raw)
}

// parseRemoteControlFooter scans the bottom band of a captured pane for the
// "/rc" remote-control pill and classifies the state. Split out from
// observeRemoteControl so it is unit-testable against hand-crafted captures
// (off / on / failed / narrow / decoy-text) with no tmux involved.
func parseRemoteControlFooter(raw string) rcObservation {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")

	// Trim trailing blank rows, then take the last rcFooterScanLines rows as
	// the footer band — the only region where the live pill can appear.
	end := len(lines)
	for end > 0 && strings.TrimSpace(stripANSI(lines[end-1])) == "" {
		end--
	}
	start := max(end-rcFooterScanLines, 0)
	band := lines[start:end]

	width := 0
	sawPill := false
	failed := false
	sessionURL := ""
	for _, rawLine := range band {
		plain := stripANSI(rawLine)
		if w := len([]rune(plain)); w > width {
			width = w
		}
		if !rcPillRe.MatchString(plain) {
			continue
		}
		// Distinguish the real footer pill from a "/rc" that merely landed in
		// the band (input box, transcript tail). A claude.ai/code hyperlink is
		// definitive; otherwise the token must be right-aligned on a wide row.
		url := claudeSessionURL(rawLine)
		if url == "" && !pillRightAligned(plain) {
			continue
		}
		sawPill = true
		if url != "" {
			sessionURL = url
		}
		if rcFailedTextRe.MatchString(plain) || pillRed(rawLine) {
			failed = true
		}
	}

	switch {
	case sawPill && failed:
		return rcObservation{
			state:      rcSeenFailed,
			sessionURL: sessionURL,
			note:       "the pane footer shows the remote-control pill in its FAILED (red) state — the agent is armed but the connection isn't established, so you likely cannot connect right now",
		}
	case sawPill:
		return rcObservation{state: rcSeenOn, sessionURL: sessionURL}
	case width < rcMinConfidentWidth:
		// Includes width 0 (empty capture, e.g. mid-redraw) and a too-narrow
		// pane: in both, an absent pill doesn't prove "off".
		note := fmt.Sprintf("the pane is only ~%d columns wide; Claude Code hides the /rc footer pill on a narrow pane, so its absence doesn't prove remote control is off — widen the pane or attach to confirm", width)
		if width == 0 {
			note = "the pane capture came back empty (it may have been mid-redraw), so remote-control state can't be confirmed from it — retry, or check the pane directly"
		}
		return rcObservation{state: rcUnknown, note: note}
	default:
		return rcObservation{state: rcSeenOff}
	}
}

// selfHealRemoteControl reconciles the tracked best-known flag with a confident
// pane observation: it writes only when they differ, logs the correction, and
// updates the in-memory row so the rest of the request sees the healed value.
// MUST be called only with a confident observation (never rcUnknown) — that is
// the caller's responsibility, kept there so this stays a dumb reconcile.
func selfHealRemoteControl(sess *db.SessionRow, observed bool) {
	if sess == nil || sess.RemoteControl == observed {
		return
	}
	if err := db.SetSessionRemoteControl(sess.ID, observed); err != nil {
		slog.Warn("remote-control self-heal write failed",
			"conv_id", sess.ConvID, "session_id", sess.ID, "observed", observed, "error", err)
		return
	}
	slog.Info("remote-control tracked flag self-healed from pane readback",
		"conv_id", sess.ConvID, "session_id", sess.ID, "was", sess.RemoteControl, "now", observed)
	sess.RemoteControl = observed
}
