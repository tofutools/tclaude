package federationcmd

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

type boardCommandParams struct {
	Action   string `pos:"true" help:"list, create, join, show, leave, members, invite, revoke-invite, set-member, remove-member, rotate-key"`
	Board    string `long:"board" help:"Immutable board ID"`
	Name     string `long:"name" help:"Board display name"`
	Token    string `long:"token" help:"One-time board invitation (keep private)"`
	TokenID  string `long:"token-id" help:"Invitation hash to revoke"`
	Instance string `long:"instance" help:"Member instance ID"`
	Role     string `long:"role" help:"reader, publisher or owner"`
	TTL      string `long:"ttl" help:"Invitation lifetime (default 1h; 1m to 7d)"`
	Cursor   string `long:"cursor" help:"Opaque page cursor"`
}

func boardsCmd() *cobra.Command {
	return boa.CmdT[boardCommandParams]{Use: "boards", Short: "Join and manage content boards without granting peer access", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *boardCommandParams, _ *cobra.Command, _ []string) {
		os.Exit(runBoardCommand(p, os.Stdout, os.Stderr))
	}}.ToCobra()
}
func runBoardCommand(p *boardCommandParams, stdout, stderr io.Writer) int {
	method, path := "GET", "/v1/federation/boards"
	body := map[string]any{}
	invalid := func(s string) int { return fail(stderr, fmt.Errorf("boards: %s", s)) }
	if p.Action != "list" && p.Action != "create" && p.Action != "join" {
		if p.Board == "" {
			return invalid("--board is required")
		}
		path += "/" + url.PathEscape(p.Board)
	}
	switch p.Action {
	case "list", "show":
	case "create":
		if p.Name == "" {
			return invalid("--name required")
		}
		method = "POST"
		body["name"] = p.Name
	case "join":
		if p.Token == "" {
			return invalid("--token required")
		}
		method = "POST"
		path += "/join"
		body["token"] = p.Token
	case "leave":
		method = "DELETE"
		path += "/membership"
	case "members":
		path += "/members"
	case "invite":
		method = "POST"
		path += "/invites"
		body["role"] = p.Role
		if p.Role == "" {
			body["role"] = "reader"
		}
		ttl := time.Hour
		var err error
		if p.TTL != "" {
			ttl, err = time.ParseDuration(p.TTL)
		}
		if err != nil || ttl < time.Minute || ttl > 7*24*time.Hour || ttl%time.Second != 0 {
			return invalid("--ttl must be whole seconds between 1m and 168h")
		}
		body["ttl_seconds"] = int64(ttl / time.Second)
	case "revoke-invite":
		if p.TokenID == "" {
			return invalid("--token-id required")
		}
		method = "DELETE"
		path += "/invites/" + url.PathEscape(p.TokenID)
	case "set-member", "remove-member":
		if p.Instance == "" {
			return invalid("--instance required")
		}
		method = "DELETE"
		if p.Action == "set-member" {
			method = "PUT"
			body["role"] = p.Role
		}
		path += "/members/" + url.PathEscape(p.Instance)
	case "rotate-key":
		method = "POST"
		path += "/rotate-key"
	default:
		return invalid("unknown action")
	}
	if strings.Contains(path, "..") {
		return invalid("invalid route")
	}
	if p.Cursor != "" {
		path += "?cursor=" + url.QueryEscape(p.Cursor)
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	var out any
	if err := agent.DaemonRequest(method, path, body, &out, agent.DaemonOpts{NoRetry: true, Timeout: 35 * time.Second}); err != nil {
		return fail(stderr, err)
	}
	return printJSON(stdout, out)
}
