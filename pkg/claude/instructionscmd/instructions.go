// Package instructionscmd exposes the operator's managed instruction library.
package instructionscmd

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/managedinstructions"
	"github.com/tofutools/tclaude/pkg/claude/common/skillbundle"
	"github.com/tofutools/tclaude/pkg/common"
)

type Params struct {
	Action    string `pos:"true" help:"ls, show, add, import, rm"`
	Name      string `pos:"true" optional:"true" help:"Instruction name"`
	Group     string `long:"group" optional:"true" help:"Group managed library"`
	User      bool   `long:"user" help:"Operator-wide managed library (never installs into harness HOME)"`
	Path      string `long:"path" optional:"true" help:"Skill directory or rule text file to import explicitly"`
	Overwrite bool   `long:"overwrite" help:"Explicitly replace an existing managed item"`
}

func Cmd(kind string) *cobra.Command {
	return boa.CmdT[Params]{Use: map[string]string{"skill": "skills", "rule": "rules"}[kind], Short: "Manage operator " + kind + " instructions without installing into harness HOME", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *Params, _ *cobra.Command, _ []string) { os.Exit(run(kind, p)) }}.ToCobra()
}
func run(kind string, p *Params) int {
	if (p.Group == "") == !p.User {
		fmt.Fprintln(os.Stderr, "choose exactly one of --group or --user")
		return 1
	}
	if p.Action != "ls" && p.Name == "" {
		fmt.Fprintln(os.Stderr, "instruction name required")
		return 1
	}
	method := "GET"
	var body any
	q := url.Values{}
	if p.User {
		q.Set("user", "1")
	} else {
		q.Set("group", p.Group)
	}
	if p.Action != "ls" {
		q.Set("name", p.Name)
	}
	switch p.Action {
	case "ls", "show":
	case "add", "import":
		if p.Path == "" {
			fmt.Fprintln(os.Stderr, "requires --path (skill directory or rule file)")
			return 1
		}
		raw, err := managedinstructions.Import(kind, p.Name, p.Path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		method = "POST"
		body = map[string]any{"user": p.User, "group": p.Group, "name": p.Name, "content": raw, "overwrite": p.Overwrite}
	case "rm":
		method = "DELETE"
		body = map[string]any{"user": p.User, "group": p.Group, "name": p.Name}
	default:
		fmt.Fprintln(os.Stderr, "unknown action; use ls, show, add, import or rm")
		return 1
	}
	if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
		return rc
	}
	path := "/v1/instructions/" + kind
	if method == "GET" {
		path += "?" + q.Encode()
	}
	var out json.RawMessage
	if err := agent.DaemonRequest(method, path, body, &out, agent.DaemonOpts{NoRetry: true, Timeout: 10 * time.Second}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if p.Action == "show" {
		var row db.ManagedInstruction
		if err := json.Unmarshal(out, &row); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		if kind == "skill" {
			bundle, err := skillbundle.Decode(row.Content)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
			file := p.Path
			if file == "" {
				file = "SKILL.md"
			}
			for _, f := range bundle.Files {
				if f.Path == file {
					fmt.Println(string(f.Data))
					return 0
				}
			}
			fmt.Fprintln(os.Stderr, "skill file not found")
			return 1
		}
		if kind == "rule" {
			fmt.Println(strings.TrimRight(string(row.Content), "\n"))
			return 0
		}
	}
	fmt.Println(string(out))
	return 0
}
