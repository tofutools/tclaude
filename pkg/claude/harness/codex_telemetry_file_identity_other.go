//go:build !linux && !darwin

package harness

import "os"

// Unsupported platforms retain the size/mtime/anchor guards. Native Windows
// is not a tclaude target; keeping this fallback preserves package buildability.
func codexTelemetryFileIdentity(os.FileInfo) (uint64, uint64, bool) {
	return 0, 0, false
}
