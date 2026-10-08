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
	Profile    string        `long:"profile" help:"Node profile applied on the master"`
	Uses       int           `long:"uses" default:"1" help:"Maximum distinct node keys"`
	TTL        time.Duration `long:"ttl" default:"24h" help:"Token lifetime (maximum 720h)"`
	TrustLevel string        `long:"trust-level" default:"restricted" help:"Reciprocal master trust on the enrolling node: restricted or unrestricted"`
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

func enrollmentGet(path string) {
	if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
		os.Exit(rc)
	}
	var out any
	if e := agent.DaemonGet(path, &out); e != nil {
		os.Exit(fail(os.Stderr, e))
	}
	os.Exit(printJSON(os.Stdout, out))
}
func enrollTokenCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "enroll-token", Short: "Issue, inspect or revoke node enrollment tokens", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{
		boa.CmdT[enrollmentCreateParams]{Use: "create", Short: "Print a signed bearer once; store only its hash", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *enrollmentCreateParams, _ *cobra.Command, _ []string) {
			if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
				os.Exit(rc)
			}
			var out struct {
				Token       string                 `json:"token"`
				Claims      proto.EnrollmentClaims `json:"claims"`
				Fingerprint string                 `json:"master_fingerprint"`
			}
			if e := agent.DaemonRequest(http.MethodPost, "/v1/federation/enroll-tokens", map[string]any{"profile": p.Profile, "uses": p.Uses, "ttl_seconds": int64(p.TTL / time.Second), "trust_level": p.TrustLevel}, &out, agent.DaemonOpts{NoRetry: true}); e != nil {
				os.Exit(fail(os.Stderr, e))
			}
			fmt.Fprintf(os.Stderr, "Master %s\nProfile %s (%s), revision %d; node trusts master at %s; expires %s; %d distinct keys\n", out.Fingerprint, out.Claims.ProfileName, out.Claims.ProfileID, out.Claims.ProfileRevision, out.Claims.TrustLevel, out.Claims.ExpiresAt.Format(time.RFC3339), p.Uses)
			fmt.Fprintln(os.Stdout, out.Token)
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
	return boa.CmdT[enrollmentParams]{Use: "enroll", Short: "Consent to reciprocal trust using a pinned enrollment token", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *enrollmentParams, _ *cobra.Command, _ []string) { os.Exit(runEnroll(p)) }}.ToCobra()
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
func runEnroll(p *enrollmentParams) int {
	if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
		return rc
	}
	token, e := readEnrollmentBearer(p)
	if e != nil {
		return fail(os.Stderr, e)
	}
	in := map[string]any{"master": p.Master, "token": token}
	var preview struct {
		Claims  proto.EnrollmentClaims `json:"claims"`
		Preview string                 `json:"preview_token"`
		Master  string                 `json:"master_fingerprint"`
		Node    string                 `json:"node_fingerprint"`
		Consent string                 `json:"consent"`
	}
	if e = agent.DaemonRequest(http.MethodPost, "/v1/federation/enroll/preview", in, &preview, agent.DaemonOpts{NoRetry: true}); e != nil {
		return fail(os.Stderr, e)
	}
	fmt.Fprintf(os.Stderr, "Node fingerprint: %s\nMaster fingerprint: %s\nProfile: %s (%s), revision %d\nMaster trust on this node: %s\nExpires: %s\n%s\n", preview.Node, preview.Master, preview.Claims.ProfileName, preview.Claims.ProfileID, preview.Claims.ProfileRevision, preview.Claims.TrustLevel, preview.Claims.ExpiresAt.Format(time.RFC3339), preview.Consent)
	if preview.Claims.TrustLevel == "unrestricted" {
		fmt.Fprintln(os.Stderr, "UNRESTRICTED: master receives all peer permissions on all live groups, automatic spawn and local unscoped grants towards this peer.")
	} else {
		fmt.Fprintln(os.Stderr, "RESTRICTED: the master needs explicit grants to access local groups or spawn workers.")
	}
	if p.Preview {
		return 0
	}
	in["preview_token"] = preview.Preview
	var out json.RawMessage
	if e = agent.DaemonRequest(http.MethodPost, "/v1/federation/enroll", in, &out, agent.DaemonOpts{Timeout: 45 * time.Second, NoRetry: true}); e != nil {
		return fail(os.Stderr, e)
	}
	return printJSON(os.Stdout, out)
}
