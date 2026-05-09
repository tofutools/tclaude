package agent

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
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
		RunFunc: func(p *replyParams, _ *cobra.Command, _ []string) {
			os.Exit(runReply(p, &messageDeps{nudge: defaultNudge}, os.Stdout, os.Stderr, os.Stdin))
		},
	}.ToCobra()
}

func runReply(p *replyParams, d *messageDeps, stdout, stderr io.Writer, stdin io.Reader) int {
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

	if DaemonAvailable() {
		return runReplyDaemon(id, p.Subject, body, stdout, stderr)
	}
	return runReplyDirect(id, p.Subject, body, d, stdout, stderr)
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
	fmt.Fprintf(stdout, "Sent reply #%d via group %q (%s)\n", resp.ID, resp.ViaGroup, state)
	return rcOK
}

func runReplyDirect(id int64, subject, body string, d *messageDeps, stdout, stderr io.Writer) int {
	myID, err := currentConvID()
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcNotFound
	}
	orig, err := db.GetAgentMessage(id)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	if orig == nil {
		fmt.Fprintf(stderr, "Error: no message #%d\n", id)
		return rcNotFound
	}
	if orig.ToConv != myID {
		fmt.Fprintf(stderr, "Error: you are not the recipient of message #%d\n", id)
		return rcAuth
	}
	if subject == "" && orig.Subject != "" {
		subject = "Re: " + orig.Subject
	}
	shared, err := db.SharedGroupsForConvs(myID, orig.FromConv)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	if len(shared) == 0 {
		fmt.Fprintf(stderr, "Error: no shared group with sender; reply path closed\n")
		return rcAuth
	}
	via := shared[0]
	newID, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:  via.ID,
		FromConv: myID,
		ToConv:   orig.FromConv,
		Subject:  subject,
		Body:     body,
	})
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	delivered := false
	if d != nil && d.nudge != nil {
		if candidates, err := db.FindSessionsByConvID(orig.FromConv); err == nil {
			for _, c := range candidates {
				if c.TmuxSession == "" || !session.IsTmuxSessionAlive(c.TmuxSession) {
					continue
				}
				nudge := fmt.Sprintf(
					"[system: new agent message #%d for you. fetch with: tclaude agent inbox read %d]",
					newID, newID,
				)
				if err := d.nudge(c.TmuxSession, nudge); err == nil {
					delivered = true
					_ = db.MarkAgentMessageDelivered(newID)
				}
				break
			}
		}
	}
	state := "queued; target not online"
	if delivered {
		state = "delivered"
	}
	fmt.Fprintf(stdout, "Sent reply #%d via group %q (%s)\n", newID, via.Name, state)
	return rcOK
}

// Compile-time check that we still need the clcommon import (used by
// the rest of the package via defaultNudge in message.go).
var _ = clcommon.TmuxCommand
