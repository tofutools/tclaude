package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

type inboxAttachmentParams struct {
	ID     string `pos:"true" help:"Message ID from inbox ls"`
	Name   string `pos:"true" optional:"true" help:"The attachment's file name (default: all of them)"`
	Output string `long:"output" short:"o" optional:"true" help:"Where to save one attachment (default: a new directory under the temp dir)"`
}

func inboxAttachmentCmd() *cobra.Command {
	return boa.CmdT[inboxAttachmentParams]{
		Use:         "attachment",
		Short:       "Save a message's attachments and print where they are",
		Long:        "Fetches files attached to a message you received or sent. They are stored where agents can't reach them, so this is how you get to them. It does not mark the message read.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *inboxAttachmentParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.ID).SetAlternativesFunc(completeInboxMessageIDs)
			return nil
		},
		RunFunc: func(p *inboxAttachmentParams, _ *cobra.Command, _ []string) {
			os.Exit(runInboxAttachment(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runInboxAttachment(p *inboxAttachmentParams, stdout, stderr io.Writer) int {
	id, err := strconv.ParseInt(p.ID, 10, 64)
	if err != nil {
		fmt.Fprintf(stderr, "Error: invalid message ID %q\n", p.ID)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var m struct {
		Attachments []inboxAttachment `json:"attachments"`
	}
	path := fmt.Sprintf("/v1/messages/%d/attachments", id)
	if err := DaemonRequest(http.MethodGet, path, nil, &m, DaemonOpts{}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	wanted := m.Attachments[:0:0]
	for _, a := range m.Attachments {
		if p.Name == "" || a.Name == p.Name {
			wanted = append(wanted, a)
		}
	}
	if len(wanted) == 0 {
		fmt.Fprintf(stderr, "Error: message #%d has no attachment %q\n", id, p.Name)
		return rcInvalidArg
	}
	if p.Output != "" && len(wanted) > 1 {
		fmt.Fprintln(stderr, "Error: --output saves one attachment; name it")
		return rcInvalidArg
	}
	// A new directory with an unguessable name: nothing can be planted in it.
	dir := ""
	if p.Output == "" {
		if dir, err = os.MkdirTemp("", "tclaude-attachment-*"); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return rcIOFailure
		}
	}
	for _, a := range wanted {
		dest := p.Output
		if dest == "" {
			dest = filepath.Join(dir, filepath.Base(a.Name))
		}
		if err := saveMessageAttachment(id, a.ID, dest); err != nil {
			fmt.Fprintf(stderr, "Error: %s: %v\n", a.Name, err)
			return MapDaemonErrorToRC(err)
		}
		fmt.Fprintln(stdout, dest)
	}
	return rcOK
}

// daemonAttachmentStream is swapped by tests.
var daemonAttachmentStream = DaemonStreamGet

func saveMessageAttachment(messageID, attachmentID int64, dest string) error {
	body, err := daemonAttachmentStream(context.Background(), fmt.Sprintf("/v1/messages/%d/attachments/%d", messageID, attachmentID))
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".attachment-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	_, copyErr := io.Copy(tmp, body)
	if closeErr := tmp.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return copyErr
	}
	return os.Rename(tmp.Name(), dest)
}
