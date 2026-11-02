package agent

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
			externalStewardLsCmd(), externalStewardAddCmd(), externalStewardRenewCmd(), externalStewardRevokeCmd(), externalStewardSpawnCmd(),
		},
	}.ToCobra()
}

type externalStewardCredentialRow struct {
	TokenID   string    `json:"token_id"`
	ExpiresAt time.Time `json:"expires_at"`
	Live      bool      `json:"live"`
}

type externalStewardChildRow struct {
	AgentID string `json:"agent_id"`
	Title   string `json:"title"`
	Online  bool   `json:"online"`
}

type externalStewardSpawnPin struct {
	Groups         []string `json:"groups"`
	SpawnProfile   string   `json:"spawn_profile"`
	SandboxProfile string   `json:"sandbox_profile"`
}

type externalStewardRow struct {
	AgentID     string                         `json:"agent_id"`
	Name        string                         `json:"name"`
	Retired     bool                           `json:"retired"`
	Credentials []externalStewardCredentialRow `json:"credentials"`
	Spawn       *externalStewardSpawnPin       `json:"spawn"`
	Children    []externalStewardChildRow      `json:"children"`
}

func printExternalStewardChildren(w io.Writer, children []externalStewardChildRow) {
	for _, c := range children {
		status := "stopped"
		if c.Online {
			status = "running"
		}
		fmt.Fprintf(w, "  worker %s  %-7s  %s\n", c.AgentID, status, c.Title)
	}
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
		if s.Spawn != nil {
			fmt.Fprintf(stdout, "  starts workers in %s, spawn profile %s, sandbox profile %s\n",
				strings.Join(s.Spawn.Groups, ", "), s.Spawn.SpawnProfile, s.Spawn.SandboxProfile)
		}
		printExternalStewardChildren(stdout, s.Children)
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

	StopChildren bool `long:"stop-children" help:"Also stop the workers it started; without this they keep running"`
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
		Revoked  int64                     `json:"revoked"`
		Children []externalStewardChildRow `json:"children"`
		Stopped  []struct {
			AgentID string `json:"agent_id"`
			Action  string `json:"action"`
			Detail  string `json:"detail"`
		} `json:"stopped"`
	}
	body := map[string]any{"token_id": p.TokenID, "stop_children": p.StopChildren}
	if err := DaemonPost("/v1/external-stewards/"+url.PathEscape(p.Name)+"/revoke", body, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Revoked %d credential(s).\n", resp.Revoked)
	if len(resp.Children) > 0 {
		if p.StopChildren {
			fmt.Fprintln(stdout, "Stopping its workers:")
		} else {
			fmt.Fprintln(stdout, "Its workers keep running. Use --stop-children to stop them too:")
		}
		printExternalStewardChildren(stdout, resp.Children)
	}
	for _, stop := range resp.Stopped {
		if stop.Action == "error" {
			fmt.Fprintf(stderr, "Could not stop %s: %s\n", stop.AgentID, stop.Detail)
		}
	}
	return rcOK
}

type externalStewardSpawnParams struct {
	Name           string   `pos:"true" help:"The external steward"`
	Groups         []string `long:"group" optional:"true" help:"A group it can start workers in; repeat for more"`
	SpawnProfile   string   `long:"spawn-profile" optional:"true" help:"The spawn profile its workers use"`
	SandboxProfile string   `long:"sandbox-profile" optional:"true" help:"The sandbox profile its workers run under"`
	Off            bool     `long:"off" help:"Stop it starting workers"`
}

func externalStewardSpawnCmd() *cobra.Command {
	return boa.CmdT[externalStewardSpawnParams]{
		Use:   "spawn",
		Short: "Choose where an external steward can start workers",
		Long: "Name its groups, one spawn profile and one sandbox profile, or pass --off. The spawn profile has to set an " +
			"approval mode where you answer every action, so its workers stop at the first action that isn't read-only " +
			"until you do.\n\n" + externalStewardNeeds,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *externalStewardSpawnParams, _ *cobra.Command, _ []string) {
			os.Exit(runExternalStewardSpawn(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runExternalStewardSpawn(p *externalStewardSpawnParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	body := map[string]any{
		"groups": p.Groups, "spawn_profile": p.SpawnProfile, "sandbox_profile": p.SandboxProfile, "off": p.Off,
	}
	if err := DaemonPost("/v1/external-stewards/"+url.PathEscape(p.Name)+"/spawn", body, nil); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.Off {
		fmt.Fprintf(stdout, "%s can't start workers now. Any it started keep running.\n", p.Name)
		return rcOK
	}
	fmt.Fprintf(stdout, "%s can start workers in %s.\n", p.Name, strings.Join(p.Groups, ", "))
	return rcOK
}
