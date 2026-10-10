package federationcmd

import (
	"encoding/json"
	"fmt"
	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
	"io"
	"net/url"
	"os"
	"time"
)

type accessParams struct {
	Action     string `pos:"true" help:"request, status, list, approve, deny, or extend"`
	Node       string `long:"node" optional:"true" help:"Receiving peer ID or local label (request/status)"`
	ID         string `long:"id" optional:"true" help:"Opaque access request ID"`
	Permission string `long:"permission" optional:"true" help:"Requested peer permission from the dispatcher"`
	GroupID    int64  `long:"group-id" optional:"true" help:"Stable receiving group ID; 0 requests an unscoped grant"`
	Reason     string `long:"reason" optional:"true" help:"Explain the access request to the receiving operator"`
	TTL        string `long:"ttl" optional:"true" help:"Grant lifetime, e.g. 1h or 0 for permanent; default 1h on requests"`
	Seconds    int    `long:"seconds" optional:"true" help:"Extend the decision deadline by 1-300 seconds"`
}

func accessCmd() *cobra.Command {
	return boa.CmdT[accessParams]{Use: "access", Short: "Request or decide operator-to-operator peer access (operator only)", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *accessParams, _ *cobra.Command, _ []string) { os.Exit(runAccess(p, os.Stdout, os.Stderr)) }}.ToCobra()
}
func runAccess(p *accessParams, stdout, stderr io.Writer) int {
	method := "GET"
	path := "/v1/federation/access-requests"
	var body any
	var ttl *int
	if p.TTL != "" {
		d, err := time.ParseDuration(p.TTL)
		if err != nil || d < 0 || d > 30*24*time.Hour || d%time.Second != 0 {
			return fail(stderr, fmt.Errorf("--ttl must be whole seconds between 0 and 30 days"))
		}
		seconds := int(d / time.Second)
		ttl = &seconds
	}
	switch p.Action {
	case "request":
		if p.Node == "" || p.Permission == "" || p.GroupID < 0 {
			return fail(stderr, fmt.Errorf("request requires --node, --permission, and a nonnegative --group-id"))
		}
		method = "POST"
		path = "/v1/federation/peer/" + url.PathEscape(p.Node) + "/peer-access-requests"
		in := map[string]any{"permission": p.Permission, "group_id": p.GroupID, "reason": p.Reason}
		if ttl != nil {
			in["grant_ttl_seconds"] = *ttl
		}
		body = in
	case "status":
		if p.Node == "" || p.ID == "" {
			return fail(stderr, fmt.Errorf("status requires --node and --id"))
		}
		path = "/v1/federation/peer/" + url.PathEscape(p.Node) + "/peer-access-requests/" + url.PathEscape(p.ID)
	case "list":
		if p.Node != "" {
			return fail(stderr, fmt.Errorf("list is local-only"))
		}
	case "approve", "deny", "extend":
		if p.Node != "" || p.ID == "" || p.GroupID < 0 {
			return fail(stderr, fmt.Errorf("decisions require --id and are local-only"))
		}
		method = "POST"
		path += "/" + url.PathEscape(p.ID) + "/decision"
		in := map[string]any{"decision": p.Action}
		if ttl != nil {
			in["grant_ttl_seconds"] = *ttl
		}
		if p.GroupID != 0 {
			in["group_id"] = p.GroupID
		}
		if p.Seconds != 0 {
			in["secs"] = p.Seconds
		}
		body = in
	default:
		return fail(stderr, fmt.Errorf("unknown access action"))
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	var out json.RawMessage
	if err := agent.DaemonRequest(method, path, body, &out, agent.DaemonOpts{NoRetry: true}); err != nil {
		return failPeerView(stderr, err)
	}
	return printJSON(stdout, out)
}
