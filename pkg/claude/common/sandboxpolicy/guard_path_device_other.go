//go:build !unix

package sandboxpolicy

import "io/fs"

// pathDevice has no portable answer off unix. Reporting the answer as
// unavailable makes the spelling probe refuse to draw a definitive "does not
// fold" conclusion it cannot justify, which is the safe direction for a guard.
// tclaude targets Linux and macOS; this exists so the package still builds.
func pathDevice(fs.FileInfo) (uint64, bool) { return 0, false }
