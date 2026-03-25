package common

import (
	"os"
	"os/exec"
	"strings"
)

// absolutePaths controls whether DetectArgs returns absolute paths.
// When false (default), prefers bare "tclaude" if it's found on PATH.
var absolutePaths bool

// SetAbsolutePaths controls whether DetectArgs returns absolute paths to tclaude.
// When false (default), bare "tclaude" is used if it's on PATH.
func SetAbsolutePaths(v bool) {
	absolutePaths = v
}

// DetectArgs returns the command args prefix needed to invoke a tclaude subcommand.
// By default, returns ["tclaude"] if tclaude is on PATH.
// When absolutePaths is set, returns the full path e.g. ["/path/to/tclaude"].
func DetectArgs() []string {
	if !absolutePaths {
		if _, err := exec.LookPath("tclaude"); err == nil {
			return []string{"tclaude"}
		}
	}
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
