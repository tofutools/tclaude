package main

import (
	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
)

type apiCall func(*cobra.Command, string, string, any) error

// Controls address explicit durable targets. The application resolves current
// authority and configuration; this client never chooses native harness syntax.
func registerControls(root *cobra.Command, call apiCall) {
	var interactID string
	interact := boa.CmdT[struct{}]{Use: "interact EXECUTION_ID TEXT", Short: "Send input to an authorized execution"}.ToCobra()
	interact.Args = cobra.ExactArgs(2)
	interact.Flags().StringVar(&interactID, "request-id", "", "Stable identity for this input request")
	_ = interact.MarkFlagRequired("request-id")
	interact.RunE = func(cmd *cobra.Command, args []string) error {
		return call(cmd, "POST", "/v2/interact", map[string]any{"request_id": interactID, "execution_id": args[0], "text": args[1]})
	}
	root.AddCommand(interact)

	var stopID string
	var force bool
	stop := boa.CmdT[struct{}]{Use: "stop EXECUTION_ID", Short: "Stop an authorized execution"}.ToCobra()
	stop.Args = cobra.ExactArgs(1)
	stop.Flags().StringVar(&stopID, "request-id", "", "Stable identity for this stop request")
	stop.Flags().BoolVar(&force, "force", false, "Request forced termination")
	_ = stop.MarkFlagRequired("request-id")
	stop.RunE = func(cmd *cobra.Command, args []string) error {
		return call(cmd, "POST", "/v2/stop", map[string]any{"request_id": stopID, "execution_id": args[0], "force": force})
	}
	root.AddCommand(stop)

	for _, resume := range []bool{false, true} {
		name, usage, description := "launch", "launch AGENT_ID", "Start an authorized agent with its current desired configuration"
		if resume {
			name, usage, description = "resume", "resume AGENT_ID CONVERSATION_ID", "Continue a selected logical conversation"
		}
		var requestID string
		var revision, associationRevision uint64
		cmd := boa.CmdT[struct{}]{Use: usage, Short: description}.ToCobra()
		cmd.Args = cobra.ExactArgs(1)
		cmd.Flags().StringVar(&requestID, "request-id", "", "Stable identity for this exact request")
		cmd.Flags().Uint64Var(&revision, "expected-revision", 0, "Agent revision returned by status")
		_ = cmd.MarkFlagRequired("request-id")
		_ = cmd.MarkFlagRequired("expected-revision")
		if resume {
			cmd.Args = cobra.ExactArgs(2)
			cmd.Flags().Uint64Var(&associationRevision, "expected-association-revision", 0, "Association revision returned by identity or snapshot")
			_ = cmd.MarkFlagRequired("expected-association-revision")
		}
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			body := map[string]any{"request_id": requestID, "target": map[string]any{"agent": map[string]any{"agent_id": args[0], "expected_revision": revision}}}
			if resume {
				body["conversation_id"] = args[1]
				body["expected_association_revision"] = associationRevision
			}
			return call(cmd, "POST", "/v2/"+name, body)
		}
		root.AddCommand(cmd)
	}
}
