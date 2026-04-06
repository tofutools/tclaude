package web

import (
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common"

	"github.com/spf13/cobra"
)

type Params struct {
	Port    int    `long:"port" short:"p" help:"Port to listen on" default:"8443"`
	User    string `long:"user" short:"u" optional:"true" help:"Username for basic auth (default: hostname)"`
	Pass    string `long:"pass" optional:"true" help:"Password for basic auth (default: auto-generated)"`
	Session string `pos:"true" optional:"true" help:"Session ID to attach to (auto-detects if only one running)"`
	Bind    string `long:"bind" help:"Address to bind to (use 0.0.0.0 for all interfaces)"`
	NoTLS   bool   `long:"no-tls" help:"Disable TLS (not recommended)"`
	NewCert bool   `long:"new-cert" help:"Force regenerate TLS certificate"`
	QR      bool   `long:"qr" help:"Show QR code for easy mobile connection"`
}

func Cmd() *cobra.Command {
	return boa.CmdT[Params]{
		Use:   "web",
		Short: "Serve a Claude session via web terminal",
		Long: `Start a web server that mirrors a Claude Code tmux session in the browser.

Connects to an existing tmux-based session and serves it via xterm.js + WebSocket.
Both the desktop terminal and the web browser see the same session simultaneously.

Credentials are auto-generated if not provided (username defaults to hostname).`,
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, params *Params, cmd *cobra.Command) error {
			bindParam := boa.GetParamT(ctx, &params.Bind)
			hostname, err := os.Hostname()
			if err != nil {
				hostname = "127.0.0.1"
			}
			bindParam.SetDefaultT(hostname)
			bindParam.SetAlternativesFunc(func(cmd *cobra.Command, args []string, toComplete string) []string {
				return getBindAlternatives()
			})
			return nil
		},
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
	// Reconfigure slog to also write to stderr for live server output
	common.SetupLoggingWithStderr(slog.LevelInfo)

	// Auto-generate credentials if not provided
	if params.User == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("failed to get hostname for default user: %w", err)
		}
		params.User = hostname
	}
	if params.Pass == "" {
		pass, err := generatePassword()
		if err != nil {
			return fmt.Errorf("failed to generate password: %w", err)
		}
		params.Pass = pass
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

	// Resolve bind address - hostnames need to be resolved to an IP for binding,
	// but we keep the original for display/QR purposes.
	bindAddr := params.Bind
	if net.ParseIP(bindAddr) == nil {
		addrs, err := net.LookupHost(bindAddr)
		if err != nil {
			return fmt.Errorf("cannot resolve bind address %q: %w", bindAddr, err)
		}
		bindAddr = addrs[0]
	}

	// Bind the TCP port immediately, before printing any output
	ps, err := prepare(bindAddr, params.Port, params.User, params.Pass, tmuxSession, !params.NoTLS)
	if err != nil {
		return err
	}

	if bindAddr != params.Bind {
		fmt.Printf("Serving session %s on %s://%s (%s):%d\n", sessionID, scheme, params.Bind, bindAddr, params.Port)
	} else {
		fmt.Printf("Serving session %s on %s://%s:%d\n", sessionID, scheme, params.Bind, params.Port)
	}
	fmt.Printf("  tmux session: %s\n", tmuxSession)
	fmt.Printf("  auth: %s / %s\n", params.User, params.Pass)

	if !params.NoTLS {
		fmt.Println("  tls: self-signed certificate (accept browser warning)")
	}

	printNetworkInfo(scheme, params.Port)

	connURL := connectionURL(scheme, params.Bind, params.Port, params.User, params.Pass)

	// Show QR code if requested
	if params.QR {
		if bindAddr == "127.0.0.1" {
			fmt.Println("\n  Warning: bound to localhost - QR URL won't be reachable from other devices.")
			fmt.Println("  Consider using --bind 0.0.0.0")
		}
		printQR(connURL)
	}

	if params.QR {
		fmt.Println("Press q or Ctrl+C to stop")
	} else {
		fmt.Println("Press space to show QR code, q or Ctrl+C to stop")
	}

	// Start keypress listener for spacebar QR toggle
	cleanup := startKeypressListener(func() {
		printQR(connURL)
	})
	defer cleanup()

	return ps.serve()
}
