package product

import (
	"net/url"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
)

// Management commands use the same client and authenticated application routes
// as execution callers. Selecting a command never selects a stronger principal.
func registerManagement(root *cobra.Command, call apiCall) {
	registerConfigurationCatalog(root, call)
	snapshot := boa.CmdT[struct{}]{Use: "snapshot", Short: "Read the operator's complete work overview"}.ToCobra()
	snapshot.Args = cobra.NoArgs
	snapshot.RunE = func(cmd *cobra.Command, _ []string) error { return call(cmd, "GET", "/v2/snapshot", nil) }
	root.AddCommand(snapshot)
	agent := boa.CmdT[struct{}]{Use: "agent", Short: "Create and configure durable agents"}.ToCobra()
	agent.AddCommand(managementFileCommand("create", "Create an agent with explicit desired configuration", "POST", "/v2/agents", false, call))
	agent.AddCommand(managementFileCommand("update ID", "Update desired configuration at the expected revision", "PUT", "/v2/agents/", true, call))
	group := boa.CmdT[struct{}]{Use: "group", Short: "Organize agents and visible ownership"}.ToCobra()
	group.AddCommand(managementFileCommand("create", "Create a group with explicit members", "POST", "/v2/groups", false, call))
	owner := managementFileCommand("owner ID", "Assign the visible bounded owner role", "PUT", "/v2/groups/", true, call)
	owner.Annotations = map[string]string{"path-suffix": "/owner"}
	group.AddCommand(owner)
	root.AddCommand(agent, group)
	authority := boa.CmdT[struct{}]{Use: "authority", Short: "Inspect and administer explicit scoped authority"}.ToCobra()
	list := boa.CmdT[struct{}]{Use: "list", Short: "List visible grants, roles and assignments"}.ToCobra()
	list.Args = cobra.NoArgs
	list.RunE = func(cmd *cobra.Command, _ []string) error { return call(cmd, "GET", "/v2/authority", nil) }
	authority.AddCommand(list, managementFileCommand("explain", "Explain current authority for an exact resource and configuration", "POST", "/v2/authority/explain", false, call))
	for _, spec := range []struct{ name, method, path, suffix, description string }{
		{"grant ID", "PUT", "/v2/authority/grants/", "", "Save an explicitly bounded grant"},
		{"revoke-grant ID", "DELETE", "/v2/authority/grants/", "", "Revoke a grant at its expected revision"},
		{"role ID", "PUT", "/v2/authority/roles/", "", "Save an authored role"},
		{"assign ID", "PUT", "/v2/authority/roles/", "/assignments", "Assign a role to an explicit subject and resource"},
		{"unassign ID", "DELETE", "/v2/authority/roles/", "/assignments", "Remove an exact role assignment"},
		{"revoke-access ID", "POST", "/v2/executions/", "/access/revoke", "Revoke an execution's action access"},
	} {
		cmd := managementFileCommand(spec.name, spec.description, spec.method, spec.path, true, call)
		cmd.Annotations = map[string]string{"path-suffix": spec.suffix}
		authority.AddCommand(cmd)
	}
	access := boa.CmdT[struct{}]{Use: "access EXECUTION", Short: "Read action access state and revision"}.ToCobra()
	access.Args = cobra.ExactArgs(1)
	access.RunE = func(cmd *cobra.Command, args []string) error {
		return call(cmd, "GET", "/v2/executions/"+url.PathEscape(args[0])+"/access", nil)
	}
	authority.AddCommand(access)
	root.AddCommand(authority)
}

func managementFileCommand(use, description, method, path string, target bool, call apiCall) *cobra.Command {
	cmd := boa.CmdT[struct{}]{Use: use, Short: description}.ToCobra()
	cmd.Args = cobra.NoArgs
	if target {
		cmd.Args = cobra.ExactArgs(1)
	}
	var file string
	cmd.Flags().StringVar(&file, "file", "", "Request JSON with explicit configuration and expected revision where required")
	_ = cmd.MarkFlagRequired("file")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		body, err := readJourneyJSON(file)
		if err != nil {
			return err
		}
		endpoint := path
		if target {
			endpoint += url.PathEscape(args[0])
		}
		endpoint += cmd.Annotations["path-suffix"]
		return call(cmd, method, endpoint, body)
	}
	return cmd
}
