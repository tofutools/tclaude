package federationcmd

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

type hubClaimParams struct {
	Token string `pos:"true" help:"One-time token printed by the hub host"`
}
type hubResourceParams struct {
	Action       string   `pos:"true" help:"list, add/remove (admins), admit/revoke, create/revoke, set/reset, recover/revoke-old"`
	Instance     string   `long:"instance" help:"Immutable instance ID"`
	Spaces       []string `long:"spaces" help:"Visible space names"`
	Capabilities []string `long:"capabilities" help:"Explicit hub admin capabilities"`
	TokenHash    string   `long:"token-hash" help:"Invite token hash, never the bearer token"`
	Space        string   `long:"space" help:"Invite space (default default)"`
	TTL          string   `long:"ttl" help:"Invite TTL (default 1h)"`
	Key          string   `long:"key" help:"Setting name"`
	Value        string   `long:"value" help:"Integer setting value"`
	Old          string   `long:"old" help:"Predecessor immutable ID"`
	New          string   `long:"new" help:"Replacement immutable ID"`
	Apply        bool     `long:"apply" help:"Apply identity change (default preview)"`
	Fingerprint  string   `long:"fingerprint" help:"Exact fingerprint confirmed out of band"`
	Cursor       string   `long:"cursor" help:"Opaque continuation cursor"`
	MaxEntries   int      `long:"max-entries" help:"Bound the list or log response"`
}

func hubCmd() *cobra.Command {
	sub := []*cobra.Command{
		boa.CmdT[hubClaimParams]{Use: "claim", Short: "Claim hub administration using the host's one-time token", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *hubClaimParams, _ *cobra.Command, _ []string) {
			os.Exit(runHubCall("POST", "claim", map[string]any{"token": p.Token}, os.Stdout, os.Stderr))
		}}.ToCobra(),
	}
	for _, resource := range []string{"status", "health"} {
		sub = append(sub, boa.CmdT[struct{}]{Use: resource, Short: "Read hub " + resource, ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			os.Exit(runHubCall("GET", resource, nil, os.Stdout, os.Stderr))
		}}.ToCobra())
	}
	for _, resource := range []string{"admins", "admissions", "invites", "spaces", "settings", "identity", "logs"} {
		sub = append(sub, boa.CmdT[hubResourceParams]{Use: resource, Short: "Manage hub " + resource + " (operator only)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *hubResourceParams, _ *cobra.Command, _ []string) {
			os.Exit(runHubResource(resource, p, os.Stdout, os.Stderr))
		}}.ToCobra())
	}
	return boa.CmdT[struct{}]{Use: "hub", Short: "Administer the connected hub with signed operator RPCs", ParamEnrich: common.DefaultParamEnricher(), SubCmds: sub}.ToCobra()
}
func runHubResource(resource string, p *hubResourceParams, stdout, stderr io.Writer) int {
	method, tail := "GET", resource
	body := map[string]any{}
	invalid := func(message string) int { return fail(stderr, fmt.Errorf("%s: %s", resource, message)) }
	switch resource + "/" + p.Action {
	case "admins/list", "admissions/list", "invites/list", "spaces/list", "settings/get", "logs/tail":
	case "admins/add":
		if p.Instance == "" || len(p.Capabilities) == 0 {
			return invalid("add requires --instance and --capabilities")
		}
		method = "POST"
		body = map[string]any{"instance": p.Instance, "capabilities": p.Capabilities}
	case "admins/remove", "admissions/revoke":
		if p.Instance == "" {
			return invalid("requires --instance")
		}
		method = "DELETE"
		tail += "/" + url.PathEscape(p.Instance)
	case "admissions/admit", "spaces/set":
		if p.Instance == "" {
			return invalid("requires --instance")
		}
		method = "POST"
		if resource == "spaces" {
			method = "PUT"
		}
		body = map[string]any{"instance": p.Instance, "spaces": p.Spaces}
	case "invites/create":
		method = "POST"
		body["space"] = p.Space
		if p.TTL != "" {
			duration, err := time.ParseDuration(p.TTL)
			if err != nil || duration < time.Minute || duration > 7*24*time.Hour || duration%time.Second != 0 {
				return invalid("--ttl must be whole seconds between 1m and 168h")
			}
			body["ttl_seconds"] = int64(duration / time.Second)
		}
	case "invites/revoke":
		if p.TokenHash == "" {
			return invalid("requires --token-hash")
		}
		method = "DELETE"
		tail += "/" + url.PathEscape(p.TokenHash)
	case "settings/set", "settings/reset":
		if p.Key == "" {
			return invalid("requires --key")
		}
		var value *int64
		if p.Action == "set" {
			n, err := strconv.ParseInt(p.Value, 10, 64)
			if err != nil {
				return invalid("requires integer --value")
			}
			value = &n
		}
		method = "PATCH"
		body["overrides"] = map[string]*int64{p.Key: value}
	case "identity/recover":
		if p.Old == "" || p.New == "" {
			return invalid("recover requires --old and --new")
		}
		method = "POST"
		tail += "/recover"
		body = map[string]any{"old": p.Old, "new": p.New, "apply": p.Apply, "fingerprint": p.Fingerprint}
	case "identity/revoke-old":
		if p.Instance == "" {
			return invalid("revoke-old requires --instance")
		}
		method = "POST"
		tail += "/revoke-old"
		body = map[string]any{"instance": p.Instance, "apply": p.Apply, "fingerprint": p.Fingerprint}
	default:
		return invalid("unknown action " + p.Action)
	}
	if method == "GET" {
		q := url.Values{}
		if p.Cursor != "" {
			q.Set("cursor", p.Cursor)
		}
		if p.MaxEntries != 0 {
			q.Set("max_entries", strconv.Itoa(p.MaxEntries))
		}
		if len(q) > 0 {
			tail += "?" + q.Encode()
		}
		return runHubCall(method, tail, nil, stdout, stderr)
	}
	return runHubCall(method, tail, body, stdout, stderr)
}
func runHubCall(method, tail string, body any, stdout, stderr io.Writer) int {
	if strings.Contains(tail, "..") {
		return fail(stderr, fmt.Errorf("invalid hub route"))
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	var out any
	if err := agent.DaemonRequest(method, "/v1/federation/hub/"+tail, body, &out, agent.DaemonOpts{NoRetry: true, Timeout: 35 * time.Second}); err != nil {
		return fail(stderr, err)
	}
	return printJSON(stdout, out)
}
