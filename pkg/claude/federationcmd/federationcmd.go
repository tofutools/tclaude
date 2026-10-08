// Package federationcmd is the operator CLI for federation:
// `tclaude federation …`. Every verb talks to the local agentd's
// human-only /v1/federation/* API; agents send remote mail with the
// ordinary `tclaude agent message member@peer`.
package federationcmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
)

const long = `Link this tclaude instance to others through a tclaude-hub.

Flow:
  1. tclaude federation identity            show this instance's id; give it to the hub admin
                                             (or get an invite token from them)
  2. tclaude federation connect <hub-url>   [--invite <token>]
  3. tclaude federation peers               see visible instances
     tclaude federation trust <instance> --label bob
  4. tclaude federation grant bob message.direct --scope group=<group>
                                             let bob see / mail a local group
  5. tclaude federation remote              see what peers share with you
     tclaude agent permissions grant <agent> message.direct --scope peer=bob/<group>
                                             let that agent address bob's group
  6. agents: tclaude agent message <member>@bob "..." (needs the
     message.direct slug scoped with peer=bob[/group]); replies need nothing extra.

  7. agents: tclaude federation spawn-request <group>@bob --brief "..."
     (needs the groups.members.spawn scoped with peer=bob/group, or agent.spawn scoped with peer=bob; bob's operator approves or denies)

Every federation command is human-only except nodes (node.read), spawn-request, sessions (sessions.read),
and attach (sessions.watch or sessions.attach). Agents need the matching peer= scope.`

// Cmd returns `tclaude federation`.
func Cmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "federation",
		Short:       "Link this instance with other tclaude instances through a hub",
		Long:        long,
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			statusCmd(), identityCmd(), connectCmd(), disconnectCmd(),
			peersCmd(), trustCmd(), untrustCmd(), nodeProfilesCmd(), enrollTokenCmd(), enrollCmd(), enrollmentsCmd(),
			grantCmd(), revokeCmd(), grantsCmd(), remoteCmd(), nodesCmd(), nodeLabelsCmd(), sessionsCmd(), attachCmd(), viewersCmd(), kickCmd(),
			sendCmd(), outboxCmd(), notifyCmd(), inboxCmd(), awayCmd(), returnCmd(), answerCmd(),
			spawnRequestCmd(), requestsCmd(), offerConfigCmd(), offersCmd(), shareAgentCmd(), moveAgentCmd(), movesCmd(),
		},
	}.ToCobra()
}

// --- wire types (mirror agentd federation_api.go) ---

type status struct {
	Enabled     bool   `json:"enabled"`
	InstanceID  string `json:"instance_id"`
	Fingerprint string `json:"fingerprint"`
	Name        string `json:"name"`
	HubURL      string `json:"hub_url"`
	Hub         *struct {
		State     string    `json:"state"`
		HubID     string    `json:"hub_id"`
		Spaces    []string  `json:"spaces"`
		LastError string    `json:"last_error"`
		Since     time.Time `json:"since"`
	} `json:"hub"`
	Peers []struct {
		Level       string    `json:"level"`
		InstanceID  string    `json:"instance_id"`
		Fingerprint string    `json:"fingerprint"`
		Label       string    `json:"label"`
		Name        string    `json:"name"`
		Trusted     bool      `json:"trusted"`
		Online      bool      `json:"online"`
		LastSeen    time.Time `json:"last_seen"`
		Version     string    `json:"version"`
	} `json:"peers"`
	PeerGrants []peerGrant    `json:"peer_grants"`
	Outbox     map[string]int `json:"outbox"`
	Remote     []struct {
		Peer       string    `json:"peer"`
		Label      string    `json:"label"`
		Online     bool      `json:"online"`
		ReceivedAt time.Time `json:"catalog_received_at"`
		Groups     []struct {
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Caps        []string `json:"caps"`
			Members     []struct {
				Agent    string `json:"agent"`
				Name     string `json:"name"`
				Role     string `json:"role"`
				Presence string `json:"presence"`
			} `json:"members"`
			Routes []struct {
				Publisher string `json:"publisher"`
				Name      string `json:"name"`
			} `json:"routes"`
		} `json:"groups"`
	} `json:"remote"`
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "Error: %v\n", err)
	return agent.MapDaemonErrorToRC(err)
}

func loadStatus(stderr io.Writer) (*status, int) {
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return nil, rc
	}
	var st status
	if err := agent.DaemonGet("/v1/federation/status", &st); err != nil {
		return nil, fail(stderr, err)
	}
	return &st, 0
}

func printJSON(w io.Writer, v any) int {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
	return 0
}

func post(stderr io.Writer, path string, in, out any) int {
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	if err := agent.DaemonPost(path, in, out); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func del(stderr io.Writer, path string, in any) int {
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	if err := agent.DaemonRequest(http.MethodDelete, path, in, nil, agent.DaemonOpts{}); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return d.String() + " ago"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.Local().Format(time.DateOnly)
}

type jsonParam struct {
	JSON bool `long:"json" help:"Output JSON"`
}

// --- status / identity ---

func statusCmd() *cobra.Command {
	return boa.CmdT[jsonParam]{
		Use:         "status",
		Short:       "Show identity, hub connection, peers, peer grants",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *jsonParam, _ *cobra.Command, _ []string) {
			os.Exit(runStatus(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runStatus(p *jsonParam, stdout, stderr io.Writer) int {
	st, rc := loadStatus(stderr)
	if st == nil {
		return rc
	}
	if p.JSON {
		return printJSON(stdout, st)
	}
	fmt.Fprintf(stdout, "Instance:    %s (%s)\n", st.InstanceID, st.Name)
	fmt.Fprintf(stdout, "Fingerprint: %s\n", st.Fingerprint)
	switch {
	case !st.Enabled:
		fmt.Fprintf(stdout, "Hub:         disabled (tclaude federation connect <hub-url>)\n")
	case st.Hub == nil:
		fmt.Fprintf(stdout, "Hub:         %s (not running)\n", st.HubURL)
	default:
		fmt.Fprintf(stdout, "Hub:         %s — %s", st.HubURL, st.Hub.State)
		if st.Hub.HubID != "" {
			fmt.Fprintf(stdout, " (%s, spaces %s)", st.Hub.HubID, strings.Join(st.Hub.Spaces, ","))
		}
		fmt.Fprintln(stdout)
		if st.Hub.LastError != "" {
			fmt.Fprintf(stdout, "             last error: %s\n", st.Hub.LastError)
		}
	}
	trusted, visible := 0, 0
	for _, pe := range st.Peers {
		if pe.Trusted {
			trusted++
		} else {
			visible++
		}
	}
	fmt.Fprintf(stdout, "Peers:       %d trusted, %d visible untrusted\n", trusted, visible)
	for _, pe := range st.Peers {
		if pe.Trusted {
			fmt.Fprintf(stdout, "  %s: %s\n", pe.InstanceID, pe.Level)
		}
	}
	fmt.Fprintf(stdout, "Peer grants: %d\n", len(st.PeerGrants))
	if len(st.Outbox) > 0 {
		var parts []string
		for _, k := range []string{"queued", "sent", "accepted", "refused", "expired"} {
			if n := st.Outbox[k]; n > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", k, n))
			}
		}
		fmt.Fprintf(stdout, "Outbox:      %s\n", strings.Join(parts, ", "))
	}
	return 0
}

func identityCmd() *cobra.Command {
	return boa.CmdT[jsonParam]{
		Use:         "identity",
		Short:       "Print this instance's federation id and fingerprint (give the id to a hub admin)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *jsonParam, _ *cobra.Command, _ []string) {
			st, rc := loadStatus(os.Stderr)
			if st == nil {
				os.Exit(rc)
			}
			if p.JSON {
				os.Exit(printJSON(os.Stdout, map[string]string{"instance_id": st.InstanceID, "fingerprint": st.Fingerprint, "name": st.Name}))
			}
			fmt.Println(st.InstanceID)
			fmt.Fprintf(os.Stderr, "fingerprint %s  name %s\n", st.Fingerprint, st.Name)
		},
	}.ToCobra()
}

// --- connect / disconnect ---

type connectParams struct {
	HubURL string `pos:"true" help:"Hub URL: wss://host[:port], or ws:// to a loopback hub"`
	Invite string `long:"invite" optional:"true" help:"Single-use invite token from the hub admin"`
	Name   string `long:"name" optional:"true" help:"Display name on the hub (default user@hostname)"`
	CAFile string `long:"ca-file" optional:"true" help:"PEM bundle to trust for the hub's TLS certificate"`
}

func connectCmd() *cobra.Command {
	return boa.CmdT[connectParams]{
		Use:         "connect",
		Short:       "Configure and enable the hub connection",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *connectParams, _ *cobra.Command, _ []string) {
			on := true
			req := map[string]any{"enabled": on, "hub_url": p.HubURL}
			if p.Invite != "" {
				req["invite"] = p.Invite
			}
			if p.Name != "" {
				req["name"] = p.Name
			}
			if p.CAFile != "" {
				// agentd reads the file itself, from its own working
				// directory, so a relative path must be resolved here.
				ca, err := filepath.Abs(p.CAFile)
				if err != nil {
					fmt.Fprintln(os.Stderr, err)
					os.Exit(1)
				}
				req["hub_ca_file"] = ca
			}
			if rc := post(os.Stderr, "/v1/federation/config", req, nil); rc != 0 {
				os.Exit(rc)
			}
			fmt.Println("federation enabled; check with: tclaude federation status")
		},
	}.ToCobra()
}

func disconnectCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "disconnect",
		Short:       "Disable the hub connection (keeps peers, peer grants)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			if rc := post(os.Stderr, "/v1/federation/config", map[string]any{"enabled": false}, nil); rc != 0 {
				os.Exit(rc)
			}
			fmt.Println("federation disabled")
		},
	}.ToCobra()
}

// --- peers ---

func peersCmd() *cobra.Command {
	return boa.CmdT[jsonParam]{
		Use:         "peers",
		Short:       "List trusted peers and instances the hub makes visible",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *jsonParam, _ *cobra.Command, _ []string) {
			st, rc := loadStatus(os.Stderr)
			if st == nil {
				os.Exit(rc)
			}
			if p.JSON {
				os.Exit(printJSON(os.Stdout, st.Peers))
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "INSTANCE\tLABEL\tNAME\tTRUSTED\tLEVEL\tONLINE\tLAST SEEN\tFINGERPRINT")
			for _, pe := range st.Peers {
				online := "no"
				if pe.Online {
					online = "yes"
				}
				trusted := "no"
				if pe.Trusted {
					trusted = "yes"
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", pe.InstanceID, dash(pe.Label), dash(pe.Name), trusted, dash(pe.Level), online, ago(pe.LastSeen), pe.Fingerprint)
			}
			_ = tw.Flush()
		},
	}.ToCobra()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

type trustParams struct {
	Profile          string `long:"profile" optional:"true" help:"Apply this local node profile at trust"`
	NoDefaultProfile bool   `long:"no-default-profile" help:"Skip the default peer profile"`
	Level            string `long:"level" optional:"true" help:"Local trust level: restricted or unrestricted (own machines only)"`
	Yes              bool   `long:"yes" help:"Confirm unrestricted access without prompting"`
	Instance         string `pos:"true" help:"Instance id (or 8+ char prefix) from 'tclaude federation peers'"`
	Label            string `long:"label" optional:"true" help:"Short local name for the peer, used in addresses (member@label)"`
}

func trustCmd() *cobra.Command {
	return boa.CmdT[trustParams]{
		Use:         "trust",
		Short:       "Trust a visible instance (compare its fingerprint out of band first)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *trustParams, _ *cobra.Command, _ []string) {
			if p.Level != "" && p.Level != "restricted" && p.Level != "unrestricted" {
				fmt.Fprintln(os.Stderr, "level must be restricted or unrestricted")
				os.Exit(1)
			}
			in := map[string]any{"instance": p.Instance, "label": p.Label, "level": p.Level, "profile": p.Profile, "no_default_profile": p.NoDefaultProfile, "preview": true}
			var preview struct {
				InstanceID  string                        `json:"instance_id"`
				Fingerprint string                        `json:"fingerprint"`
				Level       string                        `json:"level"`
				Plan        *db.FederationNodeProfilePlan `json:"plan"`
			}
			if rc := post(os.Stderr, "/v1/federation/peers/trust", in, &preview); rc != 0 {
				os.Exit(rc)
			}
			if preview.Plan != nil {
				if rc := printJSON(os.Stdout, preview.Plan); rc != 0 {
					os.Exit(rc)
				}
				if len(preview.Plan.Conflicts) > 0 {
					fmt.Fprintln(os.Stderr, "profile has manual-edit conflicts")
					os.Exit(1)
				}
				in["preview_token"] = preview.Plan.Token
			}
			if preview.Level == "unrestricted" {
				if e := confirmNodeProfileUnrestricted(preview.Fingerprint, p.Yes); e != nil {
					os.Exit(fail(os.Stderr, e))
				}
				in["confirm_fingerprint"] = preview.Fingerprint
			}
			in["preview"] = false
			var out struct {
				InstanceID  string `json:"instance_id"`
				Fingerprint string `json:"fingerprint"`
			}
			if rc := post(os.Stderr, "/v1/federation/peers/trust", in, &out); rc != 0 {
				os.Exit(rc)
			}
			fmt.Printf("trusted %s (fingerprint %s)\n", out.InstanceID, out.Fingerprint)
		},
	}.ToCobra()
}

type peerParam struct {
	Peer string `pos:"true" help:"Peer label, name, or instance id"`
}

func untrustCmd() *cobra.Command {
	return boa.CmdT[peerParam]{
		Use:         "untrust",
		Short:       "Stop trusting a peer; its grants and cached catalog are removed",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *peerParam, _ *cobra.Command, _ []string) {
			if rc := post(os.Stderr, "/v1/federation/peers/untrust", map[string]any{"instance": p.Peer}, nil); rc != 0 {
				os.Exit(rc)
			}
			fmt.Println("untrusted", p.Peer)
		},
	}.ToCobra()
}

// --- peer grants ---
type peerGrant struct {
	PoolID      string `json:"pool_id,omitempty"`
	PoolName    string `json:"pool_name,omitempty"`
	Peer        string `json:"peer"`
	Slug        string `json:"slug"`
	Scope       string `json:"scope"`
	SpawnPolicy struct {
		Profile string `json:"profile,omitempty"`
		Cwd     string `json:"cwd,omitempty"`
		Harness string `json:"harness,omitempty"`
		Model   string `json:"model,omitempty"`
		MaxLive int    `json:"max_live,omitempty"`
	} `json:"spawn_policy,omitempty"`
}
type grantParams struct {
	Peer    string `pos:"true" help:"Trusted peer label or instance id"`
	Slug    string `pos:"true" help:"Permission slug to grant"`
	Scope   string `long:"scope" optional:"true" help:"group=<local group>; omitted covers all current and future groups"`
	Profile string `long:"profile" optional:"true" help:"Receiver launch profile for groups.members.spawn"`
	Cwd     string `long:"cwd" optional:"true" help:"Receiver worker directory"`
	Harness string `long:"harness" optional:"true" help:"Receiver worker harness"`
	Model   string `long:"model" optional:"true" help:"Receiver worker model"`
	MaxLive int    `long:"max-live" optional:"true" help:"Positive live auto-worker cap (default 2)"`
}

func grantCmd() *cobra.Command {
	return boa.CmdT[grantParams]{Use: "grant", Short: "Grant a trusted peer group or instance permission (human only)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *grantParams, _ *cobra.Command, _ []string) {
		grant := peerGrant{Peer: p.Peer, Slug: p.Slug, Scope: p.Scope}
		grant.SpawnPolicy.Profile = p.Profile
		grant.SpawnPolicy.Cwd = p.Cwd
		grant.SpawnPolicy.Harness = p.Harness
		grant.SpawnPolicy.Model = p.Model
		grant.SpawnPolicy.MaxLive = p.MaxLive
		var resp struct {
			Warnings []string `json:"warnings"`
		}
		if rc := post(os.Stderr, "/v1/federation/grants", grant, &resp); rc != 0 {
			os.Exit(rc)
		}
		for _, warning := range resp.Warnings {
			fmt.Fprintln(os.Stderr, warning)
		}
		fmt.Printf("granted %s to %s (%s)\n", p.Slug, p.Peer, p.Scope)
	}}.ToCobra()
}

type revokeParams struct {
	Peer  string `pos:"true" help:"Trusted peer label or instance id"`
	Slug  string `pos:"true" help:"Permission slug to revoke"`
	Scope string `long:"scope" optional:"true" help:"group=<local group>; omitted revokes the unscoped grant"`
}

func revokeCmd() *cobra.Command {
	return boa.CmdT[revokeParams]{Use: "revoke", Short: "Revoke a peer grant (human only)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *revokeParams, _ *cobra.Command, _ []string) {
		if rc := del(os.Stderr, "/v1/federation/grants", map[string]any{"peer": p.Peer, "slug": p.Slug, "scope": p.Scope}); rc != 0 {
			os.Exit(rc)
		}
		fmt.Printf("revoked %s from %s\n", p.Slug, p.Peer)
	}}.ToCobra()
}

type grantsParams struct {
	Peer string `pos:"true" help:"Trusted peer label or instance id"`
	JSON bool   `long:"json" optional:"true" help:"Print JSON"`
}

func grantsCmd() *cobra.Command {
	return boa.CmdT[grantsParams]{Use: "grants", Short: "List a peer's grants (human only)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *grantsParams, _ *cobra.Command, _ []string) {
		var resp struct {
			Grants []peerGrant `json:"grants"`
		}
		if err := agent.DaemonGet("/v1/federation/grants?peer="+url.QueryEscape(p.Peer), &resp); err != nil {
			os.Exit(fail(os.Stderr, err))
		}
		if p.JSON {
			if rc := printJSON(os.Stdout, resp); rc != 0 {
				os.Exit(rc)
			}
			return
		}
		for _, g := range resp.Grants {
			fmt.Printf("%s %s %s", g.Peer, g.Slug, g.Scope)
			if g.PoolName != "" && g.Peer != "group:"+g.PoolName {
				fmt.Printf(" [inherited from group:%s]", g.PoolName)
			}
			if g.Slug == "groups.members.spawn" {
				policy, _ := json.Marshal(g.SpawnPolicy)
				fmt.Printf(" %s", policy)
			}
			fmt.Println()
		}
	}}.ToCobra()
}

func remoteCmd() *cobra.Command {
	return boa.CmdT[jsonParam]{
		Use:         "remote",
		Short:       "Show what each trusted peer grants you, plus your peer grants",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *jsonParam, _ *cobra.Command, _ []string) {
			os.Exit(runRemote(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runRemote(p *jsonParam, stdout, stderr io.Writer) int {
	st, rc := loadStatus(stderr)
	if st == nil {
		return rc
	}
	if p.JSON {
		return printJSON(stdout, map[string]any{"remote": st.Remote, "peer_grants": st.PeerGrants})
	}
	if len(st.Remote) == 0 {
		fmt.Fprintln(stdout, "No trusted peers. See: tclaude federation peers / trust")
	}
	for _, r := range st.Remote {
		state := "offline"
		if r.Online {
			state = "online"
		}
		fmt.Fprintf(stdout, "%s (%s, %s, catalog %s)\n", r.Label, r.Peer, state, ago(r.ReceivedAt))
		if len(r.Groups) == 0 {
			fmt.Fprintln(stdout, "  grants you nothing")
		}
		for _, g := range r.Groups {
			line := fmt.Sprintf("  %s/%s  [%s]", r.Label, g.Name, strings.Join(g.Caps, ","))
			fmt.Fprintln(stdout, line)
			for _, m := range g.Members {
				extra := ""
				if m.Role != "" {
					extra += " role=" + m.Role
				}
				if m.Presence != "" {
					extra += " " + m.Presence
				}
				fmt.Fprintf(stdout, "    %s@%s%s\n", m.Name, r.Label, extra)
			}
			for _, rt := range g.Routes {
				fmt.Fprintf(stdout, "    route %s/%s@%s\n", rt.Publisher, rt.Name, r.Label)
			}
		}
	}
	if len(st.PeerGrants) > 0 {
		fmt.Fprintln(stdout, "\nPeer grants:")
		for _, g := range st.PeerGrants {
			fmt.Fprintf(stdout, "  %s %s %s\n", g.Peer, g.Slug, g.Scope)
		}
	}
	return 0
}

// --- mail ---

type sendParams struct {
	To      string   `pos:"true" help:"member@peer, or group:<group>@peer for every member of a remote group"`
	Body    string   `pos:"true" help:"Message body"`
	Subject string   `long:"subject" optional:"true" help:"Subject"`
	Role    string   `long:"role" optional:"true" help:"With group:<group>@peer, only members holding this role (needs the group's roster grant)"`
	Attach  []string `long:"attach" short:"a" optional:"true" help:"Attach a file (repeatable; the remote group must accept attachments)"`
}

func sendCmd() *cobra.Command {
	return boa.CmdT[sendParams]{
		Use:         "send",
		Short:       "Send remote mail as the human operator",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *sendParams, _ *cobra.Command, _ []string) {
			var out struct {
				EnvelopeID string `json:"envelope_id"`
				To         string `json:"to"`
				State      string `json:"state"`
				Connected  bool   `json:"hub_connected"`
			}
			req := map[string]any{"to": p.To, "body": p.Body, "subject": p.Subject}
			if p.Role != "" {
				req["role"] = p.Role
			}
			if len(p.Attach) > 0 {
				atts, err := agent.ReadRemoteAttachments(p.Attach)
				if err != nil {
					os.Exit(fail(os.Stderr, err))
				}
				req["attachments"] = atts
			}
			if rc := post(os.Stderr, "/v1/federation/send", req, &out); rc != 0 {
				os.Exit(rc)
			}
			fmt.Printf("%s to %s (envelope %s)\n", out.State, out.To, out.EnvelopeID[:12])
			if !out.Connected {
				fmt.Fprintln(os.Stderr, "hub not connected; the message will be sent when it is")
			}
		},
	}.ToCobra()
}

type notifyParams struct {
	Peer    string `pos:"true" help:"Trusted peer (label, name or instance id)"`
	Body    string `pos:"true" help:"Message body"`
	Subject string `long:"subject" optional:"true" help:"Subject"`
}

func notifyCmd() *cobra.Command {
	return boa.CmdT[notifyParams]{
		Use:         "notify",
		Short:       "Message a trusted peer's human operator (lands in their Messages inbox)",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *notifyParams, _ *cobra.Command, _ []string) {
			var out struct {
				EnvelopeID string `json:"envelope_id"`
				To         string `json:"to"`
				State      string `json:"state"`
				Connected  bool   `json:"hub_connected"`
			}
			if rc := post(os.Stderr, "/v1/federation/notify", map[string]any{"peer": p.Peer, "body": p.Body, "subject": p.Subject}, &out); rc != 0 {
				os.Exit(rc)
			}
			fmt.Printf("%s to %s (envelope %s)\n", out.State, out.To, out.EnvelopeID[:12])
			if !out.Connected {
				fmt.Fprintln(os.Stderr, "hub not connected; the message will be sent when it is")
			}
		},
	}.ToCobra()
}

type inboxParams struct {
	JSON   bool `long:"json" help:"Output JSON"`
	Unread bool `long:"unread" help:"Only unread messages"`
}

func inboxCmd() *cobra.Command {
	return boa.CmdT[inboxParams]{
		Use:         "inbox",
		Short:       "Show messages remote operators sent you",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *inboxParams, _ *cobra.Command, _ []string) {
			if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
				os.Exit(rc)
			}
			type msg struct {
				ID         int64     `json:"id"`
				OfferID    string    `json:"offer_id,omitempty"`
				OfferState string    `json:"offer_state,omitempty"`
				From       string    `json:"from"`
				Instance   string    `json:"instance"`
				Subject    string    `json:"subject,omitempty"`
				Body       string    `json:"body"`
				CreatedAt  time.Time `json:"created_at"`
				Read       bool      `json:"read"`
			}
			var rows []msg
			if err := agent.DaemonGet("/v1/federation/inbox", &rows); err != nil {
				os.Exit(fail(os.Stderr, err))
			}
			kept := rows[:0]
			for _, r := range rows {
				if !p.Unread || !r.Read {
					kept = append(kept, r)
				}
			}
			if p.JSON {
				os.Exit(printJSON(os.Stdout, kept))
			}
			if len(kept) == 0 {
				fmt.Println("no remote operator messages")
				return
			}
			for _, r := range kept {
				state := "unread"
				if r.Read {
					state = "read"
				}
				if r.OfferID != "" {
					fmt.Printf("offer %s  %s  %s\n", r.OfferID, r.From, r.OfferState)
				} else {
					fmt.Printf("#%d  %s  %s  %s\n", r.ID, r.From, ago(r.CreatedAt), state)
				}
				if r.Subject != "" {
					fmt.Printf("Subject: %s\n", r.Subject)
				}
				fmt.Printf("%s\n\n", strings.TrimRight(r.Body, "\n"))
			}
		},
	}.ToCobra()
}

type outboxParams struct {
	JSON  bool `long:"json" help:"Output JSON"`
	Limit int  `long:"limit" default:"30" help:"Rows to show"`
}

func outboxCmd() *cobra.Command {
	return boa.CmdT[outboxParams]{
		Use:         "outbox",
		Short:       "Show outbound remote mail and its delivery state",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *outboxParams, _ *cobra.Command, _ []string) {
			if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
				os.Exit(rc)
			}
			var rows []struct {
				EnvelopeID string    `json:"envelope_id"`
				To         string    `json:"to"`
				From       string    `json:"from"`
				Preview    string    `json:"preview"`
				State      string    `json:"state"`
				Attempts   int       `json:"attempts"`
				LastError  string    `json:"last_error"`
				CreatedAt  time.Time `json:"created_at"`
			}
			if err := agent.DaemonGet(fmt.Sprintf("/v1/federation/outbox?limit=%d", p.Limit), &rows); err != nil {
				os.Exit(fail(os.Stderr, err))
			}
			if p.JSON {
				os.Exit(printJSON(os.Stdout, rows))
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "ENVELOPE\tFROM\tTO\tSTATE\tTRIES\tSENT\tPREVIEW\tNOTE")
			for _, r := range rows {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", r.EnvelopeID[:12], r.From, r.To, r.State, r.Attempts, ago(r.CreatedAt), r.Preview, r.LastError)
			}
			_ = tw.Flush()
		},
	}.ToCobra()
}

// --- remote spawn requests ---

type spawnRequestParams struct {
	Target  string `pos:"true" optional:"true" help:"<group>@<peer>: a remote group visible in the peer catalog (omit with --node)"`
	Node    string `long:"node" optional:"true" help:"Automatically select a node: auto or group:<pool>"`
	Group   string `long:"group" optional:"true" help:"Remote group for automatic placement; may be omitted only with one authorized group"`
	Require string `long:"require" optional:"true" help:"Required node metadata: comma-separated os=, arch=, harness=, label="`
	Prefer  string `long:"prefer" optional:"true" help:"Placement ranking: least-loaded (default) or most-free-ram"`
	JSON    bool   `long:"json" help:"Output the request and placement explanation as JSON"`
	Brief   string `long:"brief" help:"What the worker should do (sent to the remote operator and, if approved, to the worker)"`
	Name    string `long:"name" optional:"true" help:"Requested worker name"`
	Role    string `long:"role" optional:"true" help:"Requested worker role"`
}

func spawnRequestCmd() *cobra.Command {
	return boa.CmdT[spawnRequestParams]{
		Use:   "spawn-request",
		Short: "Ask a remote instance to spawn a worker into one of its groups (its operator decides)",
		Long: "Agent-callable (needs the groups.members.spawn scoped with peer=bob/group, or agent.spawn scoped with peer=bob) as well as usable by the operator. The remote group must be visible in its catalog. " +
			"The peer either spawns under its granted policy or asks its operator; the decision arrives in your inbox.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *spawnRequestParams, _ *cobra.Command, _ []string) {
			os.Exit(runSpawnRequest(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type spawnRequestRow struct {
	Require          string    `json:"require,omitempty"`
	PlacementVersion int       `json:"placement_version,omitempty"`
	ID               int64     `json:"id"`
	From             string    `json:"from"`
	Group            string    `json:"group"`
	Name             string    `json:"name"`
	Role             string    `json:"role"`
	Brief            string    `json:"brief"`
	Status           string    `json:"status"`
	ResultAgent      string    `json:"result_agent"`
	Reason           string    `json:"reason"`
	CreatedAt        time.Time `json:"created_at"`
}

type requestsParams struct {
	JSON bool `long:"json" help:"Output JSON"`
	All  bool `long:"all" help:"Include decided and expired requests"`
}

func requestsCmd() *cobra.Command {
	cmd := boa.CmdT[requestsParams]{
		Use:         "requests",
		Short:       "List spawn requests remote peers sent to your visible groups",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *requestsParams, _ *cobra.Command, _ []string) {
			if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
				os.Exit(rc)
			}
			var rows []spawnRequestRow
			if err := agent.DaemonGet("/v1/federation/spawn-requests", &rows); err != nil {
				os.Exit(fail(os.Stderr, err))
			}
			kept := rows[:0]
			for _, r := range rows {
				if p.All || r.Status == "pending" {
					kept = append(kept, r)
				}
			}
			if p.JSON {
				os.Exit(printJSON(os.Stdout, kept))
			}
			if len(kept) == 0 {
				fmt.Println("no pending spawn requests (--all shows decided ones)")
				return
			}
			for _, r := range kept {
				fmt.Printf("#%d  %s  from %s  into %s  %s\n", r.ID, r.Status, r.From, r.Group, ago(r.CreatedAt))
				if r.Name != "" || r.Role != "" {
					fmt.Printf("    name %q  role %q\n", r.Name, r.Role)
				}
				if r.PlacementVersion != 0 {
					fmt.Printf("    requires: %s (choose a compatible harness)\n", r.Require)
				}
				if r.ResultAgent != "" {
					fmt.Printf("    spawned %s\n", r.ResultAgent)
				}
				if r.Reason != "" {
					fmt.Printf("    reason: %s\n", r.Reason)
				}
				fmt.Printf("    %s\n\n", strings.ReplaceAll(strings.TrimSpace(r.Brief), "\n", "\n    "))
			}
		},
	}.ToCobra()
	cmd.AddCommand(approveCmd(), denyCmd(), abandonCmd())
	return cmd
}

type approveParams struct {
	ID      int64  `pos:"true" help:"Request id"`
	Name    string `long:"name" optional:"true" help:"Override the worker name"`
	Profile string `long:"profile" optional:"true" help:"Spawn profile to launch with"`
	Cwd     string `long:"cwd" optional:"true" help:"Working directory"`
	Harness string `long:"harness" optional:"true" help:"Harness to launch"`
	Model   string `long:"model" optional:"true" help:"Model to launch"`
}

func approveCmd() *cobra.Command {
	return boa.CmdT[approveParams]{
		Use:         "approve",
		Short:       "Approve a spawn request: spawn the worker into the requested group",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *approveParams, _ *cobra.Command, _ []string) {
			var out struct {
				Group   string `json:"group"`
				AgentID string `json:"agent_id"`
				Label   string `json:"label"`
			}
			body := map[string]any{"name": p.Name, "profile": p.Profile, "cwd": p.Cwd, "harness": p.Harness, "model": p.Model}
			if rc := post(os.Stderr, fmt.Sprintf("/v1/federation/spawn-requests/%d/approve", p.ID), body, &out); rc != 0 {
				os.Exit(rc)
			}
			fmt.Printf("launch started for %s (%s) into %s; the requester is told after enrollment\n", out.Label, out.AgentID, out.Group)
		},
	}.ToCobra()
}

type denyParams struct {
	ID     int64  `pos:"true" help:"Request id"`
	Reason string `long:"reason" optional:"true" help:"Reason sent back to the requester"`
}

func denyCmd() *cobra.Command {
	return boa.CmdT[denyParams]{
		Use:         "deny",
		Short:       "Deny a spawn request",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *denyParams, _ *cobra.Command, _ []string) {
			if rc := post(os.Stderr, fmt.Sprintf("/v1/federation/spawn-requests/%d/deny", p.ID), map[string]any{"reason": p.Reason}, nil); rc != 0 {
				os.Exit(rc)
			}
			fmt.Printf("denied request #%d\n", p.ID)
		},
	}.ToCobra()
}

type abandonParams struct {
	ID          int64 `pos:"true" help:"Launching request id"`
	Acknowledge bool  `long:"acknowledge-late-worker" optional:"true" help:"Acknowledge that a late worker may still appear after abandonment"`
}

func abandonCmd() *cobra.Command {
	return boa.CmdT[abandonParams]{Use: "abandon", Short: "Return an unconfirmed launch to pending; WARNING a late worker may still appear", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *abandonParams, _ *cobra.Command, _ []string) {
		fmt.Fprintln(os.Stderr, "WARNING: abandoning a launch permits another approval; a late worker may still appear")
		if rc := post(os.Stderr, fmt.Sprintf("/v1/federation/spawn-requests/%d/abandon", p.ID), map[string]any{"acknowledge_late_worker": p.Acknowledge}, nil); rc != 0 {
			os.Exit(rc)
		}
		fmt.Printf("request #%d returned to pending\n", p.ID)
	}}.ToCobra()
}
