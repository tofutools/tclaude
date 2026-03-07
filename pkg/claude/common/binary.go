package common

import (
	"os"
	"os/exec"
	"strings"
)

// DetectTofuArgs returns the command args prefix needed to invoke a tclaude subcommand.
// Returns e.g. ["/path/to/tclaude"].
// Checks os.Executable() first, then looks for tclaude in PATH.
func DetectTofuArgs() []string {
	if path, err := os.Executable(); err == nil {
		return []string{path}
	}
	if p, err := exec.LookPath("tclaude"); err == nil {
		return []string{p}
	}
	return []string{"tclaude"}
}

// DetectTofuCmd returns the full shell command string for invoking a tclaude subcommand.
// E.g. DetectTofuCmd("session", "focus") → "tclaude session focus".
func DetectTofuCmd(subcommands ...string) string {
	args := append(DetectTofuArgs(), subcommands...)
	return strings.Join(args, " ")
}
