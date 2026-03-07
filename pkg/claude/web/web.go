package web

import (
	"fmt"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common"

	"github.com/spf13/cobra"
)

type Params struct {
	Port    int    `long:"port" short:"p" help:"Port to listen on" default:"8443"`
	User    string `long:"user" short:"u" help:"Username for basic auth (required)"`
	Pass    string `long:"pass" help:"Password for basic auth (required)"`
	Session string `pos:"true" optional:"true" help:"Session ID to attach to (auto-detects if only one running)"`
	Bind    string `long:"bind" help:"Address to bind to (use 0.0.0.0 for all interfaces)" default:"127.0.0.1"`
	NoTLS   bool   `long:"no-tls" help:"Disable TLS (not recommended)"`
	NewCert bool   `long:"new-cert" help:"Force regenerate TLS certificate"`
}

func Cmd() *cobra.Command {
	return boa.CmdT[Params]{
		Use:   "web [session-id]",
		Short: "Serve a Claude session via web terminal",
		Long: `Start a web server that mirrors a Claude Code tmux session in the browser.

Connects to an existing tmux-based session and serves it via xterm.js + WebSocket.
Both the desktop terminal and the web browser see the same session simultaneously.

Requires basic auth credentials for access.`,
		ParamEnrich: common.DefaultParamEnricher(),
		ValidArgsFunc: func(p *Params, cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return session.GetSessionCompletions(false), cobra.ShellCompDirectiveKeepOrder | cobra.ShellCompDirectiveNoFileComp
		},
		RunFunc: func(params *Params, cmd *cobra.Command, args []string) {
			if err := run(params); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func run(params *Params) error {
	if params.User == "" || params.Pass == "" {
		return fmt.Errorf("--user and --pass are required for basic auth")
	}

	// Resolve which session to attach to
	sessionInput := clcommon.ExtractIDFromCompletion(params.Session)
	tmuxSession, sessionID, err := resolveSession(sessionInput)
	if err != nil {
		return err
	}

	// Force regenerate cert if requested
	if params.NewCert && !params.NoTLS {
		if err := deleteCerts(); err != nil {
			return fmt.Errorf("failed to delete old certs: %w", err)
		}
		fmt.Println("Deleted old TLS certificate")
	}

	scheme := "https"
	if params.NoTLS {
		scheme = "http"
	}

	fmt.Printf("Serving session %s on %s://%s:%d\n", sessionID, scheme, params.Bind, params.Port)
	fmt.Printf("  tmux session: %s\n", tmuxSession)
	fmt.Printf("  auth: %s / ****\n", params.User)

	if !params.NoTLS {
		fmt.Println("  tls: self-signed certificate (accept browser warning)")
	}

	printNetworkInfo(scheme, params.Port)

	fmt.Println("\nPress Ctrl+C to stop the server")

	return serve(params.Bind, params.Port, params.User, params.Pass, tmuxSession, !params.NoTLS)
}
