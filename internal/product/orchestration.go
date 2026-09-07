package product

import (
	"net/url"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
)

func registerOrchestration(root *cobra.Command, call apiCall) {
	for _, group := range []struct {
		name, description string
		writes            []struct{ verb, path, description string }
		list, inspect     string
	}{
		{"definition", "Author immutable process and team definitions", []struct{ verb, path, description string }{
			{"validate", "/v2/definitions/validate", "Validate a typed definition draft"},
			{"save", "/v2/definitions", "Save a new immutable revision with expected head revision"},
		}, "/v2/definitions", "/v2/definitions/"},
		{"program-profile", "Manage explicit program execution profiles", []struct{ verb, path, description string }{
			{"save", "/v2/program-profiles", "Save executable, policy and bounded output configuration"},
		}, "/v2/program-profiles", "/v2/program-profiles/"},
		{"process", "Start pinned process graphs and report exact node evidence", []struct{ verb, path, description string }{
			{"start", "/v2/processes", "Start a process using a pinned definition or explicit graph"},
			{"evidence", "/v2/processes/evidence", "Record evidence for an exact issued node attempt"},
			{"resolve-blocked", "/v2/processes/resolve-blocked", "Resolve an exact parked attempt by retry, rework, waiver, or cancellation"},
		}, "", "/v2/work/"},
		{"decision", "Read and answer authorized decision windows", []struct{ verb, path, description string }{
			{"submit", "/v2/decisions/submit", "Submit an answer at the expected decision window revision"},
		}, "/v2/decisions", "/v2/decisions/"},
		{"automation", "Author rules and inspect durable occurrences", []struct{ verb, path, description string }{
			{"save", "/v2/automation/rules", "Save a rule with explicit owner, delegation and policy"},
			{"run", "/v2/automation/run", "Admit a deduplicated manual occurrence"},
		}, "/v2/automation/rules", "/v2/automation/rules/"},
		{"team", "Deploy pinned team definitions", []struct{ verb, path, description string }{
			{"deploy", "/v2/teams/deploy", "Deploy explicitly pinned members and dependency waves"},
			{"rebrief", "/v2/teams/rebrief", "Send a selected pinned briefing revision to a deployment"},
			{"advance-phase", "/v2/teams/advance-phase", "Advance an advisory checklist without completing work"},
			{"stand-down", "/v2/teams/stand-down", "Stop a deployment while retaining owned checkouts and history"},
		}, "/v2/teams/deployments", "/v2/teams/deployments/"},
	} {
		cmd := boa.CmdT[struct{}]{Use: group.name, Short: group.description}.ToCobra()
		for _, write := range group.writes {
			cmd.AddCommand(managementFileCommand(write.verb, write.description, "POST", write.path, false, call))
		}
		if group.list != "" {
			list := boa.CmdT[struct{}]{Use: "list", Short: "List authorized records"}.ToCobra()
			list.Args = cobra.NoArgs
			list.RunE = func(cmd *cobra.Command, _ []string) error { return call(cmd, "GET", group.list, nil) }
			cmd.AddCommand(list)
		}
		inspect := boa.CmdT[struct{}]{Use: "inspect ID", Short: "Read an authorized record and its current revision"}.ToCobra()
		inspect.Args = cobra.ExactArgs(1)
		inspect.RunE = func(cmd *cobra.Command, args []string) error {
			return call(cmd, "GET", group.inspect+url.PathEscape(args[0]), nil)
		}
		cmd.AddCommand(inspect)
		if group.name == "automation" {
			occurrences := boa.CmdT[struct{}]{Use: "occurrences RULE", Short: "Read durable occurrence outcomes for a rule"}.ToCobra()
			occurrences.Args = cobra.ExactArgs(1)
			occurrences.RunE = func(cmd *cobra.Command, args []string) error {
				return call(cmd, "GET", "/v2/automation/occurrences?rule_id="+url.QueryEscape(args[0]), nil)
			}
			cmd.AddCommand(occurrences)
		}
		root.AddCommand(cmd)
	}
}
