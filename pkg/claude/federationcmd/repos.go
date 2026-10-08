package federationcmd

import (
	"net/http"
	"net/url"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/common"
)

type repoAddParams struct {
	Name     string   `pos:"true" help:"Repository alias"`
	URL      string   `long:"url" help:"Exact allowed Git URL (HTTPS, SSH or explicit file URL)"`
	Clone    string   `long:"clone" help:"Existing local clone whose identity anchors this entry"`
	Groups   []string `long:"group" help:"Allowed active receiving group (repeatable)"`
	Revision int64    `long:"revision" optional:"true" help:"Current revision when updating"`
}
type repoRemoveParams struct {
	Name string `pos:"true" help:"Repository alias or immutable ID"`
}

func repoRequest(method, path string, body any) {
	if rc := agent.RequireDaemonOrExit(os.Stderr); rc != 0 {
		os.Exit(rc)
	}
	var out any
	if e := agent.DaemonRequest(method, path, body, &out, agent.DaemonOpts{}); e != nil {
		os.Exit(fail(os.Stderr, e))
	}
	os.Exit(printJSON(os.Stdout, out))
}
func reposCmd() *cobra.Command {
	write := func(p *repoAddParams, method string) {
		path := "/v1/federation/repos"
		if method == http.MethodPut {
			path += "/" + url.PathEscape(p.Name)
		}
		repoRequest(method, path, map[string]any{"name": p.Name, "url": p.URL, "clone": p.Clone, "groups": p.Groups, "revision": p.Revision})
	}
	return boa.CmdT[struct{}]{Use: "repos", Short: "Allow repositories for remote jobs and transfers", ParamEnrich: common.DefaultParamEnricher(), SubCmds: []*cobra.Command{
		boa.CmdT[struct{}]{Use: "ls", Short: "List allowed repositories", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(_ *struct{}, _ *cobra.Command, _ []string) {
			repoRequest(http.MethodGet, "/v1/federation/repos", nil)
		}}.ToCobra(),
		boa.CmdT[repoAddParams]{Use: "add", Short: "Allow a repository for explicit receiving groups", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *repoAddParams, _ *cobra.Command, _ []string) { write(p, http.MethodPost) }}.ToCobra(),
		boa.CmdT[repoAddParams]{Use: "update", Short: "Replace an entry using its current revision", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *repoAddParams, _ *cobra.Command, _ []string) { write(p, http.MethodPut) }}.ToCobra(),
		boa.CmdT[repoRemoveParams]{Use: "remove", Short: "Disable an entry and invalidate queued admissions", ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *repoRemoveParams, _ *cobra.Command, _ []string) {
			repoRequest(http.MethodDelete, "/v1/federation/repos/"+url.PathEscape(p.Name), nil)
		}}.ToCobra(),
	}}.ToCobra()
}
