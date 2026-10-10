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
	Board        string   `long:"board" optional:"true" help:"Board ID for hub moderation"`
	Frozen       *bool    `long:"frozen" help:"Freeze/unfreeze a board"`
	QuotaBytes   *int64   `long:"quota-bytes" optional:"true" help:"Board ciphertext quota"`
	MaxMembers   *int64   `long:"max-members" optional:"true" help:"Board member quota"`
	MaxVersions  *int64   `long:"max-versions" optional:"true" help:"Board version quota"`
	Action       string   `pos:"true" help:"list, add/remove (admins), admit/revoke, create/revoke, set/reset, recover/revoke-old"`
	Instance     string   `long:"instance" optional:"true" help:"Immutable instance ID"`
	Spaces       []string `long:"spaces" optional:"true" help:"Visible space names"`
	Capabilities []string `long:"capabilities" optional:"true" help:"Explicit hub admin capabilities"`
	TokenHash    string   `long:"token-hash" optional:"true" help:"Invite token hash, never the bearer token"`
	Space        string   `long:"space" optional:"true" help:"Invite space (default default)"`
	TTL          string   `long:"ttl" optional:"true" help:"Invite TTL (default 1h)"`
	Key          string   `long:"key" optional:"true" help:"Setting name"`
	Value        string   `long:"value" optional:"true" help:"Integer setting value"`
	Old          string   `long:"old" optional:"true" help:"Predecessor immutable ID"`
	New          string   `long:"new" optional:"true" help:"Replacement immutable ID"`
	Apply        bool     `long:"apply" help:"Apply identity change (default preview)"`
	Fingerprint  string   `long:"fingerprint" optional:"true" help:"Exact fingerprint confirmed out of band"`
	Cursor       string   `long:"cursor" optional:"true" help:"Opaque continuation cursor"`
	MaxEntries   int      `long:"max-entries" optional:"true" help:"Bound the list or log response"`
}

func hubCmd() *cobra.Command {
	sub := []*cobra.Command{
		hubRunCmd(), hubUpdateCmd(),
		boa.CmdT[hubClaimParams]{Use: "claim", Short: "Claim hub administration using the host's one-time token", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *hubClaimParams, _ *cobra.Command, _ []string) {
			os.Exit(runHubCall("POST", "claim", map[string]any{"token": p.Token}, os.Stdout, os.Stderr))
		}}.ToCobra(),
	}
	for _, resource := range []string{"status", "health"} {
		sub = append(sub, boa.CmdT[hubReadParams]{Use: resource, Short: "Read hub " + resource, ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *hubReadParams, _ *cobra.Command, _ []string) {
			os.Exit(runHubRead(resource, p, os.Stdout, os.Stderr))
		}}.ToCobra())
	}
	for _, resource := range []string{"admins", "admissions", "invites", "spaces", "settings", "identity", "logs", "audit", "boards"} {
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
	case "boards/list":
	case "boards/set", "boards/delete":
		if p.Board == "" {
			return invalid("requires --board")
		}
		tail += "/" + url.PathEscape(p.Board)
		method = "DELETE"
		if p.Action == "set" {
			method = "PATCH"
			if p.Frozen != nil {
				body["frozen"] = *p.Frozen
			}
			if p.QuotaBytes != nil {
				body["quota_bytes"] = *p.QuotaBytes
			}
			if p.MaxMembers != nil {
				body["max_members"] = *p.MaxMembers
			}
			if p.MaxVersions != nil {
				body["max_versions"] = *p.MaxVersions
			}
		}

	case "admins/list", "admissions/list", "invites/list", "spaces/list", "settings/get", "logs/tail", "audit/list":
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
