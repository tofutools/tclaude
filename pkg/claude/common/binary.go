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
	return DetectAbsoluteArgs()
}

// DetectAbsoluteArgs returns the absolute path to the tclaude binary.
// Use this when the command will be executed outside the user's normal shell
// environment (e.g. terminal-notifier -execute, protocol handlers) where
// PATH may be minimal.
func DetectAbsoluteArgs() []string {
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
//
// Each argument is shell-quoted, so a binary path containing spaces (e.g.
// os.Executable() resolving to "/Users/First Last/go/bin/tclaude") still emits
// a runnable POSIX shell command. Args without shell-special characters pass
// through unchanged, so the common case is byte-for-byte identical to a bare join.
func DetectCmd(subcommands ...string) string {
	return shellJoin(append(DetectArgs(), subcommands...))
}

// DetectAbsoluteCmd returns the full shell command string using absolute paths.
// Use this when the command will be executed outside the user's normal shell
// environment (e.g. terminal-notifier -execute). Arguments are shell-quoted; see
// DetectCmd for the rationale.
func DetectAbsoluteCmd(subcommands ...string) string {
	return shellJoin(append(DetectAbsoluteArgs(), subcommands...))
}

// shellJoin quotes each argument with ShellQuoteArg and joins them with spaces,
// producing a string safe to execute via a POSIX shell.
func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = ShellQuoteArg(a)
	}
	return strings.Join(quoted, " ")
}
