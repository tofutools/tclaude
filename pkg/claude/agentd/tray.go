package agentd

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// trayApprovalSlotCount caps how many pending-approval rows we surface
// inline in the tray menu. Five is enough for the realistic burst (a
// human running a coordinated group will rarely have more than a
// handful blocked at once) and small enough that the menu stays
// scannable. Overflow is signalled via "+N more…" on the header.
const trayApprovalSlotCount = 5

// Tray poller cadence. trayTick is the base poll — small enough that a
// pending-approval popup flips the icon promptly. agentRefreshEvery
// throttles the agent-status aggregate (which shells out to `tmux ls`)
// to a slower sub-cadence so we don't spawn a tmux subprocess 5×/sec
// when nothing is changing. blinkEvery sets the blink half-period: the
// icon toggles green↔red every blinkEvery ticks while an agent is
// blocked on the human.
const (
	trayTick          = 200 * time.Millisecond
	agentRefreshEvery = 5 // every ~1s
	blinkEvery        = 2 // ~400ms half-period → ~800ms full blink cycle
)

// trayMode is the colour state pickTrayMode resolves the daemon into.
// renderTrayIcon turns a mode (+ the current blink phase) into the
// actual icon bytes. Splitting policy (mode) from rendering (bytes)
// keeps the decision pure and unit-testable, and lets the poller
// animate the blink without the policy knowing about timing.
type trayMode int

const (
	trayGreen  trayMode = iota // ≥1 agent working, or nothing to watch (default)
	trayYellow                 // at least one online agent, and all of them idle
	trayOrange                 // a sudo grant is active (passive reminder)
	trayBlink                  // an agent is blocked on the human (green↔red)
)

// agentTrayCounts is the aggregate over currently-online agents that
// pickTrayMode reduces to a colour. busy = working / main_agent_idle;
// awaiting = awaiting_permission / awaiting_input; errored = a turn
// that ended in error. online = busy + idle + awaiting + errored —
// only agents in a recognized LIVE status; an alive row with an
// exited/empty/unknown status is excluded (see countAgentStates).
type agentTrayCounts struct {
	online   int
	busy     int
	idle     int
	awaiting int
	errored  int
}

// approvalSlot is one pre-allocated submenu item the tray poller
// rebinds as the pending-approvals set changes. Pre-allocation matters:
// fyne.io/systray doesn't support reliably removing items at runtime
// on all platforms, so we create a fixed slate at onReady time and
// Hide/Show + SetTitle them as state evolves.
//
// currentID is read by the slot's click handler at click time, NOT
// captured by closure — slot bindings shift as approvals come and go,
// and we want the click to fire on the approval that's currently
// shown, not whatever was there when the click handler was wired.
type approvalSlot struct {
	item *systray.MenuItem
	mu   sync.Mutex
	id   string // current approval-id bound to this slot; "" when hidden
}

func (s *approvalSlot) setID(id string) {
	s.mu.Lock()
	s.id = id
	s.mu.Unlock()
}

func (s *approvalSlot) getID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// trayConfig is the data tray menu items need.
type trayConfig struct {
	SocketPath   string
	PopupBaseURL string
}

// runTrayBlocking blocks the calling goroutine on systray.Run. It must
// be called from the main goroutine: fyne.io/systray's package init
// pins the main thread, and the underlying GUI loops (Cocoa, GTK,
// Win32) all require the main thread.
//
// onQuit fires exactly once, when the user clicks "Quit" in the tray
// menu. SIGTERM/SIGINT-driven shutdown should invoke `systray.Quit()`
// directly to unblock this function — onQuit will not be invoked in
// that case (the caller already knows shutdown is in progress).
//
// On hosts without a system tray host (WSL without an SNI watcher,
// pure Wayland sessions, headless servers) systray's nativeStart will
// log a warning and the tray icon won't appear, but `Run` keeps
// blocking until `Quit` is called — so the daemon still works fine,
// it just doesn't have a visible tray entry.
func runTrayBlocking(cfg trayConfig, onQuit func()) {
	greenIcon := makeTrayIcon(color.RGBA{R: 30, G: 180, B: 30, A: 255})
	// Yellow flips on while a `--ask-human` popup is awaiting decision.
	// Returns to green once the human decides or the popup times out.
	// Picked an amber-yellow that reads distinctly from green even in
	// reduced-color taskbar themes.
	yellowIcon := makeTrayIcon(color.RGBA{R: 230, G: 180, B: 30, A: 255})
	// Orange flips on while at least one sudo grant is active somewhere.
	// Pure orange (255,140,0) reads distinctly from yellow but stays in
	// the same warm-warning family — the human is reminded that an
	// elevation window is open without a flashing red alarm. Yellow
	// (pending approval) takes priority so the human can never miss a
	// popup waiting on them.
	orangeIcon := makeTrayIcon(color.RGBA{R: 255, G: 140, B: 0, A: 255})
	// Red is the blink's "on" frame — the icon alternates green↔red
	// while an agent is blocked on the human (a CC permission prompt /
	// elicitation dialog, an errored turn, or a pending agentd approval
	// popup). A bright red so the blink reads clearly even against a
	// busy taskbar; it pairs with greenIcon as the "off" frame.
	redIcon := makeTrayIcon(color.RGBA{R: 220, G: 40, B: 40, A: 255})

	onReady := func() {
		systray.SetIcon(greenIcon)
		systray.SetTitle("")
		systray.SetTooltip("tclaude agentd")

		mTitle := systray.AddMenuItem("tclaude agentd", "")
		mTitle.Disable()
		systray.AddSeparator()

		dashAvailable := cfg.PopupBaseURL != ""
		mDash := systray.AddMenuItem("Open dashboard",
			"Open the agentd dashboard in your browser")
		if !dashAvailable {
			mDash.Disable()
		}
		// Quick declutter — the dashboard's whole-roster "unfocus all"
		// without the trip through the browser. Detaches every active
		// agent's terminal window; the agents themselves keep running.
		mUnfocusAll := systray.AddMenuItem("Unfocus all agents",
			"Detach every agent's terminal window — agents keep running")
		mReinstall := systray.AddMenuItem("Reinstall agent skills",
			"Run `tclaude setup --install-agent-skills`")
		mConfig := systray.AddMenuItem("Open config.json", config.ConfigPath())

		// Pending-approvals submenu — pre-allocated fixed slate so the
		// poller can Show/Hide + SetTitle without runtime add/remove
		// (which fyne.io/systray doesn't reliably support).
		mApprovalsHeader := systray.AddMenuItem("Pending approvals", "")
		mApprovalsHeader.Disable()
		mApprovalsHeader.Hide()
		approvalSlots := make([]*approvalSlot, trayApprovalSlotCount)
		for i := range approvalSlots {
			it := systray.AddMenuItem("", "Open this approval in the browser")
			it.Hide()
			slot := &approvalSlot{item: it}
			approvalSlots[i] = slot
			// One click goroutine per slot, reading the current id at
			// click time. Captures `slot` (stable address) — not `id`
			// — so rebinding works.
			go func(s *approvalSlot) {
				for range s.item.ClickedCh {
					id := s.getID()
					if id == "" || cfg.PopupBaseURL == "" {
						continue
					}
					// Open the dashboard deep-linked to this access request
					// (the Messages tab's "Access requests" folder). Mint a
					// one-shot dashboard init token for the cookie exchange —
					// in-process, the tray IS the daemon.
					url := cfg.PopupBaseURL + "/?init_token=" +
						mintInitToken(initScopeDashboard) + "&" + accessRequestDeepLinkQuery(id)
					if err := openBrowser(url); err != nil {
						slog.Warn("tray: open approval failed",
							"id", id, "error", err)
					}
				}
			}(slot)
		}
		// Separator below the approval submenu so it's visually grouped
		// even when collapsed. Always present; cheap.
		systray.AddSeparator()

		systray.AddSeparator()
		// Disabled info rows so the human can copy-paste socket / popup
		// addresses without dropping to a shell.
		if cfg.SocketPath != "" {
			ms := systray.AddMenuItem("socket: "+cfg.SocketPath, cfg.SocketPath)
			ms.Disable()
		}
		if cfg.PopupBaseURL != "" {
			mp := systray.AddMenuItem("popup:  "+cfg.PopupBaseURL, cfg.PopupBaseURL)
			mp.Disable()
		}
		systray.AddSeparator()

		mQuit := systray.AddMenuItem("Quit", "Stop tclaude agentd")

		// State poller. Resolves the daemon into a trayMode each tick and
		// flips the icon green → yellow → orange → blink(green↔red) as
		// agents change status, approval requests come and go, and sudo
		// grants open and close. trayTick is small enough that a popup
		// feels responsive without burning cycles when nothing is
		// happening. Quits when mQuit fires (the goroutine below closes
		// trayDone).
		//
		// Three things move on their own cadence here:
		//   - approvals + sudo are read every tick (cheap in-process /
		//     SQLite reads);
		//   - the agent-status aggregate is refreshed every
		//     agentRefreshEvery ticks — it shells out to `tmux ls`, which
		//     we don't want to spawn 5×/sec;
		//   - the blink phase flips every blinkEvery ticks, INDEPENDENT of
		//     state change, so a blocked-on-human icon keeps toggling even
		//     while the underlying counts hold steady.
		//
		// The same tick refreshes the pending-approvals submenu when the
		// id set changes: binds the first N slots to the oldest approvals
		// (longest-waiting popup at the top), hides any unused slots.
		trayDone := make(chan struct{})
		go func() {
			tk := time.NewTicker(trayTick)
			defer tk.Stop()
			var (
				agentCounts agentTrayCounts
				tickN       int
				blinkOn     bool
				lastIcon    []byte
				lastTooltip string
				lastIDs     []string
			)
			for {
				select {
				case <-trayDone:
					return
				case <-tk.C:
					if tickN%agentRefreshEvery == 0 {
						agentCounts = snapshotAgentTrayState()
					}
					pending := approvals.pendingCount()
					sudoActive, hint := snapshotSudoTrayState()
					summary := approvals.snapshot()
					ids := pendingIDs(summary)

					mode, tooltip := pickTrayMode(agentCounts, pending, sudoActive, hint)
					// Advance the blink phase only while blinking; reset to
					// the green ("off") frame otherwise so a state that
					// stops blinking doesn't freeze on the red frame.
					if mode == trayBlink {
						if tickN%blinkEvery == 0 {
							blinkOn = !blinkOn
						}
					} else {
						blinkOn = false
					}

					icon := renderTrayIcon(mode, blinkOn, greenIcon, yellowIcon, orangeIcon, redIcon)
					if !bytes.Equal(icon, lastIcon) {
						systray.SetIcon(icon)
						lastIcon = icon
					}
					if tooltip != lastTooltip {
						systray.SetTooltip(tooltip)
						lastTooltip = tooltip
					}
					// Refresh the approvals submenu when the id-set changes
					// (instant) OR, while requests stay pending, on the slow
					// sub-cadence so the relative "Xs ago" labels keep ticking
					// instead of freezing at whatever they read when the set
					// last changed.
					if !sliceEq(ids, lastIDs) || (len(ids) > 0 && tickN%agentRefreshEvery == 0) {
						refreshApprovalsSubmenu(mApprovalsHeader, approvalSlots, summary)
						lastIDs = ids
					}
					tickN++
				}
			}
		}()

		go func() {
			defer close(trayDone)
			for {
				select {
				case <-mDash.ClickedCh:
					if dashAvailable {
						// Mint a one-shot init token in-process — the tray
						// runs inside agentd, so it IS the human-side
						// daemon and needs no socket round-trip. The
						// dashboard `/` route exchanges the token for the
						// session cookie. See handleDashboardRoot.
						url := cfg.PopupBaseURL + "/?init_token=" + mintInitToken(initScopeDashboard)
						if err := openBrowser(url); err != nil {
							slog.Warn("tray: open dashboard failed", "error", err)
						}
					}
				case <-mUnfocusAll.ClickedCh:
					// Detach every active agent's window in the background —
					// the parallel tmux detaches shouldn't block the tray
					// event loop. Same fire-and-log shape as Reinstall.
					go func() {
						resp, err := unfocusAllAgentWindows()
						if err != nil {
							slog.Warn("tray: unfocus all agents failed", "error", err)
							return
						}
						slog.Info("tray: unfocused all agents",
							"targeted", resp.Targeted, "detached", resp.Detached,
							"no_window", resp.NoWindow, "failed", resp.Failed)
					}()
				case <-mReinstall.ClickedCh:
					go runReinstallSkills()
				case <-mConfig.ClickedCh:
					if err := openBrowser(config.ConfigPath()); err != nil {
						slog.Warn("tray: open config failed", "error", err)
					}
				case <-mQuit.ClickedCh:
					onQuit()
					systray.Quit()
					return
				}
			}
		}()
	}

	systray.Run(onReady, func() {})
}

// runReinstallSkills shells out to `tclaude setup --install-agent-skills`.
// Best-effort — the result is logged but the tray menu doesn't surface
// success/failure beyond the icon staying alive.
func runReinstallSkills() {
	cmd := exec.Command("tclaude", "setup", "--install-agent-skills", "-y")
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("tray: reinstall agent skills failed",
			"error", err, "output", string(out))
		return
	}
	slog.Info("tray: agent skills reinstalled")
}

// refreshApprovalsSubmenu rebinds the pre-allocated slot slate to the
// current pending-approval set. Slots beyond what's needed are hidden.
// The header surfaces an overflow count when there are more pending
// requests than slots ("Pending approvals (+2 more…)").
//
// Pure function over (header, slots, summary) — no global state. The
// poller calls it on every state-change tick.
func refreshApprovalsSubmenu(header *systray.MenuItem, slots []*approvalSlot, summary []pendingApprovalSummary) {
	if len(summary) == 0 {
		header.Hide()
		for _, s := range slots {
			s.setID("")
			s.item.Hide()
		}
		return
	}
	overflow := len(summary) - len(slots)
	if overflow > 0 {
		header.SetTitle(fmt.Sprintf("Pending approvals (+%d more…)", overflow))
	} else {
		header.SetTitle("Pending approvals")
	}
	header.Show()
	for i, s := range slots {
		if i >= len(summary) {
			s.setID("")
			s.item.Hide()
			continue
		}
		row := summary[i]
		label := formatApprovalSlotLabel(row)
		s.setID(row.ID)
		s.item.SetTitle(label)
		s.item.SetTooltip(fmt.Sprintf("perm=%s · conv=%s · id=%s",
			row.Perm, row.ConvTitle, row.ID))
		s.item.Show()
	}
}

// formatApprovalSlotLabel renders one menu-item label. Kept short so
// it fits typical tray-menu width: "<perm> · <who> · 24s ago".
// Conv-title falls back to a short id when no title is known.
func formatApprovalSlotLabel(row pendingApprovalSummary) string {
	who := row.ConvTitle
	if who == "" {
		who = shortApprovalID(row.ConvID)
	}
	age := time.Since(row.CreatedAt).Round(time.Second)
	return fmt.Sprintf("%s · %s · %s ago", row.Perm, who, age)
}

func shortApprovalID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// pendingIDs extracts just the IDs in order. Used by the poller to
// detect "same count, different request set" (rare but possible
// when one popup decides and another arrives within the 200ms window).
func pendingIDs(summary []pendingApprovalSummary) []string {
	out := make([]string, len(summary))
	for i, r := range summary {
		out[i] = r.ID
	}
	return out
}

func sliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// pickTrayMode resolves the daemon's current state into a trayMode +
// tooltip. Pure function over the aggregate counts so the policy can be
// unit-tested without spawning a real systray or the daemon SQLite; the
// poller maps the returned mode (+ the live blink phase) to icon bytes
// via renderTrayIcon.
//
// Priority (highest first):
//
//   - blink (green↔red) — at least one agent is BLOCKED ON THE HUMAN:
//     a CC permission prompt / elicitation dialog (awaiting), an errored
//     turn (needs attention), or a pending --ask-human approval popup.
//     Highest because the human is the bottleneck; missing it stalls an
//     agent. The three sources collapse into one "act now" signal.
//   - orange — at least one sudo grant is active somewhere. The human
//     should know an elevation window is open, but it's a passive
//     reminder, not a blocking interrupt.
//   - yellow — there is at least one online agent and ALL of them are
//     idle (the quiet state — nothing is working, nothing needs you).
//   - green  — at least one agent is working, or there are no online
//     agents at all (the default: nothing to flag).
//
// sudoExpiryHint is appended to the orange tooltip when non-empty
// (e.g. "soonest expires in 4m12s"); pickTrayMode doesn't format
// durations itself so the unit test can pin tooltip composition
// without time math.
func pickTrayMode(a agentTrayCounts, pending, sudoActive int, sudoExpiryHint string) (trayMode, string) {
	if pending > 0 || a.awaiting > 0 || a.errored > 0 {
		return trayBlink, blockedTooltip(pending, a.awaiting, a.errored)
	}
	if sudoActive > 0 {
		t := fmt.Sprintf("tclaude agentd · %d active sudo grant(s)", sudoActive)
		if sudoExpiryHint != "" {
			t += " · " + sudoExpiryHint
		}
		return trayOrange, t
	}
	// All online agents idle → yellow (the quiet state). Checked as
	// idle == online (not merely busy == 0) so a status that counts
	// toward online without being idle can never masquerade as idle.
	// For today's statuses, past the blink check awaiting == errored == 0
	// so online == busy + idle and the two forms agree; the explicit form
	// just doesn't depend on that invariant holding forever.
	if a.online > 0 && a.idle == a.online {
		return trayYellow, fmt.Sprintf("tclaude agentd · all %d agent(s) idle", a.idle)
	}
	if a.busy > 0 {
		return trayGreen, fmt.Sprintf("tclaude agentd · %d agent(s) working", a.busy)
	}
	return trayGreen, "tclaude agentd"
}

// blockedTooltip composes the blink-state tooltip from whichever
// "needs you" sources are non-zero, in the same order the icon priority
// implies: awaiting input, then errored turns, then approval popups.
func blockedTooltip(pending, awaiting, errored int) string {
	var parts []string
	if awaiting > 0 {
		parts = append(parts, fmt.Sprintf("%d awaiting input", awaiting))
	}
	if errored > 0 {
		parts = append(parts, fmt.Sprintf("%d errored", errored))
	}
	if pending > 0 {
		parts = append(parts, fmt.Sprintf("%d pending approval(s)", pending))
	}
	return "tclaude agentd · " + strings.Join(parts, ", ")
}

// renderTrayIcon maps a trayMode (plus the current blink phase) to the
// icon bytes the systray should display. For trayBlink the phase picks
// the frame: blinkOn → red, otherwise green. Every other mode is static.
func renderTrayIcon(mode trayMode, blinkOn bool, green, yellow, orange, red []byte) []byte {
	switch mode {
	case trayBlink:
		if blinkOn {
			return red
		}
		return green
	case trayOrange:
		return orange
	case trayYellow:
		return yellow
	default:
		return green
	}
}

// countAgentStates reduces the daemon's session rows + the live tmux set
// to per-status counts over ONLINE agents (one entry per conv, keyed on
// the most-recently-updated alive row — mirrors stateForConvIn /
// FindSessionsByConvID so the tray and the dashboard agree on each
// agent's status). Offline convs (no alive tmux row) are skipped: a dead
// agent's hook status is frozen and would otherwise mislabel the icon.
// Pure over its inputs so the counting can be unit-tested without tmux
// or SQLite.
func countAgentStates(rows []*db.SessionRow, alive map[string]struct{}) agentTrayCounts {
	type pick struct {
		status  string
		updated time.Time
	}
	best := map[string]pick{}
	for _, r := range rows {
		if r.ConvID == "" || r.TmuxSession == "" {
			continue
		}
		if _, ok := alive[r.TmuxSession]; !ok {
			continue
		}
		if cur, seen := best[r.ConvID]; !seen || r.UpdatedAt.After(cur.updated) {
			best[r.ConvID] = pick{status: r.Status, updated: r.UpdatedAt}
		}
	}
	var c agentTrayCounts
	for _, p := range best {
		// online counts only rows in a recognized LIVE status. An alive
		// tmux session whose status is exited/empty/unknown (e.g.
		// SessionEnd fired but tmux hasn't torn the session down yet) is
		// deliberately NOT counted: it must not tip the icon to the
		// "all idle" yellow when the agent isn't actually idle.
		switch p.status {
		case session.StatusWorking, session.StatusMainAgentIdle:
			c.busy++
			c.online++
		case session.StatusIdle:
			c.idle++
			c.online++
		case session.StatusAwaitingPermission, session.StatusAwaitingInput:
			c.awaiting++
			c.online++
		case session.StatusError:
			c.errored++
			c.online++
		}
	}
	return c
}

// snapshotAgentTrayState is the IO shell over countAgentStates: it takes
// one `tmux ls` (the live session set) plus the session rows and reduces
// them to the aggregate the tray colours off. Any error collapses to a
// zero aggregate (online == 0 → green) — a transient tmux/SQLite hiccup
// shouldn't strobe the icon. Called on the poller's slow sub-cadence
// (agentRefreshEvery) so the `tmux ls` subprocess isn't spawned 5×/sec.
func snapshotAgentTrayState() agentTrayCounts {
	alive, err := session.LiveTmuxSessions()
	if err != nil {
		return agentTrayCounts{}
	}
	rows, err := db.ListSessions()
	if err != nil {
		return agentTrayCounts{}
	}
	return countAgentStates(rows, alive)
}

// snapshotSudoTrayState polls db.ListAllActiveSudoGrants and returns
// (count, expiryHint) for the tray. expiryHint is "soonest expires in
// <duration>" when there's at least one grant, otherwise empty. DB
// errors collapse to (0, "") — a transient SQLite hiccup shouldn't
// flicker the icon orange when there's nothing to elevate.
func snapshotSudoTrayState() (int, string) {
	rows, err := db.ListAllActiveSudoGrants()
	if err != nil || len(rows) == 0 {
		return 0, ""
	}
	soonest := rows[0].ExpiresAt
	for _, g := range rows[1:] {
		if g.ExpiresAt.Before(soonest) {
			soonest = g.ExpiresAt
		}
	}
	rem := time.Until(soonest).Round(time.Second)
	if rem <= 0 {
		// All grants slipped past expires_at between the SELECT and
		// here (rare, but the partial index uses a wall-clock cutoff).
		// Drop the hint rather than render "expires in -1s".
		return len(rows), ""
	}
	return len(rows), fmt.Sprintf("soonest expires in %s", rem)
}

// makeTrayIcon returns a small filled-circle icon. PNG everywhere
// except Windows, which needs ICO-wrapped PNG for Shell_NotifyIcon.

func makeTrayIcon(c color.RGBA) []byte {
	pngBytes := makeIconPNG(c)
	if runtime.GOOS == "windows" {
		return pngToICO(pngBytes, 22)
	}
	return pngBytes
}

func makeIconPNG(c color.RGBA) []byte {
	size := 22
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	center := float64(size) / 2
	radius := float64(size)/2 - 1
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx := float64(x) - center + 0.5
			dy := float64(y) - center + 0.5
			if dx*dx+dy*dy <= radius*radius {
				img.Set(x, y, c)
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		// Encoding a 22x22 RGBA never fails in practice; an empty PNG
		// is harmless to systray (icon area will just be blank).
		return nil
	}
	return buf.Bytes()
}

// pngToICO wraps a PNG in a single-image ICO container. Windows
// Vista+ accepts PNG-inside-ICO at any size. size is the pixel
// dimension (use 0 to mean 256).
func pngToICO(pngBytes []byte, size int) []byte {
	var b uint8
	if size >= 256 {
		b = 0
	} else {
		b = uint8(size)
	}
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // type = ICO
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // image count
	buf.WriteByte(b)                                       // width
	buf.WriteByte(b)                                       // height
	buf.WriteByte(0)                                       // palette
	buf.WriteByte(0)                                       // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))           // planes
	_ = binary.Write(&buf, binary.LittleEndian, uint16(32))          // bpp
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(pngBytes))) // size
	_ = binary.Write(&buf, binary.LittleEndian, uint32(22))          // offset
	buf.Write(pngBytes)
	return buf.Bytes()
}
