package agentd

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"log/slog"
	"os/exec"
	"runtime"

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

		go func() {
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
