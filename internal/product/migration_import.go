package product

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/internal/backend/migration"
	"github.com/tofutools/tclaude/internal/backend/server"
)

func registerOfflineImport(root *cobra.Command) {
	var bundle, manifest, state string
	var metadataOnly bool
	cmd := boa.CmdT[struct{}]{Use: "import", Short: "Convert an explicit offline snapshot into a new private state directory"}.ToCobra()
	cmd.Args = cobra.NoArgs
	cmd.Flags().StringVar(&bundle, "bundle", "", "Explicit immutable snapshot bundle directory")
	cmd.Flags().StringVar(&manifest, "manifest", "", "Manifest path inside the bundle")
	cmd.Flags().StringVar(&state, "state-dir", "", "Absolute new private target state directory")
	cmd.Flags().BoolVar(&metadataOnly, "metadata-only-attachments", false, "Explicitly retain unavailable attachment metadata without claiming preserved content")
	for _, flag := range []string{"bundle", "manifest", "state-dir"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		var result migration.ImportResult
		err := server.ImportOffline(cmd.Context(), state, func(ctx context.Context, database string) error {
			var err error
			result, err = migration.ImportSnapshot(ctx, migration.Bundle{Root: bundle, ManifestPath: manifest}, migration.ImportOptions{DestinationPath: database, MetadataOnlyAttachments: metadataOnly})
			return err
		})
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}
	root.AddCommand(cmd)
	var reportState string
	report := boa.CmdT[struct{}]{Use: "report", Short: "Read redacted import provenance from an explicit offline target"}.ToCobra()
	report.Args = cobra.NoArgs
	report.Flags().StringVar(&reportState, "state-dir", "", "Explicit private imported state directory")
	_ = report.MarkFlagRequired("state-dir")
	report.RunE = func(cmd *cobra.Command, _ []string) error {
		if !filepath.IsAbs(reportState) {
			return errors.New("absolute import state directory is required")
		}
		result, err := migration.ReadImportReport(cmd.Context(), filepath.Join(reportState, "backend.sqlite"))
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}
	root.AddCommand(report)
}
