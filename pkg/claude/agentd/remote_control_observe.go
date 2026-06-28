package agentd

import (
	"fmt"
	"log/slog"
	"regexp"
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
// pane "no pill" doesn't prove "off". Heuristic — tune once CC's real
// hide-threshold is known; erring toward `unknown` keeps us from confidently
// reporting `off` for a pane that simply couldn't draw the pill.
const rcMinConfidentWidth = 50

// rcPillRe matches the "/rc" pill as a bounded token — preceded by whitespace
// or start-of-line and followed by whitespace, the failed suffix, or
// end-of-line — so a path fragment like "src/rc" or "doc/rce" never matches.
var rcPillRe = regexp.MustCompile(`(^|\s)/rc(\s|$)`)

// rcFailedTextRe matches the textual failed variant if CC spells it out
// ("/rc failed"). The colour-based failed variant is caught by lineMarksRed.
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

// sgrRe captures the parameter list of an SGR sequence (ESC [ … m) so
// lineMarksRed can test it for a red foreground.
var sgrRe = regexp.MustCompile(`\x1b\[([0-9;]*)m`)

// stripANSI removes OSC and CSI escape sequences, returning the visible text.
func stripANSI(s string) string {
	s = oscStripRe.ReplaceAllString(s, "")
	s = csiStripRe.ReplaceAllString(s, "")
	return s
}

// firstOSC8URL returns the first OSC 8 hyperlink target on the raw line, or "".
func firstOSC8URL(rawLine string) string {
	m := osc8Re.FindStringSubmatch(rawLine)
	if len(m) >= 2 && m[1] != "" {
		return m[1]
	}
	return ""
}

// lineMarksRed reports whether the raw line sets a red foreground anywhere in
// its SGR codes (31 = red, 91 = bright red). Best-effort: 256-colour and
// truecolour reds aren't decoded — if CC ever renders the failed pill that way,
// extend this. A red footer line is how we tell the failed pill from the
// healthy one when CC conveys the difference by colour rather than text.
func lineMarksRed(rawLine string) bool {
	for _, m := range sgrRe.FindAllStringSubmatch(rawLine, -1) {
		for p := range strings.SplitSeq(m[1], ";") {
			if p == "31" || p == "91" {
				return true
			}
		}
	}
	return false
}

// capturePane snapshots the visible content of a tmux pane target
// ("session:0.0"), preserving escape sequences (-e, for colour + the OSC 8
// hyperlink) and joining wrapped lines (-J, so the pill/link isn't split across
// a soft wrap). Goes through the same clcommon.Default seam every other tmux
// call uses, so flow tests intercept it via TmuxSim. A dead/missing pane
// surfaces as a non-nil error (the command exits non-zero).
func capturePane(paneTarget string) (string, error) {
	out, err := clcommon.Default.Command("capture-pane", "-p", "-e", "-J", "-t", paneTarget).Output()
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
		sawPill = true
		if u := firstOSC8URL(rawLine); u != "" {
			sessionURL = u
		}
		if rcFailedTextRe.MatchString(plain) || lineMarksRed(rawLine) {
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
	case width > 0 && width < rcMinConfidentWidth:
		return rcObservation{
			state: rcUnknown,
			note:  fmt.Sprintf("the pane is only ~%d columns wide; Claude Code hides the /rc footer pill on a narrow pane, so its absence doesn't prove remote control is off — widen the pane or attach to confirm", width),
		}
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
