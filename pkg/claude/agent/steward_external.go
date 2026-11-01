package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent steward external` — the operator names an assistant running
// outside tclaude and makes its credential. As with the rest of this command
// group the daemon refuses an agent, with no permission or sudo path. Issuing
// calls never retry: the response holds the credential, so the daemon keeps no
// copy to replay, and a retry would mint another one.

const externalStewardNeeds = "Only the human operator can run this; agents are refused. Needs Collab and steward.external_enabled in the config."

func stewardExternalCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "external",
		Short:       "Manage assistants outside tclaude that act as a steward (human only)",
		Long:        "An external steward is an assistant you run yourself, proved by a credential you make here. It reads and messages like the Steward, and it can never approve a permission request or hold sudo.\n\n" + externalStewardNeeds,
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			externalStewardLsCmd(), externalStewardAddCmd(), externalStewardRenewCmd(), externalStewardRevokeCmd(),
		},
	}.ToCobra()
}

type externalStewardCredentialRow struct {
	TokenID   string    `json:"token_id"`
	ExpiresAt time.Time `json:"expires_at"`
	Live      bool      `json:"live"`
}

type externalStewardRow struct {
	AgentID     string                         `json:"agent_id"`
	Name        string                         `json:"name"`
	Retired     bool                           `json:"retired"`
	Credentials []externalStewardCredentialRow `json:"credentials"`
}

type externalStewardIssuedResp struct {
	Name       string    `json:"name"`
	TokenID    string    `json:"token_id"`
	ExpiresAt  time.Time `json:"expires_at"`
	Credential string    `json:"credential"`
	Grants     []string  `json:"grants"`
}

type externalStewardLsParams struct {
	JSON bool `long:"json" help:"Output JSON"`
}

func externalStewardLsCmd() *cobra.Command {
	return boa.CmdT[externalStewardLsParams]{
		Use:         "ls",
		Short:       "List external stewards and their credentials",
		Long:        externalStewardNeeds,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *externalStewardLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runExternalStewardLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runExternalStewardLs(p *externalStewardLsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var raw json.RawMessage
	if err := DaemonGet("/v1/external-stewards", &raw); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		_, _ = stdout.Write(raw)
		fmt.Fprintln(stdout)
		return rcOK
	}
	var resp struct {
		Stewards []externalStewardRow `json:"external_stewards"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcIOFailure
	}
	if len(resp.Stewards) == 0 {
		fmt.Fprintln(stdout, "No external stewards. Use `tclaude agent steward external add <name>`.")
		return rcOK
	}
	for _, s := range resp.Stewards {
		state := ""
		if s.Retired {
			state = " (retired)"
		}
		fmt.Fprintf(stdout, "%s  %s%s\n", s.AgentID, s.Name, state)
		for _, c := range s.Credentials {
			status := "ended"
			if c.Live {
				status = "live"
			}
			fmt.Fprintf(stdout, "  %s  %-5s  expires %s\n", c.TokenID, status, c.ExpiresAt.Format(time.DateOnly))
		}
	}
	return rcOK
}

func printExternalStewardIssued(w io.Writer, resp *externalStewardIssuedResp) {
	fmt.Fprintf(w, "Credential for %s, valid until %s. It is shown only this once:\n\n%s\n\n",
		resp.Name, resp.ExpiresAt.Format(time.DateOnly), resp.Credential)
	fmt.Fprintln(w, "Keep it in a file only you can read (chmod 600), or pass it in the environment. Never on a command line.")
	if len(resp.Grants) > 0 {
		fmt.Fprintf(w, "It starts with: %v\n", resp.Grants)
	}
}

type externalStewardAddParams struct {
	Name string `pos:"true" help:"Lowercase name for the assistant, shown on the roster"`
	Days int    `long:"days" optional:"true" default:"30" help:"Days until the credential expires, at most 365"`
}

func externalStewardAddCmd() *cobra.Command {
	return boa.CmdT[externalStewardAddParams]{
		Use:         "add",
		Short:       "Create an external steward and print its credential once",
		Long:        externalStewardNeeds,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *externalStewardAddParams, _ *cobra.Command, _ []string) {
			os.Exit(runExternalStewardAdd(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runExternalStewardAdd(p *externalStewardAddParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp externalStewardIssuedResp
	body := map[string]any{"name": p.Name, "expires_days": p.Days}
	if err := DaemonRequest(http.MethodPost, "/v1/external-stewards", body, &resp, DaemonOpts{NoRetry: true}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	printExternalStewardIssued(stdout, &resp)
	return rcOK
}

type externalStewardRenewParams struct {
	Name      string `pos:"true" help:"The external steward"`
	Days      int    `long:"days" optional:"true" default:"30" help:"Days until the new credential expires, at most 365"`
	RevokeOld bool   `long:"revoke-old" help:"End its other credentials now"`
}

func externalStewardRenewCmd() *cobra.Command {
	return boa.CmdT[externalStewardRenewParams]{
		Use:         "renew",
		Short:       "Make a new credential and print it once",
		Long:        "The old credentials keep working until they expire, unless --revoke-old ends them now.\n\n" + externalStewardNeeds,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *externalStewardRenewParams, _ *cobra.Command, _ []string) {
			os.Exit(runExternalStewardRenew(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runExternalStewardRenew(p *externalStewardRenewParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp externalStewardIssuedResp
	body := map[string]any{"expires_days": p.Days, "revoke_existing": p.RevokeOld}
	if err := DaemonRequest(http.MethodPost, "/v1/external-stewards/"+url.PathEscape(p.Name)+"/renew", body, &resp, DaemonOpts{NoRetry: true}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	printExternalStewardIssued(stdout, &resp)
	return rcOK
}

type externalStewardRevokeParams struct {
	Name    string `pos:"true" help:"The external steward"`
	TokenID string `long:"token" optional:"true" help:"End only this credential; without it, all of them"`
}

func externalStewardRevokeCmd() *cobra.Command {
	return boa.CmdT[externalStewardRevokeParams]{
		Use:         "revoke",
		Short:       "End an external steward's credentials",
		Long:        "It takes effect on the next request, and works with external stewards turned off.\n\n" + externalStewardNeeds,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *externalStewardRevokeParams, _ *cobra.Command, _ []string) {
			os.Exit(runExternalStewardRevoke(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runExternalStewardRevoke(p *externalStewardRevokeParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		Revoked int64 `json:"revoked"`
	}
	body := map[string]any{"token_id": p.TokenID}
	if err := DaemonPost("/v1/external-stewards/"+url.PathEscape(p.Name)+"/revoke", body, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Revoked %d credential(s).\n", resp.Revoked)
	return rcOK
}
