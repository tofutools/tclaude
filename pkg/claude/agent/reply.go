package agent

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type replyParams struct {
	ID      string   `pos:"true" help:"Message ID to reply to (from inbox ls)"`
	Text    string   `pos:"true" optional:"true" help:"Reply body (or use --body / --stdin / --file)"`
	Body    string   `long:"body" optional:"true" help:"Reply body as a flag instead of positional text"`
	Subject string   `long:"subject" short:"s" optional:"true" help:"Override the auto-generated 'Re: …' subject"`
	Stdin   bool     `long:"stdin" help:"Read body from stdin"`
	File    string   `long:"file" short:"f" optional:"true" help:"Read body from a file ('-' reads stdin). Sidesteps shell quoting — best for long, multi-line, or backtick-containing bodies (the shell eats backticks from an inline body)."`
	Attach  []string `long:"attach" short:"a" optional:"true" help:"Attach a file you can read (repeatable, 4 files and 512 KiB at most; images, text, data and PDF). Not on a reply to a remote message."`
}

func replyCmd() *cobra.Command {
	return boa.CmdT[replyParams]{
		Use:         "reply",
		Short:       "Reply to a message in your inbox by ID",
		Long:        "Looks up the message, sends the body to its sender (Reply-To), and inherits a 'Re: <subject>' unless --subject is given.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *replyParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.ID).SetAlternativesFunc(completeInboxMessageIDs)
			return nil
		},
		RunFunc: func(p *replyParams, _ *cobra.Command, _ []string) {
			os.Exit(runReply(p, os.Stdout, os.Stderr, os.Stdin))
		},
	}.ToCobra()
}

func runReply(p *replyParams, stdout, stderr io.Writer, stdin io.Reader) int {
	id, err := strconv.ParseInt(p.ID, 10, 64)
	if err != nil {
		fmt.Fprintf(stderr, "Error: invalid message ID %q\n", p.ID)
		return rcInvalidArg
	}

	// Reuse readBody by adapting params (it is shared with `message`).
	body, status := readBody(&messageParams{
		Text:  p.Text,
		Body:  p.Body,
		Stdin: p.Stdin,
		File:  p.File,
	}, stdin, stderr)
	if status != rcOK {
		return status
	}
	if strings.TrimSpace(body) == "" {
		fmt.Fprintf(stderr, "Error: reply body is empty\n")
		return rcInvalidArg
	}

	atts, err := ReadAttachments(p.Attach)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}

	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	return runReplyDaemon(id, p.Subject, body, atts, stdout, stderr)
}

func runReplyDaemon(id int64, subject, body string, atts []proto.AttachmentPayload, stdout, stderr io.Writer) int {
	var resp struct {
		ID       int64  `json:"id"`
		Queued   bool   `json:"queued"`
		Pending  int    `json:"pending"`
		ViaGroup string `json:"via_group"`
	}
	payload := map[string]any{
		"subject": subject,
		"body":    body,
	}
	if len(atts) > 0 {
		payload["attachments"] = atts
	}
	err := DaemonRequest(http.MethodPost, fmt.Sprintf("/v1/messages/%d/reply", id), payload, &resp, DaemonOpts{})
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	// The inbox row is durable before the asynchronous nudge runs. Pending is
	// the target's unprocessed regular-message backlog, not a delivery verdict.
	state := queuedState(resp.Pending)
	// A reply to a direct (off-group) message has no routing group.
	via := "directly"
	if resp.ViaGroup != "" {
		via = fmt.Sprintf("via group %q", resp.ViaGroup)
	}
	fmt.Fprintf(stdout, "Sent reply #%d %s (%s)\n", resp.ID, via, state)
	return rcOK
}
