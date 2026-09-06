package product

import (
	"fmt"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/internal/product/browser"
)

func browserCommand() *cobra.Command {
	cmd := boa.CmdT[struct{}]{Use: "dashboard", Short: "Open a local operator dashboard for an initialized backend"}.ToCobra()
	cmd.Args = cobra.NoArgs
	var state, listen string
	cmd.Flags().StringVar(&state, "state-dir", "", "Explicit initialized operator state directory")
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:0", "Loopback IP and port; zero selects an available port")
	_ = cmd.MarkFlagRequired("state-dir")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		view, err := browser.Open(state, listen)
		if err != nil {
			return err
		}
		defer func() { _ = view.Close() }()
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Open this private, single-use login link:\n"+view.URL()); err != nil {
			return err
		}
		return view.Serve(cmd.Context())
	}
	return cmd
}
