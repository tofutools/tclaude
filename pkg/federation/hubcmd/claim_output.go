package hubcmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/tofutools/tclaude/pkg/common"
)

type ClaimOutputParams struct {
	PrintClaimToken string `long:"print-claim-token" default:"auto" help:"Claim token output: auto (both stdout and stderr are terminals), always (may expose the token in logs), or never (private-file guidance only)"`
}

func validateClaimOutput(mode string) error {
	switch mode {
	case "auto", "always", "never":
		return nil
	}
	return fmt.Errorf("--print-claim-token must be auto, always, or never")
}

func printClaimOutput(w io.Writer, token, path, mode string) {
	interactive := common.TokenOutputIsInteractive()
	writeClaimOutput(w, token, path, mode, interactive, interactive)
}

func writeClaimOutput(w io.Writer, token, path, mode string, stdoutTTY, stderrTTY bool) {
	if common.ShouldPrintToken(mode, stdoutTTY, stderrTTY) {
		fmt.Fprintf(w, "Hub admin claim token (single-use, valid 24h): %s\n", token)
	}
	// Quote as a shell word: paths can contain spaces, quotes, or substitutions.
	quoted := "'" + strings.ReplaceAll(path, "'", "'\\''") + "'"
	fmt.Fprintf(w, "Private claim file (0600, single-use, valid 24h): %s\nRead it on the hub host: sudo cat -- %s\n", path, quoted)
}
