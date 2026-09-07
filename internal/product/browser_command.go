package product

import (
	"fmt"
	"net/url"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/internal/product/browser"
)

func browserCommand() *cobra.Command {
	cmd := boa.CmdT[struct{}]{Use: "dashboard", Short: "Open a local operator dashboard for an initialized backend"}.ToCobra()
	cmd.Args = cobra.NoArgs
	var state, listen string
	var wizard, slop bool
	cmd.Flags().StringVar(&state, "state-dir", "", "Explicit initialized operator state directory")
	cmd.Flags().StringVar(&listen, "listen", "127.0.0.1:0", "Loopback IP and port; zero selects an available port")
	cmd.Flags().BoolVar(&wizard, "wizard", false, "Open with wizard vocabulary and styling")
	cmd.Flags().BoolVar(&slop, "slop", false, "Open with slop-machine styling")
	cmd.MarkFlagsMutuallyExclusive("wizard", "slop")
	_ = cmd.MarkFlagRequired("state-dir")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		view, err := browser.Open(state, listen)
		if err != nil {
			return err
		}
		defer func() { _ = view.Close() }()
		link, err := url.Parse(view.URL())
		if err != nil {
			return err
		}
		params := link.Query()
		if wizard {
			params.Set("wizard", "1")
		}
		if slop {
			params.Set("slop", "1")
		}
		link.RawQuery = params.Encode()
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Open this private, single-use login link:\n"+link.String()); err != nil {
			return err
		}
		return view.Serve(cmd.Context())
	}
	return cmd
}
