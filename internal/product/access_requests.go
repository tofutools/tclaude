package product

import (
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
)

func registerAccessRequests(root *cobra.Command, call apiCall) {
	access := boa.CmdT[struct{}]{Use: "access-request", Short: "Request and decide bounded temporary authority"}.ToCobra()
	access.AddCommand(managementFileCommand("ask", "Ask for one exact bounded and expiring grant", "POST", "/v2/access-requests", false, call))

	var pending bool
	list := boa.CmdT[struct{}]{Use: "list", Short: "List this agent's requests, or all requests as operator"}.ToCobra()
	list.Args = cobra.NoArgs
	list.Flags().BoolVar(&pending, "pending", false, "Only open unexpired requests")
	list.RunE = func(cmd *cobra.Command, _ []string) error {
		path := "/v2/access-requests"
		if pending {
			path += "?pending_only=true"
		}
		return call(cmd, "GET", path, nil)
	}
	access.AddCommand(list)

	inspect := boa.CmdT[struct{}]{Use: "inspect ID", Short: "Read an authorized request and decision projection"}.ToCobra()
	inspect.Args = cobra.ExactArgs(1)
	inspect.RunE = func(cmd *cobra.Command, args []string) error {
		return call(cmd, "GET", "/v2/access-requests/"+url.PathEscape(args[0]), nil)
	}
	access.AddCommand(inspect)

	for _, answer := range []string{"approve", "deny"} {
		answer := answer
		cmd := boa.CmdT[struct{}]{Use: answer + " ID", Short: answer + " an exact request at its expected revision"}.ToCobra()
		cmd.Args = cobra.ExactArgs(1)
		var file string
		cmd.Flags().StringVar(&file, "file", "", "Decision JSON containing request_id, expected_window_revision and reason")
		_ = cmd.MarkFlagRequired("file")
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			raw, err := readJourneyJSON(file)
			if err != nil {
				return err
			}
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				return err
			}
			if _, supplied := body["answer"]; supplied {
				return fmt.Errorf("decision file must not supply answer; the %s command fixes it", answer)
			}
			body["answer"] = answer
			return call(cmd, "POST", "/v2/access-requests/"+url.PathEscape(args[0])+"/decision", body)
		}
		access.AddCommand(cmd)
	}
	root.AddCommand(access)
}
