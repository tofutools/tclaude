// Package hubcmd is the CLI of the standalone tclaude-hub binary.
package hubcmd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"

	"github.com/tofutools/tclaude/pkg/claude/cli"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/federation/hub"
)

const long = `tclaude-hub relays messages between federated tclaude agentd instances.

Every agentd dials out to the hub, so no instance needs an inbound listener.
The hub admits instances (allow-list or single-use invites), scopes who can
see whom with spaces, rate-limits, and routes signed envelopes. It stores no
messages and cannot forge or alter them; each agentd enforces its own
permissions.

Admin commands edit the hub database directly; a running hub picks the
changes up within its policy refresh interval (default 15s).`

// RootCmd returns the tclaude-hub root command.
func RootCmd() *cobra.Command {
	cmd := boa.CmdT[struct{}]{
		Use:         "tclaude-hub",
		Short:       "Federation hub for tclaude agentd instances",
		Long:        long,
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			serveCmd(), admitCmd(), revokeCmd(), spacesCmd(), inviteCmd(), lsCmd(), invitesCmd(),
		},
	}.ToCobra()
	cli.ConfigureRoot(cmd)
	return cmd
}

// DefaultDBPath is $TCLAUDE_HUB_DIR/hub.sqlite, else ~/.tclaude-hub/hub.sqlite.
func DefaultDBPath() string {
	if d := os.Getenv("TCLAUDE_HUB_DIR"); d != "" {
		return filepath.Join(d, "hub.sqlite")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".tclaude-hub", "hub.sqlite")
}

type dbParam struct {
	DB string `long:"db" optional:"true" help:"Hub database path (default $TCLAUDE_HUB_DIR/hub.sqlite or ~/.tclaude-hub/hub.sqlite)"`
}

func (p dbParam) open() (*hub.Store, error) {
	path := p.DB
	if path == "" {
		path = DefaultDBPath()
	}
	return hub.OpenStore(path)
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	os.Exit(1)
}

type serveParams struct {
	dbParam
	Listen          string        `long:"listen" default:"127.0.0.1:8470" help:"Listen address"`
	TLSCert         string        `long:"tls-cert" optional:"true" help:"TLS certificate (PEM). Without it the hub serves plain HTTP, which clients only accept on loopback; front it with a TLS proxy otherwise"`
	TLSKey          string        `long:"tls-key" optional:"true" help:"TLS private key (PEM)"`
	Open            bool          `long:"open" help:"Admit any instance that proves key possession (development only)"`
	FramesPerMinute int           `long:"frames-per-minute" default:"120" help:"Per-instance send rate limit (frames)"`
	BytesPerMinute  int           `long:"bytes-per-minute" default:"2097152" help:"Per-instance send rate limit (bytes)"`
	PolicyRefresh   time.Duration `long:"policy-refresh" default:"15s" help:"How often admin edits are re-read"`
}

func serveCmd() *cobra.Command {
	return boa.CmdT[serveParams]{
		Use:         "serve",
		Short:       "Run the hub",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *serveParams, _ *cobra.Command, _ []string) {
			if (p.TLSCert == "") != (p.TLSKey == "") {
				fail(fmt.Errorf("--tls-cert and --tls-key go together"))
			}
			st, err := p.open()
			if err != nil {
				fail(err)
			}
			defer func() { _ = st.Close() }()
			h, err := hub.New(st, hub.Config{
				Open: p.Open, FramesPerMinute: p.FramesPerMinute, BytesPerMinute: p.BytesPerMinute,
				PolicyRefresh: p.PolicyRefresh, Version: buildversion.AppVersion(),
			})
			if err != nil {
				fail(err)
			}
			scheme := "ws"
			if p.TLSCert != "" {
				scheme = "wss"
			}
			fmt.Fprintf(os.Stderr, "tclaude-hub %s listening on %s://%s (open=%v)\n", h.ID(), scheme, p.Listen, p.Open)
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			srv := &http.Server{Addr: p.Listen, ReadHeaderTimeout: 10 * time.Second}
			if err := hub.ListenAndServe(ctx, srv, h, p.TLSCert, p.TLSKey); err != nil {
				fail(err)
			}
		},
	}.ToCobra()
}

type admitParams struct {
	dbParam
	Instance string   `pos:"true" help:"Instance id (inst_…) as printed by 'tclaude federation identity'"`
	Space    []string `long:"space" optional:"true" help:"Space(s) to admit into (default: default)"`
}

func admitCmd() *cobra.Command {
	return boa.CmdT[admitParams]{
		Use:         "admit",
		Short:       "Admit an instance into one or more spaces",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *admitParams, _ *cobra.Command, _ []string) {
			st, err := p.open()
			if err != nil {
				fail(err)
			}
			defer func() { _ = st.Close() }()
			if err := st.Admit(p.Instance, p.Space...); err != nil {
				fail(err)
			}
			fmt.Println("admitted", p.Instance)
		},
	}.ToCobra()
}

type instanceParams struct {
	dbParam
	Instance string `pos:"true" help:"Instance id (inst_…)"`
}

func revokeCmd() *cobra.Command {
	return boa.CmdT[instanceParams]{
		Use:         "revoke",
		Short:       "Revoke an instance (drops its live connection at the next policy refresh)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *instanceParams, _ *cobra.Command, _ []string) {
			st, err := p.open()
			if err != nil {
				fail(err)
			}
			defer func() { _ = st.Close() }()
			if err := st.Revoke(p.Instance); err != nil {
				fail(err)
			}
			fmt.Println("revoked", p.Instance)
		},
	}.ToCobra()
}

type spacesParams struct {
	dbParam
	Instance string `pos:"true" help:"Instance id (inst_…)"`
	Spaces   string `pos:"true" help:"Comma-separated spaces that replace the current set"`
}

func spacesCmd() *cobra.Command {
	return boa.CmdT[spacesParams]{
		Use:         "spaces",
		Short:       "Replace an instance's spaces",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *spacesParams, _ *cobra.Command, _ []string) {
			st, err := p.open()
			if err != nil {
				fail(err)
			}
			defer func() { _ = st.Close() }()
			var sp []string
			for _, s := range strings.Split(p.Spaces, ",") {
				if s = strings.TrimSpace(s); s != "" {
					sp = append(sp, s)
				}
			}
			if err := st.SetSpaces(p.Instance, sp); err != nil {
				fail(err)
			}
			fmt.Println("spaces set", p.Instance, sp)
		},
	}.ToCobra()
}

type inviteParams struct {
	dbParam
	Space string        `long:"space" default:"default" help:"Space the invitee joins"`
	TTL   time.Duration `long:"ttl" default:"24h" help:"Invite validity"`
}

func inviteCmd() *cobra.Command {
	return boa.CmdT[inviteParams]{
		Use:         "invite",
		Short:       "Mint a single-use invite token",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *inviteParams, _ *cobra.Command, _ []string) {
			st, err := p.open()
			if err != nil {
				fail(err)
			}
			defer func() { _ = st.Close() }()
			tok, err := st.CreateInvite(p.Space, p.TTL)
			if err != nil {
				fail(err)
			}
			fmt.Println(tok)
		},
	}.ToCobra()
}

func lsCmd() *cobra.Command {
	return boa.CmdT[dbParam]{
		Use:         "ls",
		Short:       "List known instances",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *dbParam, _ *cobra.Command, _ []string) {
			st, err := p.open()
			if err != nil {
				fail(err)
			}
			defer func() { _ = st.Close() }()
			all, err := st.List()
			if err != nil {
				fail(err)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "INSTANCE\tNAME\tSPACES\tSTATE\tLAST SEEN\tVERSION")
			for _, in := range all {
				state := "admitted"
				if in.Revoked {
					state = "revoked"
				}
				seen := "-"
				if !in.LastSeen.IsZero() {
					seen = in.LastSeen.Local().Format(time.DateTime)
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", in.ID, in.Name, strings.Join(in.Spaces, ","), state, seen, in.Version)
			}
			_ = tw.Flush()
		},
	}.ToCobra()
}

func invitesCmd() *cobra.Command {
	return boa.CmdT[dbParam]{
		Use:         "invites",
		Short:       "List invites (hashes only)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *dbParam, _ *cobra.Command, _ []string) {
			st, err := p.open()
			if err != nil {
				fail(err)
			}
			defer func() { _ = st.Close() }()
			all, err := st.ListInvites()
			if err != nil {
				fail(err)
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "HASH\tSPACE\tEXPIRES\tUSED BY")
			for _, in := range all {
				_, _ = fmt.Fprintf(tw, "%s…\t%s\t%s\t%s\n", in.Hash[:12], in.Space, in.ExpiresAt.Local().Format(time.DateTime), in.UsedBy)
			}
			_ = tw.Flush()
		},
	}.ToCobra()
}
