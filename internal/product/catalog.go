package product

import (
	"net/url"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
)

func registerConfigurationCatalog(root *cobra.Command, call apiCall) {
	catalog := boa.CmdT[struct{}]{Use: "configuration-profile", Short: "Save and select immutable launch configurations"}.ToCobra()
	catalog.AddCommand(managementFileCommand("save", "Save a revision without changing existing agents", "POST", "/v2/configuration-profiles", false, call))
	list := boa.CmdT[struct{}]{Use: "list", Short: "List saved launch configurations"}.ToCobra()
	list.Args = cobra.NoArgs
	list.RunE = func(cmd *cobra.Command, _ []string) error { return call(cmd, "GET", "/v2/configuration-profiles", nil) }
	get := boa.CmdT[struct{}]{Use: "get ID", Short: "Read a current or selected immutable configuration"}.ToCobra()
	get.Args = cobra.ExactArgs(1)
	var revision string
	get.Flags().StringVar(&revision, "revision", "", "Exact saved revision (defaults to current)")
	get.RunE = func(cmd *cobra.Command, args []string) error {
		return call(cmd, "GET", "/v2/configuration-profiles/"+url.PathEscape(args[0])+"?revision_id="+url.QueryEscape(revision), nil)
	}
	catalog.AddCommand(list, get)
	root.AddCommand(catalog)
}
