package federationcmd

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type joinParams struct {
	Code      string `pos:"true" optional:"true" help:"Join code (tchj_…) from the hub admin (prefer --code-file or --code-stdin)"`
	CodeFile  string `long:"code-file" optional:"true" help:"Read the join code from a private file"`
	CodeStdin bool   `long:"code-stdin" help:"Read the join code from stdin"`
	Name      string `long:"name" optional:"true" help:"Display name on the hub (default: the current one, or user@hostname)"`
	Preview   bool   `long:"preview" help:"Show what the code holds and what joining changes, without joining"`
}

// joinPreview mirrors the daemon's /v1/federation/join response.
type joinPreview struct {
	HubURL    string           `json:"hub_url"`
	Space     string           `json:"space"`
	Invite    bool             `json:"invite"`
	ExpiresAt *time.Time       `json:"expires_at"`
	Expired   bool             `json:"expired"`
	CA        []proto.CertInfo `json:"ca"`
	Current   struct {
		HubURL    string `json:"hub_url"`
		Enabled   bool   `json:"enabled"`
		Name      string `json:"name"`
		HubCAFile string `json:"hub_ca_file"`
	} `json:"current"`
	HubCAFile string `json:"hub_ca_file"`
}

func joinCmd() *cobra.Command {
	return boa.CmdT[joinParams]{
		Use:   "join",
		Short: "Join a hub with a join code (hub address, invite and CA in one)",
		Long: `Join a hub with a join code from its admin ("tclaude hub invite --url …").

The code holds the hub address, a single-use invite and, for a hub with a
private or self-signed certificate, the CA to trust; the CA is stored under
~/.tclaude/data/federation/hub-ca/. Joining replaces this node's current hub
connection and connects. Running join is consent: use --preview to inspect
the code first. The dashboard does the same from ⚙ Fleet → Setup guide.`,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *joinParams, _ *cobra.Command, _ []string) {
			os.Exit(runJoin(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func readJoinCode(p *joinParams, stdin io.Reader) (string, error) {
	n := 0
	for _, set := range []bool{p.Code != "", p.CodeFile != "", p.CodeStdin} {
		if set {
			n++
		}
	}
	if n != 1 {
		return "", fmt.Errorf("provide exactly one of the code, --code-file or --code-stdin")
	}
	if p.Code != "" {
		return p.Code, nil
	}
	r := stdin
	if p.CodeFile != "" {
		f, err := os.Open(p.CodeFile)
		if err != nil {
			return "", err
		}
		defer f.Close()
		r = f
	}
	raw, err := io.ReadAll(io.LimitReader(r, proto.MaxJoinCodeBytes+1))
	if err != nil {
		return "", err
	}
	if len(raw) > proto.MaxJoinCodeBytes {
		return "", proto.ErrJoinCode
	}
	return string(raw), nil
}

func runJoin(p *joinParams, stdin io.Reader, stdout, stderr io.Writer) int {
	code, err := readJoinCode(p, stdin)
	if err != nil {
		return fail(stderr, err)
	}
	// Decoding here catches a mangled paste before the daemon is involved.
	if _, err := proto.DecodeJoinCode(code); err != nil {
		return fail(stderr, err)
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	in := map[string]any{"code": strings.TrimSpace(code), "preview": true}
	if p.Name != "" {
		in["name"] = p.Name
	}
	var preview joinPreview
	if err := agent.DaemonRequest(http.MethodPost, "/v1/federation/join", in, &preview, agent.DaemonOpts{NoRetry: true}); err != nil {
		return fail(stderr, err)
	}
	describeJoin(stderr, &preview, p.Name)
	if p.Preview {
		return 0
	}
	if preview.Expired {
		return fail(stderr, fmt.Errorf("the join code has expired; ask the hub admin for a new one"))
	}
	in["preview"] = false
	var joined joinPreview
	if err := agent.DaemonRequest(http.MethodPost, "/v1/federation/join", in, &joined, agent.DaemonOpts{NoRetry: true}); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "joining %s; check with: tclaude federation status\n", joined.HubURL)
	return 0
}

func describeJoin(w io.Writer, p *joinPreview, name string) {
	fmt.Fprintf(w, "Hub:     %s\n", p.HubURL)
	if p.Space != "" {
		fmt.Fprintf(w, "Space:   %s\n", p.Space)
	}
	switch {
	case !p.Invite:
		fmt.Fprintln(w, "Invite:  none (the hub admin admits this node by instance ID)")
	case p.ExpiresAt != nil && p.Expired:
		fmt.Fprintf(w, "Invite:  single use, EXPIRED %s\n", p.ExpiresAt.Local().Format(time.DateTime))
	case p.ExpiresAt != nil:
		fmt.Fprintf(w, "Invite:  single use, expires %s\n", p.ExpiresAt.Local().Format(time.DateTime))
	default:
		fmt.Fprintln(w, "Invite:  single use")
	}
	if len(p.CA) == 0 {
		fmt.Fprintln(w, "TLS:     the system's trusted certificates")
	}
	for _, c := range p.CA {
		self := ""
		if c.SelfSigned {
			self = ", self-signed"
		}
		fmt.Fprintf(w, "TLS:     trust %s (expires %s%s)\n         SHA-256 %s — compare it with the hub admin\n", c.Subject, c.NotAfter.Local().Format(time.DateOnly), self, c.SHA256)
	}
	if name == "" {
		name = p.Current.Name
	}
	fmt.Fprintf(w, "Name:    %s\n", name)
	if p.Current.HubURL != "" && p.Current.HubURL != p.HubURL {
		fmt.Fprintf(w, "Replaces the current hub %s: peers reachable only through it become unreachable.\n", p.Current.HubURL)
	}
	if p.Current.HubCAFile != "" && len(p.CA) == 0 {
		fmt.Fprintf(w, "Stops trusting the pinned CA %s for the hub.\n", p.Current.HubCAFile)
	}
}
