package federationcmd

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

type boardCommandParams struct {
	JSON         bool
	Item         string   `long:"item" optional:"true" help:"Board item ID"`
	Version      string   `long:"version" optional:"true" help:"Exact version ID"`
	Parent       string   `long:"parent" optional:"true" help:"Current parent version for an update"`
	FromBoard    string   `long:"from-board" optional:"true" help:"Republish an original signed item from this board"`
	FromItem     string   `long:"from-item" optional:"true" help:"Original item ID"`
	FromVersion  string   `long:"from-version" optional:"true" help:"Original exact version"`
	Only         []string `long:"only" optional:"true" help:"Portable sections/items to publish or import"`
	Skip         []string `long:"skip" optional:"true" help:"Excluded sections/items"`
	Set          []string `long:"set" optional:"true" help:"Placeholder name=value"`
	Replace      bool     `long:"replace" optional:"true" help:"Explicitly replace conflicts"`
	PreviewToken string   `long:"preview-token" optional:"true" help:"Token from an explicit matching preview"`
	Path         string   `long:"path" optional:"true" help:"Entry from contents listing"`
	Offset       int64    `long:"offset" optional:"true" help:"Text entry offset"`
	MaxBytes     int64    `long:"max-bytes" optional:"true" help:"Bound text inspection"`
	File         string   `long:"file" optional:"true" help:"New output file for download"`
	Action       string   `pos:"true" help:"list, create, join, show, leave, members, invite, revoke-invite, set-member, remove-member, rotate-key"`
	Board        string   `long:"board" optional:"true" help:"Immutable board ID"`
	Name         string   `long:"name" optional:"true" help:"Board display name"`
	Token        string   `long:"token" optional:"true" help:"One-time board invitation (keep private)"`
	TokenID      string   `long:"token-id" optional:"true" help:"Invitation hash to revoke"`
	Instance     string   `long:"instance" optional:"true" help:"Member instance ID"`
	Role         string   `long:"role" optional:"true" help:"reader, publisher or owner"`
	TTL          string   `long:"ttl" optional:"true" help:"Invitation lifetime (default 1h; 1m to 7d)"`
	Cursor       string   `long:"cursor" optional:"true" help:"Opaque page cursor"`
}

// Each action registers only its own flags; required markers reflect that
// action rather than the union of every board operation.
func boardsCmd() *cobra.Command {
	root := &cobra.Command{Use: "boards", Short: "Join and manage content boards without granting peer access"}
	type action struct {
		name, dispatch, short string
		flags, required       []string
		aliases               []string
	}
	actions := []action{
		{"create", "create", "Create a board", []string{"name"}, []string{"name"}, nil},
		{"ls", "list", "List joined boards", []string{"cursor"}, nil, []string{"list"}},
		{"show", "show", "Show one board", []string{"board"}, []string{"board"}, nil},
		{"join", "join", "Join with a private invitation", []string{"token"}, []string{"token"}, nil},
		{"delete", "delete", "Permanently delete a board (owner only)", []string{"board"}, []string{"board"}, nil},
		{"leave", "leave", "Leave a board", []string{"board"}, []string{"board"}, nil},
		{"invite", "invite", "Create a one-time invitation", []string{"board", "role", "ttl"}, []string{"board"}, nil},
		{"invites", "invites", "List invitation metadata (owner only)", []string{"board", "cursor"}, []string{"board"}, nil},
		{"revoke-invite", "revoke-invite", "Revoke an invitation", []string{"board", "token-id"}, []string{"board", "token-id"}, nil},
		{"members", "members", "List members", []string{"board", "cursor"}, []string{"board"}, nil},
		{"set-role", "set-member", "Set a member role", []string{"board", "instance", "role"}, []string{"board", "instance", "role"}, []string{"set-member"}},
		{"remove-member", "remove-member", "Remove a member", []string{"board", "instance"}, []string{"board", "instance"}, nil},
		{"rotate-key", "rotate-key", "Rotate the board content key", []string{"board"}, []string{"board"}, nil},
		{"items", "items", "List items and update notices", []string{"board", "cursor"}, []string{"board"}, nil},
		{"versions", "versions", "List item versions", []string{"board", "item", "cursor"}, []string{"board", "item"}, nil},
		{"publish", "publish", "Publish selected config or republish a signed item", []string{"board", "name", "item", "parent", "from-board", "from-item", "from-version", "only", "skip"}, []string{"board"}, nil},
		{"pin", "pin", "Pin an exact item version", []string{"board", "item", "version"}, []string{"board", "item", "version"}, nil},
		{"fetch", "fetch", "Fetch and verify without importing", []string{"board", "item", "version"}, []string{"board", "item", "version"}, nil},
		{"contents", "contents", "Inspect fetched contents", []string{"board", "item", "version", "path", "offset", "max-bytes"}, []string{"board", "item", "version"}, nil},
		{"download", "download", "Save a fetched bundle to a new file", []string{"board", "item", "version", "file"}, []string{"board", "item", "version", "file"}, nil},
		{"preview", "preview", "Preview an explicit import", []string{"board", "item", "version", "only", "skip", "set", "replace"}, []string{"board", "item", "version"}, nil},
		{"import", "import", "Apply the matching reviewed preview", []string{"board", "item", "version", "only", "skip", "set", "replace", "preview-token"}, []string{"board", "item", "version", "preview-token"}, nil},
	}
	for _, a := range actions {
		p := &boardCommandParams{Action: a.dispatch}
		cmd := &cobra.Command{Use: a.name, Aliases: a.aliases, Short: a.short, Args: cobra.NoArgs, Run: func(_ *cobra.Command, _ []string) { os.Exit(runBoardCommand(p, os.Stdout, os.Stderr)) }}
		for _, name := range a.flags {
			addBoardFlag(cmd, p, name)
		}
		for _, name := range a.required {
			cmd.Flags().Lookup(name).Usage += " (required)"
			if err := cmd.MarkFlagRequired(name); err != nil {
				panic(err)
			}
		}
		cmd.Flags().BoolVar(&p.JSON, "json", false, "Print raw JSON")
		root.AddCommand(cmd)
	}
	return root
}
func addBoardFlag(cmd *cobra.Command, p *boardCommandParams, name string) {
	f := cmd.Flags()
	strings := map[string]struct {
		value *string
		help  string
	}{
		"board": {&p.Board, "Immutable board ID"}, "name": {&p.Name, "Display name (required for a new publication unless republishing)"},
		"token": {&p.Token, "Private one-time invitation"}, "token-id": {&p.TokenID, "Invitation hash to revoke"}, "instance": {&p.Instance, "Member instance ID"},
		"role": {&p.Role, "reader, publisher or owner (invite defaults to reader)"}, "ttl": {&p.TTL, "Invitation lifetime, default 1h (1m to 168h)"}, "cursor": {&p.Cursor, "Opaque page cursor"},
		"item": {&p.Item, "Item ID"}, "version": {&p.Version, "Exact version ID"}, "parent": {&p.Parent, "Latest parent version when updating an item"},
		"from-board": {&p.FromBoard, "Republish an original signed item from this board"}, "from-item": {&p.FromItem, "Original item ID"}, "from-version": {&p.FromVersion, "Original version ID"},
		"preview-token": {&p.PreviewToken, "Token from a matching explicit preview"}, "path": {&p.Path, "Entry path from contents listing"}, "file": {&p.File, "New local output file"},
	}
	if v, ok := strings[name]; ok {
		f.StringVar(v.value, name, "", v.help)
		return
	}
	switch name {
	case "only":
		f.StringSliceVar(&p.Only, name, nil, "Select portable sections/items")
	case "skip":
		f.StringSliceVar(&p.Skip, name, nil, "Exclude sections/items")
	case "set":
		f.StringArrayVar(&p.Set, name, nil, "Placeholder name=value")
	case "replace":
		f.BoolVar(&p.Replace, name, false, "Explicitly replace conflicts")
	case "offset":
		f.Int64Var(&p.Offset, name, 0, "Text entry byte offset")
	case "max-bytes":
		f.Int64Var(&p.MaxBytes, name, 0, "Bound text inspection")
	default:
		panic("unknown board flag: " + name)
	}
}
func runBoardCommand(p *boardCommandParams, stdout, stderr io.Writer) int {
	if isBoardItemAction(p.Action) {
		return runBoardItemCommand(p, stdout, stderr)
	}
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
	case "delete":
		method = "DELETE"
	case "leave":
		method = "DELETE"
		path += "/membership"
	case "invites":
		path += "/invites"
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
	if !p.JSON && (p.Action == "list" || p.Action == "invites") {
		return printRecordingTable(stdout, p.Action, out)
	}
	return printJSON(stdout, out)
}
