package common

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// HookCallbackCommand is intentionally PATH-based. Harness hook files outlive
// any one tclaude installation path and run inside constructed roots where the
// trusted tclaude projection is placed first on PATH.
const HookCallbackCommand = "tclaude session hook-callback"

// absolutePaths controls whether DetectArgs returns absolute paths.
// When false (default), prefers bare "tclaude" if it's found on PATH.
var absolutePaths bool

// selfIsTclaude records whether the running executable is the tclaude CLI
// itself. It stays true for the tclaude binary, which is why the resolvers
// below can trust os.Executable(). Sibling binaries that link this code but
// answer to a different name — today, the standalone tclaude-agentd daemon —
// call MarkSelfNotTclaude at startup so that a "run tclaude with these
// subcommands" path never resolves to a re-invocation of themselves.
var selfIsTclaude = true

// MarkSelfNotTclaude declares that this process is not the tclaude CLI, so
// SelfTclaudePath must look for a real tclaude rather than returning the
// running executable. Call it once, before any command runs.
func MarkSelfNotTclaude() {
	selfIsTclaude = false
}

// SelfTclaudePath returns an absolute path to the tclaude binary to use when
// building a command line that runs a tclaude subcommand. In the tclaude
// binary that is simply the running executable, which keeps a session pinned
// to the exact build that started it. In a sibling binary it is an executable
// tclaude installed next to that binary if there is one — `go install` puts
// them in the same directory, as does any hand-assembled install prefix —
// otherwise the first tclaude on PATH, and finally the bare name as a last
// resort. Note that the release archives do NOT co-locate them: goreleaser
// tars each build id separately, so a downloaded release lands the two
// binaries in two archives and it is PATH that resolves them.
func SelfTclaudePath() string {
	self, err := os.Executable()
	if err != nil {
		self = ""
	}
	return resolveTclaudePath(self, selfIsTclaude)
}

// resolveTclaudePath is SelfTclaudePath's decision logic with the running
// executable path passed in, so the sibling and PATH fallbacks are testable
// without re-execing the test binary. self is "" when it could not be
// determined.
func resolveTclaudePath(self string, isTclaude bool) string {
	if self != "" {
		if isTclaude {
			return self
		}
		// LookPath rather than Stat: the name contains a separator, so it is
		// tried directly, and it rejects a directory or a non-executable file
		// named tclaude — either of which would otherwise beat a working
		// tclaude on PATH.
		if sibling, err := exec.LookPath(filepath.Join(filepath.Dir(self), "tclaude")); err == nil {
			return sibling
		}
	}
	if p, err := exec.LookPath("tclaude"); err == nil {
		return p
	}
	return "tclaude"
}

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
	return []string{SelfTclaudePath()}
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
