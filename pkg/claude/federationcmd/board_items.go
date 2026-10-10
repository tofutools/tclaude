package federationcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/federation/bundletransfer"
)

func isBoardItemAction(action string) bool {
	return slices.Contains([]string{"items", "publish", "versions", "pin", "fetch", "contents", "download", "preview", "import"}, action)
}
func runBoardItemCommand(p *boardCommandParams, stdout, stderr io.Writer) int {
	invalid := func(s string) int { return fail(stderr, fmt.Errorf("boards: %s", s)) }
	if p.Board == "" {
		return invalid("--board required")
	}
	path := "/v1/federation/boards/" + url.PathEscape(p.Board) + "/items"
	method := "GET"
	body := map[string]any{"name": p.Name, "item": p.Item, "parent": p.Parent, "only": p.Only, "skip": p.Skip, "replace": p.Replace, "preview_token": p.PreviewToken}
	if p.FromBoard != "" {
		if p.FromItem == "" || p.FromVersion == "" {
			return invalid("--from-item and --from-version required")
		}
		body["source"] = map[string]string{"board": p.FromBoard, "item": p.FromItem, "version": p.FromVersion}
	}
	values := map[string]string{}
	for _, pair := range p.Set {
		name, value, ok := strings.Cut(pair, "=")
		if !ok || name == "" {
			return invalid("--set requires name=value")
		}
		values[name] = value
	}
	body["values"] = values
	if p.Action != "items" && p.Action != "publish" {
		if p.Item == "" {
			return invalid("--item required")
		}
		path += "/" + url.PathEscape(p.Item)
	}
	switch p.Action {
	case "items":
	case "publish":
		method = "POST"
	case "versions":
		path += "/versions"
	case "pin":
		if p.Version == "" {
			return invalid("--version required")
		}
		method = "PUT"
		path += "/pin"
		body["version"] = p.Version
	default:
		if p.Version == "" {
			return invalid("--version required")
		}
		path += "/versions/" + url.PathEscape(p.Version) + "/" + p.Action
		if p.Action == "fetch" || p.Action == "preview" || p.Action == "import" {
			method = "POST"
		}
		if p.Action == "import" && p.PreviewToken == "" {
			return invalid("--preview-token from preview required")
		}
	}
	query := url.Values{}
	if p.Cursor != "" {
		query.Set("cursor", p.Cursor)
	}
	if p.Path != "" {
		query.Set("path", p.Path)
	}
	if p.Offset != 0 {
		query.Set("offset", fmt.Sprint(p.Offset))
	}
	if p.MaxBytes != 0 {
		query.Set("max_bytes", fmt.Sprint(p.MaxBytes))
	}
	if len(query) > 0 {
		path += "?" + query.Encode()
	}
	if rc := agent.RequireDaemonOrExit(stderr); rc != 0 {
		return rc
	}
	if p.Action == "download" {
		if p.File == "" {
			return invalid("--file required (a new file)")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		body, e := agent.DaemonStreamGet(ctx, path)
		if e != nil {
			return fail(stderr, e)
		}
		defer func() { _ = body.Close() }()
		f, e := os.OpenFile(p.File, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return fail(stderr, e)
		}
		n, e := io.Copy(f, io.LimitReader(body, bundletransfer.Config.MaxBytes+1))
		ce := f.Close()
		if e == nil {
			e = ce
		}
		if n > bundletransfer.Config.MaxBytes {
			e = errors.New("board download exceeds 16 MiB")
		}
		if e != nil {
			_ = os.Remove(p.File)
			return fail(stderr, e)
		}
		return 0
	}
	var out any
	if e := agent.DaemonRequest(method, path, body, &out, agent.DaemonOpts{NoRetry: true, Timeout: 5 * time.Minute}); e != nil {
		return fail(stderr, e)
	}
	if p.Action == "items" && !p.JSON {
		return printRecordingTable(stdout, "items", out)
	}
	return printJSON(stdout, out)
}
