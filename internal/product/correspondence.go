package product

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func registerCorrespondence(root *cobra.Command, call apiCall) {
	var to, cc, files []string
	var toOperator, ccOperator bool
	var requestID, subject, parent string
	send := boa.CmdT[struct{}]{Use: "send [TEXT]", Short: "Send durable threaded correspondence and bounded attachments"}.ToCobra()
	send.Args = cobra.MaximumNArgs(1)
	send.Flags().StringSliceVar(&to, "to", nil, "To agent IDs")
	send.Flags().StringSliceVar(&cc, "cc", nil, "CC agent IDs")
	send.Flags().BoolVar(&toOperator, "to-operator", false, "Address the local operator")
	send.Flags().BoolVar(&ccOperator, "cc-operator", false, "CC the local operator")
	send.Flags().StringVar(&subject, "subject", "Message", "Message subject")
	send.Flags().StringVar(&parent, "reply-to", "", "Parent message ID")
	send.Flags().StringArrayVar(&files, "attach", nil, "Local file to attach (up to four, 5 MiB each, 10 MiB total)")
	send.Flags().StringVar(&requestID, "request-id", "", "Stable identity for this exact message request")
	_ = send.MarkFlagRequired("request-id")
	send.RunE = func(cmd *cobra.Command, args []string) error {
		if len(to)+len(cc) == 0 && !toOperator && !ccOperator {
			return fmt.Errorf("a To or CC recipient is required")
		}
		if len(files) > 4 {
			return fmt.Errorf("at most four attachments are allowed")
		}
		var attachments []app.AttachmentInput
		total := 0
		for _, path := range files {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			content, readErr := io.ReadAll(io.LimitReader(file, (5<<20)+1))
			closeErr := file.Close()
			if readErr != nil {
				return readErr
			}
			if closeErr != nil {
				return closeErr
			}
			total += len(content)
			if len(content) > 5<<20 || total > 10<<20 {
				return fmt.Errorf("attachment size limit exceeded")
			}
			attachments = append(attachments, app.AttachmentInput{Filename: filepath.Base(path), MediaType: http.DetectContentType(content), Content: content})
		}
		body := ""
		if len(args) != 0 {
			body = args[0]
		}
		return call(cmd, "POST", "/v2/messages", map[string]any{"request_id": requestID, "subject": subject, "parent_message_id": parent, "to": model.MessageAudience{AgentIDs: agentIDs(to), Operator: toOperator}, "cc": model.MessageAudience{AgentIDs: agentIDs(cc), Operator: ccOperator}, "body": body, "attachments": attachments})
	}
	attachment := boa.CmdT[struct{}]{Use: "attachment ID", Short: "Read an authorized attachment as metadata and base64 content"}.ToCobra()
	attachment.Args = cobra.ExactArgs(1)
	attachment.RunE = func(cmd *cobra.Command, args []string) error {
		return call(cmd, "GET", "/v2/attachments/"+url.PathEscape(args[0]), nil)
	}
	root.AddCommand(send, attachment)
}
func agentIDs(values []string) []model.AgentID {
	out := make([]model.AgentID, len(values))
	for i, v := range values {
		out[i] = model.AgentID(v)
	}
	return out
}
