package federationcmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
)

type nodeProfileFileParams struct {
	Name string `pos:"true" help:"Local profile name"`
	File string `long:"file" optional:"true" help:"Profile JSON with definition and, for update, revision"`
}
type nodeProfileNameParams struct {
	Name string `pos:"true" help:"Local profile name"`
}
type nodeProfileApplyParams struct {
	Name  string `pos:"true" help:"Local profile name"`
	Peer  string `pos:"true" optional:"true" help:"Trusted peer; omit with --all"`
	All   bool   `long:"all" help:"Re-apply to every peer assigned to this profile"`
	Apply bool   `long:"apply" help:"Commit the displayed preview"`
	Yes   bool   `long:"yes" help:"Confirm unrestricted access without prompting"`
}
type nodeProfileOfferParams struct {
	Name string `pos:"true" help:"Applied profile name"`
	Peer string `pos:"true" help:"Trusted peer"`
}

func nodeProfileWrite(method string, p *nodeProfileFileParams) {
	if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
		os.Exit(rc)
	}
	var body db.FederationNodeProfile
	if p.File != "" {
		raw, e := os.ReadFile(p.File)
		if e != nil {
			os.Exit(fail(os.Stderr, e))
		}
		if e = json.Unmarshal(raw, &body); e != nil {
			os.Exit(fail(os.Stderr, e))
		}
	}
	body.Name = p.Name
	path := "/v1/federation/profiles"
	if method == http.MethodPut {
		path += "/" + url.PathEscape(p.Name)
	}
	var out any
	if e := agent.DaemonRequest(method, path, body, &out, agent.DaemonOpts{}); e != nil {
		os.Exit(fail(os.Stderr, e))
	}
	os.Exit(printJSON(os.Stdout, out))
}
func confirmNodeProfileUnrestricted(fingerprint string, yes bool) error {
	fmt.Fprintf(os.Stderr, "Fingerprint %s: unrestricted grants all peer permissions on all live groups, automatic spawn, and local unscoped grants towards this peer. Intended for your own machines.\n", fingerprint)
	if yes {
		return nil
	}
	fmt.Fprint(os.Stderr, "Type yes to confirm: ")
	var answer string
	if _, e := fmt.Fscanln(os.Stdin, &answer); e != nil || answer != "yes" {
		return fmt.Errorf("not confirmed")
	}
	return nil
}
func nodeProfilesCmd() *cobra.Command {
	return boa.CmdT[struct{}]{Use: "profile", Aliases: []string{"profiles"}, Short: "Manage local node profiles and worker permission defaults", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{
		boa.CmdT[nodeProfileFileParams]{Use: "create", Short: "Create a node profile", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *nodeProfileFileParams, _ *cobra.Command, _ []string) { nodeProfileWrite(http.MethodPost, p) }}.ToCobra(),
		boa.CmdT[nodeProfileFileParams]{Use: "update", Short: "Save an edited profile with its current revision", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *nodeProfileFileParams, _ *cobra.Command, _ []string) { nodeProfileWrite(http.MethodPut, p) }}.ToCobra(),
		boa.CmdT[nodeProfileNameParams]{Use: "show", Short: "Show profile JSON and assigned peers", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *nodeProfileNameParams, _ *cobra.Command, _ []string) {
			if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
				os.Exit(rc)
			}
			var out struct {
				Profile db.FederationNodeProfile `json:"profile"`
			}
			if e := agent.DaemonGet("/v1/federation/profiles/"+url.PathEscape(p.Name), &out); e != nil {
				os.Exit(fail(os.Stderr, e))
			}
			os.Exit(printJSON(os.Stdout, out.Profile))
		}}.ToCobra(),
		boa.CmdT[struct{}]{Use: "ls", Short: "List profiles and the trust-time default", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
				os.Exit(rc)
			}
			var out any
			if e := agent.DaemonGet("/v1/federation/profiles", &out); e != nil {
				os.Exit(fail(os.Stderr, e))
			}
			os.Exit(printJSON(os.Stdout, out))
		}}.ToCobra(),
		boa.CmdT[nodeProfileNameParams]{Use: "rm", Short: "Delete an unused profile", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *nodeProfileNameParams, _ *cobra.Command, _ []string) {
			nodeGroupMutation(http.MethodDelete, "/v1/federation/profiles/"+url.PathEscape(p.Name), nil)
		}}.ToCobra(),
		boa.CmdT[nodeProfileNameParams]{Use: "default", Short: "Set the default for new trust, or use 'none' to clear", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *nodeProfileNameParams, _ *cobra.Command, _ []string) {
			name := p.Name
			if name == "none" {
				name = ""
			}
			nodeGroupMutation(http.MethodPut, "/v1/federation/default-peer-profile", map[string]any{"profile": name})
		}}.ToCobra(),
		boa.CmdT[nodeProfileApplyParams]{Use: "apply", Short: "Preview local policy changes; --apply commits", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *nodeProfileApplyParams, _ *cobra.Command, _ []string) { os.Exit(runNodeProfileApply(p)) }}.ToCobra(),
		boa.CmdT[nodeProfileOfferParams]{Use: "offer", Short: "Offer applied config/labels separately; receiver chooses whether to import", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *nodeProfileOfferParams, _ *cobra.Command, _ []string) {
			nodeGroupMutation(http.MethodPost, "/v1/federation/profiles/"+url.PathEscape(p.Name)+"/offer", map[string]any{"peer": p.Peer})
		}}.ToCobra(),
	}}.ToCobra()
}
func runNodeProfileApply(p *nodeProfileApplyParams) int {
	if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
		return rc
	}
	if p.All == (p.Peer != "") {
		return fail(os.Stderr, fmt.Errorf("specify a peer or --all"))
	}
	peers := []string{p.Peer}
	if p.All {
		var out struct {
			Peers []string `json:"applied_peers"`
		}
		if e := agent.DaemonGet("/v1/federation/profiles/"+url.PathEscape(p.Name), &out); e != nil {
			return fail(os.Stderr, e)
		}
		peers = out.Peers
	}
	path := "/v1/federation/profiles/" + url.PathEscape(p.Name) + "/apply"
	for _, peer := range peers {
		in := map[string]any{"peer": peer}
		var plan db.FederationNodeProfilePlan
		if e := agent.DaemonRequest(http.MethodPost, path, in, &plan, agent.DaemonOpts{}); e != nil {
			return fail(os.Stderr, e)
		}
		if rc := printJSON(os.Stdout, plan); rc != 0 {
			return rc
		}
		if !p.Apply {
			continue
		}
		if len(plan.Conflicts) > 0 {
			return fail(os.Stderr, fmt.Errorf("manual-edit conflicts; resolve them before applying"))
		}
		if plan.Profile.Definition.TrustLevel == db.FederationTrustUnrestricted {
			st, rc := loadStatus(os.Stderr)
			if rc != 0 {
				return rc
			}
			fingerprint := ""
			for _, candidate := range st.Peers {
				if candidate.InstanceID == plan.Peer {
					fingerprint = candidate.Fingerprint
				}
			}
			if fingerprint == "" {
				return fail(os.Stderr, fmt.Errorf("peer fingerprint unavailable"))
			}
			if e := confirmNodeProfileUnrestricted(fingerprint, p.Yes); e != nil {
				return fail(os.Stderr, e)
			}
			in["confirm_fingerprint"] = fingerprint
		}
		in["apply"] = true
		in["preview_token"] = plan.Token
		var applied any
		if e := agent.DaemonRequest(http.MethodPost, path, in, &applied, agent.DaemonOpts{}); e != nil {
			return fail(os.Stderr, e)
		}
		if rc := printJSON(os.Stdout, applied); rc != 0 {
			return rc
		}
	}
	return 0
}
