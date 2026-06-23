package remoteaccess

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/common"
	"golang.org/x/term"
)

// Cmd builds `tclaude remote-access` — manage the optional network-exposed
// dashboard listener (LAN / mesh / tunnel) and its mTLS + passphrase auth.
func Cmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "remote-access",
		Short: "Configure secure network access to the agentd dashboard (mTLS + passphrase)",
		Long: "Manage the optional, network-exposed agentd dashboard listener.\n\n" +
			"The local (loopback) dashboard is unchanged. This sets up a SEPARATE\n" +
			"HTTPS listener that requires BOTH a client certificate (installed on your\n" +
			"phone) and a passphrase — so you can reach the fleet dashboard over the LAN\n" +
			"(or a mesh VPN / tunnel) without exposing it unauthenticated.\n\n" +
			"  tclaude remote-access setup            # first-time: certs + passphrase + a phone .p12\n" +
			"  tclaude remote-access add-client       # issue another device's .p12\n" +
			"  tclaude remote-access status           # show config + issued devices\n",
		SubCmds: []*cobra.Command{
			setupCmd(),
			addClientCmd(),
			statusCmd(),
		},
	}.ToCobra()
}

type setupParams struct {
	Bind   string `long:"bind" default:"0.0.0.0:8443" help:"Address the remote listener binds to (e.g. 0.0.0.0:8443 for LAN, a tailnet IP for mesh, 127.0.0.1:8443 behind a tunnel)."`
	Hosts  string `long:"host" optional:"true" help:"Extra comma-separated names/IPs the server cert must be valid for (a tailnet name, a tunnel hostname). Local IPs + hostname are always included."`
	Client string `long:"client" default:"phone" help:"Name for the first device's client certificate."`
	Out    string `long:"out" optional:"true" help:"Where to write the device .p12 (default: ~/.tclaude/remote-access/clients/<name>.p12)."`
	Enable bool   `long:"enable" default:"true" help:"Also set remote_access.enabled + bind in config.json so agentd starts the listener (restart agentd to apply)."`

	// RegenerateCerts is required to re-run setup once material already
	// exists: by default setup REFUSES to clobber an existing CA (which would
	// invalidate every device's installed client cert). Pass it for a
	// deliberate fresh start; use `add-client` to add a device without it.
	RegenerateCerts bool `long:"regenerate-certs" help:"Regenerate ALL material (CA, server/client certs, cookie key, passphrase) even if already configured. WARNING: rotates the CA and INVALIDATES every client cert already installed on a device — you must re-issue and reinstall each device's .p12 afterward. To just add a device, use 'add-client' instead."`
}

func setupCmd() *cobra.Command {
	return boa.CmdT[setupParams]{
		Use:         "setup",
		Short:       "First-time setup: generate CA + server/client certs, set a passphrase, export a phone .p12",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *setupParams, _ *cobra.Command, _ []string) {
			if err := runSetup(p); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runSetup(p *setupParams) error {
	pass, err := promptNewSecret("Set remote-access passphrase", "Confirm passphrase", 8)
	if err != nil {
		return err
	}
	p12pw, err := promptNewSecret("Set a password to protect the device .p12 (you'll enter it once on the phone)", "Confirm .p12 password", 4)
	if err != nil {
		return err
	}

	res, err := Setup(SetupOptions{
		Bind:            p.Bind,
		ExtraHosts:      splitHosts(p.Hosts),
		Passphrase:      pass,
		ClientName:      p.Client,
		P12Password:     p12pw,
		P12Out:          p.Out,
		RegenerateCerts: p.RegenerateCerts,
	})
	if err != nil {
		return err
	}

	if p.Enable {
		if _, err := config.Update(func(cfg *config.Config, _ error) error {
			cfg.RemoteAccess = &config.RemoteAccessConfig{Enabled: true, Bind: p.Bind}
			return nil
		}); err != nil {
			return fmt.Errorf("enable remote_access in config: %w", err)
		}
	}

	fmt.Println("✅ Remote access configured.")
	fmt.Printf("   Material:   %s\n", res.Dir)
	fmt.Printf("   Bind:       %s\n", p.Bind)
	fmt.Printf("   Cert valid for: %s\n", strings.Join(res.Hosts, ", "))
	fmt.Printf("   Device .p12: %s  (client %q)\n", res.P12Path, res.ClientName)
	if p.Enable {
		fmt.Println("   config.json: remote_access.enabled = true — restart agentd to start the listener.")
	} else {
		fmt.Println("   NOTE: not enabled in config.json (--enable=false). Set remote_access.enabled to start the listener.")
	}
	fmt.Println()
	fmt.Println("On the phone:")
	fmt.Printf("  1. Transfer %s to the device and install it (iOS: Settings → Profile; Android: Settings → Security → install a certificate).\n", res.P12Path)
	fmt.Println("  2. Browse to https://<this-machine>:" + portOf(p.Bind) + " — accept the self-signed warning, pick the installed client cert, then enter the passphrase.")
	fmt.Println("  (A self-signed server cert shows a browser warning on LAN; a mesh/tunnel preset with a real cert avoids it.)")
	return nil
}

type addClientParams struct {
	Name string `long:"name" descr:"Name for the new device's client certificate." positional:"true"`
	Out  string `long:"out" optional:"true" help:"Where to write the device .p12 (default: ~/.tclaude/remote-access/clients/<name>.p12)."`
}

func addClientCmd() *cobra.Command {
	c := boa.CmdT[addClientParams]{
		Use:         "add-client <name>",
		Short:       "Issue another device's client certificate (.p12) from the existing CA",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *addClientParams, _ *cobra.Command, _ []string) {
			p12pw, err := promptNewSecret("Set a password to protect the device .p12", "Confirm .p12 password", 4)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			res, err := AddClient(p.Name, p12pw, p.Out)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("✅ Issued device %q → %s\n", res.Name, res.P12Path)
			fmt.Println("   Transfer it to the device and install it as a client certificate.")
		},
	}.ToCobra()
	return c
}

func statusCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "status",
		Short: "Show remote-access config and issued devices",
		RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			cfg, err := config.Load()
			if err != nil {
				// Load degrades to defaults on a parse error; warn so status
				// doesn't silently report misleading defaults as the truth.
				fmt.Fprintf(os.Stderr, "Warning: failed to load config.json (showing defaults): %v\n", err)
			}
			fmt.Printf("Material:  %s (configured: %v)\n", Dir(), Exists())
			fmt.Printf("Enabled:   %v\n", cfg.RemoteAccessEnabled())
			fmt.Printf("Bind:      %s\n", orNone(cfg.RemoteAccessBind()))
			if entries, err := os.ReadDir(clientsDir()); err == nil {
				var names []string
				for _, e := range entries {
					if strings.HasSuffix(e.Name(), ".crt") {
						names = append(names, strings.TrimSuffix(e.Name(), ".crt"))
					}
				}
				fmt.Printf("Devices:   %s\n", orNone(strings.Join(names, ", ")))
			}
		},
	}.ToCobra()
}

// promptNewSecret reads a secret twice (hidden when stdin is a terminal) and
// requires the two entries to match and meet a minimum length.
func promptNewSecret(label, confirmLabel string, minLen int) (string, error) {
	s1, err := promptSecret(label + ": ")
	if err != nil {
		return "", err
	}
	if len(s1) < minLen {
		return "", fmt.Errorf("must be at least %d characters", minLen)
	}
	s2, err := promptSecret(confirmLabel + ": ")
	if err != nil {
		return "", err
	}
	if s1 != s2 {
		return "", fmt.Errorf("entries did not match")
	}
	return s1, nil
}

// stdinReader is a single shared buffered reader for the non-terminal prompt
// path. It MUST be shared across prompts: a fresh bufio.Reader per call would
// buffer (and then discard) the rest of stdin, so the next prompt would hit
// EOF — which is exactly what a piped `pass\npass\np12\np12` would do.
var stdinReader *bufio.Reader

// promptSecret prints label to stderr and reads one secret line — with echo
// disabled when stdin is a terminal, or as a plain line (for scripted use)
// otherwise.
func promptSecret(label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	if stdinReader == nil {
		stdinReader = bufio.NewReader(os.Stdin)
	}
	line, err := stdinReader.ReadString('\n')
	trimmed := strings.TrimSpace(line)
	// A final piped line without a trailing newline returns the line AND
	// io.EOF — that's a successful read, not a failure. Only a non-EOF error
	// (or EOF with nothing read) is a real failure. This keeps scripted
	// `printf 'pass\npass\np12\np12'` (no trailing newline) working.
	if err != nil && (!errors.Is(err, io.EOF) || trimmed == "") {
		return "", err
	}
	return trimmed, nil
}

func splitHosts(s string) []string {
	var out []string
	for _, h := range strings.Split(s, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}

func portOf(bind string) string {
	if i := strings.LastIndexByte(bind, ':'); i >= 0 {
		return bind[i+1:]
	}
	return bind
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
