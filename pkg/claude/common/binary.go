package common

import (
	"os"
	"os/exec"
	"strings"
)

// DetectArgs returns the command args prefix needed to invoke a tclaude subcommand.
// Returns e.g. ["/path/to/tclaude"].
// Checks os.Executable() first, then looks for tclaude in PATH.
func DetectArgs() []string {
	if path, err := os.Executable(); err == nil {
		return []string{path}
	}
	if p, err := exec.LookPath("tclaude"); err == nil {
		return []string{p}
	}
	return []string{"tclaude"}
}

// DetectCmd returns the full shell command string for invoking a tclaude subcommand.
// E.g. DetectCmd("session", "focus") → "tclaude session focus".
func DetectCmd(subcommands ...string) string {
	args := append(DetectArgs(), subcommands...)
	return strings.Join(args, " ")
}
