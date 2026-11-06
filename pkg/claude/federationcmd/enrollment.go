package federationcmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type enrollmentCreateParams struct {
	Profile      string        `long:"profile" help:"Node profile applied on the master"`
	Uses         int           `long:"uses" default:"1" help:"Maximum distinct node keys"`
	TTL          time.Duration `long:"ttl" default:"24h" help:"Token lifetime (maximum 720h)"`
	RequestTrust string        `long:"request-trust" default:"restricted" help:"Trust in this master to REQUEST of the enrolling node: restricted or unrestricted. Only a request: plain enroll gives nothing, and only become-node with --yes-unrestricted gives unrestricted"`
	TrustLevel   string        `long:"trust-level" optional:"true" help:"Deprecated name of --request-trust"`
}
type enrollmentIDParams struct {
	ID string `pos:"true" help:"Public token ID"`
}
type enrollmentParams struct {
	Master     string `pos:"true" help:"Signed master instance ID or matching hub name"`
	Token      string `long:"token" optional:"true" help:"Bearer enrollment token (prefer --token-file or --token-stdin)"`
	TokenFile  string `long:"token-file" optional:"true" help:"Read bearer from a private file"`
	TokenStdin bool   `long:"token-stdin" help:"Read bearer from stdin"`
	Preview    bool   `long:"preview" help:"Display consent terms without enrolling"`
}

// becomeNodeParams are enroll's, plus what this node gives the master.
type becomeNodeParams struct {
	Master             string   `pos:"true" help:"Signed master instance ID or matching hub name"`
	Token              string   `long:"token" optional:"true" help:"Bearer enrollment token (prefer --token-file or --token-stdin)"`
	TokenFile          string   `long:"token-file" optional:"true" help:"Read bearer from a private file"`
	TokenStdin         bool     `long:"token-stdin" help:"Read bearer from stdin"`
	Preview            bool     `long:"preview" help:"Display the terms and the warning without enrolling"`
	GivePermissions    []string `long:"give-permissions" optional:"true" help:"Exact peer permissions to give the master on this machine: slug or slug@group=<local group>, comma-separated or repeated"`
	YesUnrestricted    bool     `long:"yes-unrestricted" help:"Give the master unrestricted trust on this machine instead of a permission list"`
	ConfirmFingerprint string   `long:"confirm-fingerprint" optional:"true" help:"The master's fingerprint, compared with its operator over another channel (required)"`
}

func enrollmentGet(path string) {
	if rc := agent.RequireDaemonOrExit(cliStderr); rc != 0 {
		os.Exit(rc)
	}
	var out any
	if e := agent.DaemonGet(path, &out); e != nil {
		os.Exit(fail(cliStderr, e))
	}
	os.Exit(printJSON(cliStdout, out))
}
func enrollTokenCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "enroll-token", Short: "Issue, inspect or revoke node enrollment tokens", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{
		boa.CmdT[enrollmentCreateParams]{Use: "create", Short: "Print a signed bearer once; store only its hash", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *enrollmentCreateParams, _ *cobra.Command, _ []string) {
			// Sub-second values would round to zero, which means the API's
			// default lifetime. Refuse rather than silently issuing 24 hours.
			if p.TTL < time.Second || p.TTL > 30*24*time.Hour || p.Uses < 1 || p.Uses > 10000 {
				os.Exit(fail(cliStderr, fmt.Errorf("TTL must be 1s-720h and uses must be 1-10000")))
			}
			requested := p.RequestTrust
			if p.TrustLevel != "" {
				requested = p.TrustLevel
			}
			if rc := agent.RequireDaemonOrExit(cliStderr); rc != 0 {
				os.Exit(rc)
			}
			var out struct {
				Token       string                 `json:"token"`
				Claims      proto.EnrollmentClaims `json:"claims"`
				Fingerprint string                 `json:"master_fingerprint"`
			}
			if e := agent.DaemonRequest(http.MethodPost, "/v1/federation/enroll-tokens", map[string]any{"profile": p.Profile, "uses": p.Uses, "ttl_seconds": int64(p.TTL / time.Second), "trust_level": requested}, &out, agent.DaemonOpts{NoRetry: true}); e != nil {
				os.Exit(fail(cliStderr, e))
			}
			fmt.Fprintf(cliStderr, "Master %s\nProfile %s (%s), revision %d; requests %s trust of the node (a request only); expires %s; %d distinct keys\n", out.Fingerprint, out.Claims.ProfileName, out.Claims.ProfileID, out.Claims.ProfileRevision, out.Claims.TrustLevel, out.Claims.ExpiresAt.Format(time.RFC3339), p.Uses)
			fmt.Fprintln(cliStdout, out.Token)
		}}.ToCobra(),
		boa.CmdT[struct{}]{Use: "ls", Short: "List public metadata and use counts", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) { enrollmentGet("/v1/federation/enroll-tokens") }}.ToCobra(),
		boa.CmdT[enrollmentIDParams]{Use: "revoke", Short: "Prevent new bindings without untrusting enrolled nodes", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *enrollmentIDParams, _ *cobra.Command, _ []string) {
			nodeGroupMutation(http.MethodPost, "/v1/federation/enroll-tokens/"+url.PathEscape(p.ID)+"/revoke", nil)
		}}.ToCobra(),
	}}.ToCobra()
}
func enrollmentsCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "enrollments", Short: "List node keys, token IDs and retired bindings", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) { enrollmentGet("/v1/federation/enrollments") }}.ToCobra()
}
func enrollCmd() *cobra.Command {
	return boa.CmdT[enrollmentParams]{Use: "enroll", Short: "Accept what a master offers this machine by token; gives the master nothing (see become-node)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *enrollmentParams, _ *cobra.Command, _ []string) { os.Exit(runEnroll(p, nil)) }}.ToCobra()
}
func becomeNodeCmd() *cobra.Command {
	return boa.CmdT[becomeNodeParams]{Use: "become-node", Short: "Enroll as a worker node of a master and GIVE it the named permissions on this machine", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *becomeNodeParams, _ *cobra.Command, _ []string) {
		os.Exit(runEnroll(&enrollmentParams{Master: p.Master, Token: p.Token, TokenFile: p.TokenFile, TokenStdin: p.TokenStdin, Preview: p.Preview}, p))
	}}.ToCobra()
}

// becomeNodeGives parses --give-permissions: slug or slug@scope, comma-separated
// or repeated.
func becomeNodeGives(raw []string) []map[string]string {
	out := []map[string]string{}
	for _, item := range raw {
		for _, part := range strings.Split(item, ",") {
			if part = strings.TrimSpace(part); part == "" {
				continue
			}
			slug, scope, _ := strings.Cut(part, "@")
			out = append(out, map[string]string{"slug": slug, "scope": scope})
		}
	}
	return out
}
func readEnrollmentBearer(p *enrollmentParams) (string, error) {
	n := 0
	if p.Token != "" {
		n++
	}
	if p.TokenFile != "" {
		n++
	}
	if p.TokenStdin {
		n++
	}
	if n != 1 {
		return "", fmt.Errorf("provide exactly one of --token, --token-file, --token-stdin")
	}
	if p.Token != "" {
		if len(p.Token) > proto.MaxEnrollmentBytes {
			return "", proto.ErrEnrollmentToken
		}
		return strings.TrimSpace(p.Token), nil
	}
	var r io.Reader = os.Stdin
	if p.TokenFile != "" {
		f, e := os.Open(p.TokenFile)
		if e != nil {
			return "", e
		}
		defer f.Close()
		r = f
	}
	raw, e := io.ReadAll(io.LimitReader(r, proto.MaxEnrollmentBytes+1))
	if e != nil {
		return "", e
	}
	if len(raw) > proto.MaxEnrollmentBytes {
		return "", proto.ErrEnrollmentToken
	}
	return strings.TrimSpace(string(raw)), nil
}
func runEnroll(p *enrollmentParams, become *becomeNodeParams) int {
	if rc := agent.RequireDaemonOrExit(cliStderr); rc != 0 {
		return rc
	}
	token, e := readEnrollmentBearer(p)
	if e != nil {
		return fail(cliStderr, e)
	}
	in := map[string]any{"master": p.Master, "token": token}
	var preview struct {
		Claims    proto.EnrollmentClaims `json:"claims"`
		Preview   string                 `json:"preview_token"`
		Master    string                 `json:"master_fingerprint"`
		Node      string                 `json:"node_fingerprint"`
		Requested string                 `json:"requested_trust"`
		Consent   string                 `json:"consent"`
	}
	if e = agent.DaemonRequest(http.MethodPost, "/v1/federation/enroll/preview", in, &preview, agent.DaemonOpts{NoRetry: true}); e != nil {
		return fail(cliStderr, e)
	}
	fmt.Fprintf(cliStderr, "Node fingerprint: %s\nMaster fingerprint: %s\nYou get: profile %s (%s), revision %d: your authority on the master\nThe master asks for: %s trust on this machine (a request; nothing applies unless you give it)\nExpires: %s\n", preview.Node, preview.Master, preview.Claims.ProfileName, preview.Claims.ProfileID, preview.Claims.ProfileRevision, preview.Requested, preview.Claims.ExpiresAt.Format(time.RFC3339))
	path := "/v1/federation/enroll"
	if become == nil {
		fmt.Fprintf(cliStderr, "It gets here: restricted trust, no grants.\n%s\n", preview.Consent)
	} else {
		path = "/v1/federation/become-node"
		if rc := becomeNodeWarn(in, p, become, preview.Master); rc != 0 {
			return rc
		}
	}
	if p.Preview {
		return 0
	}
	in["preview_token"] = preview.Preview
	var out json.RawMessage
	if e = agent.DaemonRequest(http.MethodPost, path, in, &out, agent.DaemonOpts{Timeout: 45 * time.Second, NoRetry: true}); e != nil {
		return fail(cliStderr, e)
	}
	return printJSON(cliStdout, out)
}

// becomeNodeWarn has the daemon check what become-node gives, prints the
// warning naming exactly that, and leaves in ready to send.
func becomeNodeWarn(in map[string]any, p *enrollmentParams, become *becomeNodeParams, masterFingerprint string) int {
	in["give_permissions"] = becomeNodeGives(become.GivePermissions)
	in["unrestricted"] = become.YesUnrestricted
	// The check passes the fingerprint shown, so the warning comes before the
	// operator types theirs, and a preview shows it too.
	in["confirm_fingerprint"], in["check_only"] = masterFingerprint, true
	var check struct {
		Trust string `json:"trust"`
		Gives []struct {
			Slug      string `json:"slug"`
			Scope     string `json:"scope"`
			Sensitive bool   `json:"sensitive"`
		} `json:"gives"`
	}
	e := agent.DaemonRequest(http.MethodPost, "/v1/federation/become-node", in, &check, agent.DaemonOpts{NoRetry: true})
	delete(in, "check_only")
	if e != nil {
		return fail(cliStderr, e)
	}
	bar := strings.Repeat("!", 72)
	fmt.Fprintf(cliStderr, "\n%s\n!!  WARNING: YOU ARE HANDING THIS MACHINE TO %s\n", bar, p.Master)
	if check.Trust == "unrestricted" {
		fmt.Fprintln(cliStderr, "!!  UNRESTRICTED: EVERY peer permission on EVERY live group, automatic")
		fmt.Fprintln(cliStderr, "!!  worker spawning on this machine, and local unscoped grants towards it.")
	} else {
		fmt.Fprintln(cliStderr, "!!  It will hold, on this machine, exactly:")
		for _, g := range check.Gives {
			where, mark := "every group, including future ones", ""
			if g.Scope != "" {
				where = g.Scope
			}
			if g.Sensitive {
				mark = "  [SENSITIVE]"
			}
			fmt.Fprintf(cliStderr, "!!    %s (%s)%s\n", g.Slug, where, mark)
		}
	}
	fmt.Fprintf(cliStderr, "!!  Take it back with tclaude federation revoke or untrust.\n%s\n\n", bar)
	if !p.Preview && become.ConfirmFingerprint == "" {
		return fail(cliStderr, fmt.Errorf("become-node needs --confirm-fingerprint <master fingerprint>: get it from the master's operator over another channel and compare it with the one shown above"))
	}
	in["confirm_fingerprint"] = become.ConfirmFingerprint
	return 0
}
