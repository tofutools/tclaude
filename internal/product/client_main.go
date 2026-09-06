package product

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/internal/backend/client"
)

// ClientCommand constructs the shared replacement product command.
func ClientCommand() *cobra.Command {
	root := boa.CmdT[struct{}]{Use: "tclaude", Short: "Organize and operate agentic work"}.ToCobra()
	root.SilenceUsage = true
	root.SilenceErrors = true
	var socketPath, credentialFile, operatorState string
	root.PersistentFlags().StringVar(&socketPath, "socket", os.Getenv("TCLAUDE_BACKEND_SOCKET"), "Unix socket path (or TCLAUDE_BACKEND_SOCKET)")
	root.PersistentFlags().StringVar(&credentialFile, "credential-file", os.Getenv("TCLAUDE_BACKEND_CREDENTIAL_FILE"), "Protected credential resource (or TCLAUDE_BACKEND_CREDENTIAL_FILE)")
	root.PersistentFlags().StringVar(&operatorState, "operator-state", "", "Explicit initialized operator state directory; cannot be combined with execution credentials")
	call := func(cmd *cobra.Command, method, path string, body any) error {
		selectedSocket, selectedCredential := socketPath, credentialFile
		if operatorState != "" {
			if selectedSocket != "" || selectedCredential != "" || !filepath.IsAbs(operatorState) {
				return fmt.Errorf("operator-state must be absolute and cannot be combined with socket or execution credentials")
			}
			selectedSocket = filepath.Join(operatorState, "api.sock")
			selectedCredential = filepath.Join(operatorState, "operator.token")
		}
		api, err := client.New(selectedSocket, selectedCredential)
		if err != nil {
			return err
		}
		defer api.Close()
		var result json.RawMessage
		if err := api.Call(cmd.Context(), method, path, body, &result); err != nil {
			return err
		}
		if len(result) == 0 {
			return nil
		}
		var formatted any
		if err := json.Unmarshal(result, &formatted); err != nil {
			return err
		}
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(formatted)
	}
	whoami := boa.CmdT[struct{}]{Use: "whoami", Short: "Read this execution's identity and context"}.ToCobra()
	whoami.Args = cobra.NoArgs
	whoami.RunE = func(cmd *cobra.Command, _ []string) error { return call(cmd, "GET", "/v2/identity", nil) }
	root.AddCommand(whoami)

	var unread bool
	inbox := boa.CmdT[struct{}]{Use: "inbox", Short: "Read this agent's durable inbox"}.ToCobra()
	inbox.Args = cobra.NoArgs
	inbox.Flags().BoolVar(&unread, "unread", false, "Only unread messages")
	inbox.RunE = func(cmd *cobra.Command, _ []string) error {
		if unread {
			return call(cmd, "GET", "/v2/inbox?unread_only=true", nil)
		}
		return call(cmd, "GET", "/v2/inbox", nil)
	}
	root.AddCommand(inbox)

	var statusAgent, statusExecution, statusGroup string
	status := boa.CmdT[struct{}]{Use: "status", Short: "Read authorized status (defaults to self)"}.ToCobra()
	status.Args = cobra.NoArgs
	status.Flags().StringVar(&statusAgent, "agent", "", "Agent ID")
	status.Flags().StringVar(&statusExecution, "execution", "", "Execution ID")
	status.Flags().StringVar(&statusGroup, "group", "", "Group members")
	status.MarkFlagsMutuallyExclusive("agent", "execution", "group")
	status.RunE = func(cmd *cobra.Command, _ []string) error {
		target := map[string]string{"Kind": "self"}
		if statusAgent != "" {
			target = map[string]string{"Kind": "agent", "AgentID": statusAgent}
		}
		if statusExecution != "" {
			target = map[string]string{"Kind": "execution", "ExecutionID": statusExecution}
		}
		if statusGroup != "" {
			target = map[string]string{"Kind": "group_members", "GroupID": statusGroup}
		}
		return call(cmd, "POST", "/v2/status", map[string]any{"target": target})
	}
	root.AddCommand(status)

	var recipients []string
	var sendID string
	send := boa.CmdT[struct{}]{Use: "send TEXT", Short: "Accept a durable message to authorized agents"}.ToCobra()
	send.Args = cobra.ExactArgs(1)
	send.Flags().StringSliceVar(&recipients, "to", nil, "Recipient agent IDs")
	send.Flags().StringVar(&sendID, "request-id", "", "Stable identity for this exact message request")
	_ = send.MarkFlagRequired("to")
	_ = send.MarkFlagRequired("request-id")
	send.RunE = func(cmd *cobra.Command, args []string) error {
		return call(cmd, "POST", "/v2/messages", map[string]any{"request_id": sendID, "recipients": recipients, "body": args[0]})
	}
	root.AddCommand(send)

	var readID string
	read := boa.CmdT[struct{}]{Use: "read MESSAGE_ID", Short: "Acknowledge a message in this agent's own inbox"}.ToCobra()
	read.Args = cobra.ExactArgs(1)
	read.Flags().StringVar(&readID, "request-id", "", "Stable identity for this acknowledgement")
	_ = read.MarkFlagRequired("request-id")
	read.RunE = func(cmd *cobra.Command, args []string) error {
		return call(cmd, "POST", "/v2/inbox/"+args[0]+"/read", map[string]string{"request_id": readID})
	}
	root.AddCommand(read)
	registerManagement(root, call)
	registerControls(root, call)
	registerJourney(root, call)
	return root
}
