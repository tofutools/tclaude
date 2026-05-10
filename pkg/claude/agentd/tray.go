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
	"time"

	"fyne.io/systray"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

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

		// Pending-approval poller. Flips green↔yellow as approval requests
		// come and go. 200ms tick is small enough that a popup feels
		// responsive without burning cycles when nothing is happening.
		// Quits when mQuit fires (the goroutine below closes trayDone).
		trayDone := make(chan struct{})
		go func() {
			lastPending := 0
			tk := time.NewTicker(200 * time.Millisecond)
			defer tk.Stop()
			for {
				select {
				case <-trayDone:
					return
				case <-tk.C:
					n := approvals.pendingCount()
					if n == lastPending {
						continue
					}
					// Update on EVERY count change, not just the
					// 0↔non-zero transition, so the tooltip count
					// stays accurate when a second concurrent
					// approval queues behind the first.
					icon, tooltip := pickTrayIcon(greenIcon, yellowIcon, n)
					systray.SetIcon(icon)
					systray.SetTooltip(tooltip)
					lastPending = n
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

// makeTrayIcon returns a small filled-circle icon. PNG everywhere
// except Windows, which needs ICO-wrapped PNG for Shell_NotifyIcon.
// pickTrayIcon picks the icon + tooltip the tray should display for a
// given pending-approval count. Pure function so the policy can be
// unit-tested without spawning a real systray.
func pickTrayIcon(green, yellow []byte, pending int) ([]byte, string) {
	if pending > 0 {
		return yellow, fmt.Sprintf("tclaude agentd · %d pending approval(s)", pending)
	}
	return green, "tclaude agentd"
}

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
