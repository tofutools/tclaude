package common

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// DetectTofuArgs returns the command args prefix needed to invoke a claude subcommand.
// Returns e.g. ["/path/to/tclaude"] or ["/path/to/tofu", "claude"].
// Checks os.Executable() first, then looks for tclaude then tofu in PATH.
func DetectTofuArgs() []string {
	if path, err := os.Executable(); err == nil {
		base := filepath.Base(path)
		if base == "tclaude" {
			return []string{path}
		}
		if base == "tofu" {
			return []string{path, "claude"}
		}
	}
	if p, err := exec.LookPath("tclaude"); err == nil {
		return []string{p}
	}
	if p, err := exec.LookPath("tofu"); err == nil {
		return []string{p, "claude"}
	}
	return []string{"tclaude"}
}

// DetectTofuCmd returns the full shell command string for invoking a claude subcommand.
// E.g. DetectTofuCmd("session", "focus") → "tclaude session focus" or "tofu claude session focus".
func DetectTofuCmd(subcommands ...string) string {
	args := append(DetectTofuArgs(), subcommands...)
	return strings.Join(args, " ")
}
