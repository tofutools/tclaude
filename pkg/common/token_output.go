package common

import (
	"os"

	"golang.org/x/term"
)

// ShouldPrintToken applies the shared operator-secret output policy. Unknown
// modes fail closed; CLI callers validate the flag before creating secrets.
func ShouldPrintToken(mode string, stdoutTTY, stderrTTY bool) bool {
	return mode == "always" || mode == "auto" && stdoutTTY && stderrTTY
}

// TokenOutputIsInteractive requires both output streams to be real terminals,
// so redirecting either one cannot accidentally leak an automatic banner.
func TokenOutputIsInteractive() bool {
	return term.IsTerminal(int(os.Stdout.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}
