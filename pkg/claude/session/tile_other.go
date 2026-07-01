//go:build !darwin && !linux

package session

import "log/slog"

// platformTileWindows is a no-op on platforms without a window-tiling
// backend (Windows outside WSL, the *BSDs, …). Tiling is best-effort —
// an unsupported desktop simply leaves the focused windows wherever the
// OS placed them, exactly like the focus path degrades there.
func platformTileWindows(specs []TileSpec, _ TileOptions) {
	slog.Debug("window tiling not supported on this platform; leaving windows as-is",
		"count", len(specs), "module", "tile")
}
