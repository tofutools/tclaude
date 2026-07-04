package agent

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent clipboard` — copy text to the HUMAN's system clipboard.
// agentd (running on the host) does the write; the agent's sandbox can't
// reach the display. Permission-gated on human.clipboard: NOT
// default-granted and NOT owner-implied, so it needs an explicit grant or
// a per-call --ask-human popup approval.

// maxClipboardBytes mirrors the daemon's cap (agentd.maxClipboardBytes).
// Duplicated here — the agent package can't import agentd (import cycle) —
// so the CLI can reject an oversized payload with a clear local message
// before it ever hits the socket. Keep in sync with the daemon.
const maxClipboardBytes = 256 * 1024

type clipboardParams struct {
	Text     string `pos:"true" optional:"true" help:"Text to copy (or use --file). Prefer --file for long, multi-line, or backtick-containing content."`
	File     string `long:"file" short:"f" optional:"true" help:"Read the text from this file ('-' reads stdin). Sidesteps shell quoting and keeps content out of /proc's cmdline — best for anything non-trivial."`
	AskHuman string `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny. The popup shows a preview of what would be copied."`
}

func clipboardCmd() *cobra.Command {
	return boa.CmdT[clipboardParams]{
		Use:   "clipboard",
		Short: "Copy text to the human's system clipboard",
		Long: "Copies text to the HUMAN's system clipboard — for when the human asks you to put a draft, command, or snippet on their clipboard.\n\n" +
			"The daemon performs the copy on the host (via wl-copy/xclip/xsel on Linux, pbcopy on macOS, clip.exe under WSL); your sandbox can't reach the display directly.\n\n" +
			"Gated on the `human.clipboard` permission — NOT granted by default and NOT implied by group ownership. Without the grant, pass `--ask-human <timeout>` to raise a one-off approval popup (which shows the human a preview of the text before they approve).\n\n" +
			"Give the text inline or with --file (--file - reads stdin). Content is copied verbatim — leading/trailing whitespace and newlines are preserved.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *clipboardParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *clipboardParams, _ *cobra.Command, _ []string) {
			os.Exit(runClipboard(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runClipboard(p *clipboardParams, stdin io.Reader, stdout, stderr io.Writer) int {
	text, rc := resolveBodyInput(p.Text, p.File, "the text argument", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	// Reject a body with no actual content — copying nothing is a mistake.
	// We check on the trimmed form but SEND the untrimmed text, so a
	// deliberately whitespace-bearing snippet (trailing newline, indented
	// block) is preserved verbatim on the clipboard.
	if strings.TrimSpace(text) == "" {
		fmt.Fprintln(stderr, "Error: nothing to copy — pass the text inline or via --file")
		return rcInvalidArg
	}
	if len(text) > maxClipboardBytes {
		fmt.Fprintf(stderr, "Error: text too long: %d bytes, max %d\n", len(text), maxClipboardBytes)
		return rcInvalidArg
	}
	ask, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}

	var resp struct {
		Bytes int `json:"bytes"`
	}
	if err := DaemonRequest(http.MethodPost, "/v1/clipboard",
		map[string]any{"text": text}, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Copied %d bytes to the human's clipboard.\n", resp.Bytes)
	return rcOK
}
