// Command tclaude-backend-agent calls the replacement API using an explicitly
// delivered execution credential resource. It never opens backend storage.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/internal/backend/client"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := command().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func command() *cobra.Command {
	root := boa.CmdT[struct{}]{Use: "tclaude-backend-agent", Short: "Use the replacement backend as this execution"}.ToCobra()
	root.SilenceUsage = true
	root.SilenceErrors = true
	var socketPath, credentialFile string
	root.PersistentFlags().StringVar(&socketPath, "socket", os.Getenv("TCLAUDE_BACKEND_SOCKET"), "Unix socket path (or TCLAUDE_BACKEND_SOCKET)")
	root.PersistentFlags().StringVar(&credentialFile, "credential-file", os.Getenv("TCLAUDE_BACKEND_CREDENTIAL_FILE"), "Protected credential resource (or TCLAUDE_BACKEND_CREDENTIAL_FILE)")
	call := func(cmd *cobra.Command, method, path string, body any) error {
		api, err := client.New(socketPath, credentialFile)
		if err != nil {
			return err
		}
		defer api.Close()
		var result json.RawMessage
		if err := api.Call(cmd.Context(), method, path, body, &result); err != nil {
			return err
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
	registerControls(root, call)
	registerJourney(root, call)
	return root
}
