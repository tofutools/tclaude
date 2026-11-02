package mcpserve

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
)

// Cmd is `tclaude mcp`.
func Cmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "mcp",
		Short:       "Connect an assistant running outside tclaude",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     []*cobra.Command{serveCmd()},
	}.ToCobra()
}

type serveParams struct {
	CredentialFile string `long:"credential-file" optional:"true" help:"Read the credential from this file, which only you may be able to read (chmod 600)"`
}

func serveCmd() *cobra.Command {
	return boa.CmdT[serveParams]{
		Use:   "serve",
		Short: "Serve an external steward's tools to its assistant over stdio",
		Long: "Runs an MCP server on stdin and stdout, for your assistant's MCP settings to start. " +
			"It acts as the external steward the credential names, with that steward's permissions. " +
			"The credential comes from " + CredentialEnv + " or --credential-file, never from the command line. " +
			"See docs/external-steward.md.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *serveParams, _ *cobra.Command, _ []string) {
			os.Exit(runServe(p))
		},
	}.ToCobra()
}

func runServe(p *serveParams) int {
	credential, err := LoadCredential(p.CredentialFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := NewServer(NewClient(credential), buildversion.AppVersion())
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		return 1
	}
	return 0
}
