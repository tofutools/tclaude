package product

import (
	"encoding/json"
	"fmt"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/internal/backend/migration"
)

// Offline inspection is separate from authenticated live application commands.
// Both paths are explicit; this command never discovers an existing database.
func migrationCommand(inspector migration.Inspector) *cobra.Command {
	root := boa.CmdT[struct{}]{Use: "migration", Short: "Inspect an explicit offline snapshot before migration"}.ToCobra()
	for _, name := range []string{"inspect", "plan"} {
		cmd := boa.CmdT[struct{}]{Use: name, Short: "Report offline snapshot " + name + " results without target writes"}.ToCobra()
		cmd.Args = cobra.NoArgs
		var bundle, manifest string
		cmd.Flags().StringVar(&bundle, "bundle", "", "Explicit snapshot bundle directory")
		cmd.Flags().StringVar(&manifest, "manifest", "", "Explicit manifest path inside the bundle")
		_ = cmd.MarkFlagRequired("bundle")
		_ = cmd.MarkFlagRequired("manifest")
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			inspection, err := inspector.Inspect(cmd.Context(), migration.Bundle{Root: bundle, ManifestPath: manifest})
			if err != nil {
				return err
			}
			var report any = inspection
			valid := inspection.Valid
			if name == "plan" {
				plan, err := inspector.Plan(inspection)
				if err != nil {
					return err
				}
				report = plan
				valid = plan.PreflightValid
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(report); err != nil {
				return err
			}
			if !valid {
				return fmt.Errorf("snapshot preflight has blocking diagnostics")
			}
			return nil
		}
		root.AddCommand(cmd)
	}
	return root
}
