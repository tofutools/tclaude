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
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// trayApprovalSlotCount caps how many pending-approval rows we surface
// inline in the tray menu. Five is enough for the realistic burst (a
// human running a coordinated group will rarely have more than a
// handful blocked at once) and small enough that the menu stays
// scannable. Overflow is signalled via "+N more…" on the header.
const trayApprovalSlotCount = 5

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

	onReady := func() {
		systray.SetIcon(greenIcon)
		systray.SetTitle("")
		systray.SetTooltip("tclaude agentd")

		mTitle := systray.AddMenuItem("tclaude agentd", "")
		mTitle.Disable()
		systray.AddSeparator()

		dashURL := ""
		if cfg.PopupBaseURL != "" {
			dashURL = cfg.PopupBaseURL + "/"
		}
		mDash := systray.AddMenuItem("Open dashboard", dashURL)
		if dashURL == "" {
			mDash.Disable()
		}
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
					url := cfg.PopupBaseURL + "/approve/" + id
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

		// Pending-approval + sudo-active poller. Flips
		// green ↔ orange ↔ yellow as approval requests come and go and
		// sudo grants open and close. 200ms tick is small enough that a
		// popup feels responsive without burning cycles when nothing is
		// happening. Quits when mQuit fires (the goroutine below closes
		// trayDone).
		//
		// Same tick also refreshes the pending-approvals submenu: takes
		// a fresh snapshot, binds the first N slots to the oldest
		// approvals (so the longest-waiting popup is at the top), hides
		// any unused slots. Always rebinds when the set changes — the
		// approval-id set isn't captured by lastPending alone (count
		// can stay the same while a request is replaced).
		trayDone := make(chan struct{})
		go func() {
			lastPending, lastSudo := 0, 0
			lastHint := ""
			var lastIDs []string
			tk := time.NewTicker(200 * time.Millisecond)
			defer tk.Stop()
			for {
				select {
				case <-trayDone:
					return
				case <-tk.C:
					pending := approvals.pendingCount()
					sudoActive, hint := snapshotSudoTrayState()
					summary := approvals.snapshot()
					ids := pendingIDs(summary)
					if pending == lastPending && sudoActive == lastSudo &&
						hint == lastHint && sliceEq(ids, lastIDs) {
						continue
					}
					icon, tooltip := pickTrayIcon(greenIcon, yellowIcon, orangeIcon,
						pending, sudoActive, hint)
					systray.SetIcon(icon)
					systray.SetTooltip(tooltip)
					refreshApprovalsSubmenu(mApprovalsHeader, approvalSlots, summary)
					lastPending, lastSudo, lastHint = pending, sudoActive, hint
					lastIDs = ids
				}
			}
		}()

		go func() {
			defer close(trayDone)
			for {
				select {
				case <-mDash.ClickedCh:
					if dashURL != "" {
						if err := openBrowser(dashURL); err != nil {
							slog.Warn("tray: open dashboard failed", "error", err)
						}
					}
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

// pickTrayIcon picks the icon + tooltip the tray should display for a
// given (pending-approval, sudo-active-grant) snapshot. Pure function
// so the policy can be unit-tested without spawning a real systray
// or the daemon SQLite.
//
// Priority (highest first):
//
//   - yellow — at least one --ask-human popup is open. Highest because
//     the human is BLOCKING; missing this stalls an agent.
//   - orange — at least one sudo grant is active somewhere. The human
//     should know an elevation window is open, but it's a passive
//     reminder, not a blocking interrupt.
//   - green  — idle.
//
// sudoExpiryHint is appended to the orange tooltip when non-empty
// (e.g. "soonest expires in 4m12s"); pickTrayIcon doesn't format
// durations itself so the unit test can pin tooltip composition
// without time math.
func pickTrayIcon(green, yellow, orange []byte, pending, sudoActive int, sudoExpiryHint string) ([]byte, string) {
	if pending > 0 {
		return yellow, fmt.Sprintf("tclaude agentd · %d pending approval(s)", pending)
	}
	if sudoActive > 0 {
		t := fmt.Sprintf("tclaude agentd · %d active sudo grant(s)", sudoActive)
		if sudoExpiryHint != "" {
			t += " · " + sudoExpiryHint
		}
		return orange, t
	}
	return green, "tclaude agentd"
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
