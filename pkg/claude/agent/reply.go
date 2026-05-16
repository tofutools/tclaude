package agent

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type replyParams struct {
	ID      string `pos:"true" help:"Message ID to reply to (from inbox ls)"`
	Body    string `pos:"true" optional:"true" help:"Reply body (or use --stdin / --file)"`
	Subject string `long:"subject" short:"s" optional:"true" help:"Override the auto-generated 'Re: …' subject"`
	Stdin   bool   `long:"stdin" help:"Read body from stdin"`
	File    string `long:"file" short:"f" optional:"true" help:"Read body from a file"`
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

	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	return runReplyDaemon(id, p.Subject, body, stdout, stderr)
}

func runReplyDaemon(id int64, subject, body string, stdout, stderr io.Writer) int {
	var resp struct {
		ID        int64  `json:"id"`
		Delivered bool   `json:"delivered"`
		ViaGroup  string `json:"via_group"`
	}
	err := DaemonPost(fmt.Sprintf("/v1/messages/%d/reply", id), map[string]string{
		"subject": subject,
		"body":    body,
	}, &resp)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	state := "queued; target not online"
	if resp.Delivered {
		state = "delivered"
	}
	// A reply to a direct (off-group) message has no routing group.
	via := "directly"
	if resp.ViaGroup != "" {
		via = fmt.Sprintf("via group %q", resp.ViaGroup)
	}
	fmt.Fprintf(stdout, "Sent reply #%d %s (%s)\n", resp.ID, via, state)
	return rcOK
}

