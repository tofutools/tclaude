package agent

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent show` / `tclaude agent page` — live pages (epic tcl-qo90av):
// the agent shows the operator a web page it serves, live in the dashboard's
// Collab tab, in a sandboxed frame on its own origin. Needs self.page and
// Collab on; the commands are hidden while Collab is off.

// pageCollabHidden hides the page commands while Collab is off. The daemon
// refuses the routes either way.
func pageCollabHidden() bool {
	cfg, err := config.LoadShared()
	return err != nil || !cfg.CollabEnabled()
}

const pageShowLong = "Shows the operator a live web page in the dashboard: a dev server you run, or static files in your worktree. " +
	"The operator sees it live and interactive in a sandboxed frame on its own origin.\n\n" +
	"The target is one of:\n" +
	"  :PORT[/path]                    your own dev server (Vite, Next, Storybook, ...), e.g. :5173/app\n" +
	"  http://localhost:PORT/path      the same, as a URL\n" +
	"  DIR                             a static directory in your worktree (serves its index.html)\n" +
	"  FILE.html                       one HTML file in your worktree\n\n" +
	"Nothing else: no host names or IPs, no other agent's port, nothing outside your worktree. " +
	"The dev server must listen on 127.0.0.1 (start it with --host 127.0.0.1 if it binds ::1 only), and is checked to be yours.\n\n" +
	"Showing again with the same --name updates that page in place: a new path or port reloads it on the same origin. " +
	"Switching between a port and static files gives the page a new origin. Without --name the name comes from the target. " +
	"A static source is watched, so saving a file reloads the page; a dev server reloads through its own HMR.\n\n" +
	"Anything the operator types into the page can be read by you. The printed origin is the page's only key: keep it to yourself. " +
	"Needs the self.page permission and Collab on."

func showCmd() *cobra.Command {
	cmd := boa.CmdT[pageShowParams]{
		Use:         "show <target>",
		Short:       "Show the operator a live page (short for `agent page show`)",
		Long:        pageShowLong,
		Args:        cobra.ExactArgs(1),
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *pageShowParams, _ *cobra.Command, args []string) {
			os.Exit(runPageShow(p, args[0], os.Stdout, os.Stderr))
		},
	}.ToCobra()
	cmd.Hidden = pageCollabHidden()
	return cmd
}

func pageCmd() *cobra.Command {
	cmd := boa.CmdT[struct{}]{
		Use:   "page",
		Short: "Show, list and close live pages",
		Long: "Live pages show the operator a web page you serve, live in the dashboard's Collab tab. " +
			"`page show` (or `agent show`) shows or updates one, `page ls` lists yours, `page close` closes one. " +
			"You can act only on pages you showed. Needs the self.page permission and Collab on.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			boa.CmdT[pageShowParams]{
				Use:         "show <target>",
				Short:       "Show the operator a live page, or update one in place",
				Long:        pageShowLong,
				Args:        cobra.ExactArgs(1),
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *pageShowParams, _ *cobra.Command, args []string) {
					os.Exit(runPageShow(p, args[0], os.Stdout, os.Stderr))
				},
			}.ToCobra(),
			boa.CmdT[pageLsParams]{
				Use:         "ls",
				Short:       "List your live pages",
				Args:        cobra.NoArgs,
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *pageLsParams, _ *cobra.Command, _ []string) {
					os.Exit(runPageLs(p, os.Stdout, os.Stderr))
				},
			}.ToCobra(),
			boa.CmdT[struct{}]{
				Use:         "close <name>",
				Short:       "Close one of your live pages",
				Long:        "Closes the page: the operator's tab goes away and its origin stops answering for good.",
				Args:        cobra.ExactArgs(1),
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(_ *struct{}, _ *cobra.Command, args []string) {
					os.Exit(runPageClose(args[0], os.Stdout, os.Stderr))
				},
			}.ToCobra(),
		},
	}.ToCobra()
	cmd.Hidden = pageCollabHidden()
	return cmd
}

type pageShowParams struct {
	Title string `long:"title" optional:"true" help:"The tab's title (default: the name)"`
	Name  string `long:"name" optional:"true" help:"The page's name; showing again with the same name updates it in place (default: from the target)"`
	JSON  bool   `long:"json" help:"Output JSON"`
}

type pageLsParams struct {
	JSON bool `long:"json" help:"Output JSON"`
}

type pageWire struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Title     string    `json:"title"`
	Target    string    `json:"target"`
	Kind      string    `json:"kind"`
	Worktree  string    `json:"worktree,omitempty"`
	State     string    `json:"state"`
	Origin    string    `json:"origin"`
	URL       string    `json:"url"`
	ShownAt   time.Time `json:"shown_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Created   bool      `json:"created,omitempty"`
	NewOrigin bool      `json:"new_origin,omitempty"`
	Rules     []string  `json:"rules,omitempty"`
}

// pageTargetForDaemon makes a file target absolute against the caller's
// directory. Port targets go as typed; the daemon validates everything.
func pageTargetForDaemon(target string) string {
	t := strings.TrimSpace(target)
	if t == "" || strings.HasPrefix(t, ":") || strings.Contains(t, "://") || filepath.IsAbs(t) {
		return t
	}
	if abs, err := filepath.Abs(t); err == nil {
		return abs
	}
	return t
}

func runPageShow(p *pageShowParams, target string, stdout, stderr io.Writer) int {
	body := map[string]any{"target": pageTargetForDaemon(target), "title": p.Title, "name": p.Name}
	var resp pageWire
	if err := DaemonRequest(http.MethodPost, "/v1/pages", body, &resp, DaemonOpts{Timeout: 30 * time.Second}); err != nil {
		return boardErr(stderr, err)
	}
	if p.JSON {
		return writeBoardJSON(stdout, resp)
	}
	verb := "Updated"
	switch {
	case resp.Created:
		verb = "Showing"
	case resp.NewOrigin:
		verb = "Showing (new origin)"
	}
	fmt.Fprintf(stdout, "%s page %q (%s) to the operator.\n", verb, resp.Name, resp.Title)
	source := resp.Target
	if resp.Kind == "port" {
		source += "  (your dev server)"
	} else {
		source += "  (static, read-only, watched)"
	}
	fmt.Fprintf(stdout, "  source:   %s\n", source)
	if resp.Worktree != "" {
		fmt.Fprintf(stdout, "  worktree: %s\n", resp.Worktree)
	}
	if resp.Origin != "" {
		fmt.Fprintf(stdout, "  origin:   %s  (sandboxed frame, its own origin)\n", resp.Origin)
	} else {
		fmt.Fprintln(stdout, "  origin:   (the dashboard is not listening; the page opens when it is)")
	}
	if len(resp.Rules) > 0 {
		fmt.Fprintln(stdout, "  rules:")
		for _, r := range resp.Rules {
			fmt.Fprintf(stdout, "    - %s\n", r)
		}
	}
	return rcOK
}

func runPageLs(p *pageLsParams, stdout, stderr io.Writer) int {
	var resp struct {
		Pages []pageWire `json:"pages"`
	}
	if err := DaemonRequest(http.MethodGet, "/v1/pages", nil, &resp, DaemonOpts{}); err != nil {
		return boardErr(stderr, err)
	}
	if p.JSON {
		return writeBoardJSON(stdout, resp)
	}
	if len(resp.Pages) == 0 {
		fmt.Fprintln(stdout, "No live pages.")
		return rcOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTITLE\tSOURCE\tSTATE\tUPDATED\tORIGIN")
	for _, pg := range resp.Pages {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", pg.Name, pg.Title, pg.Target, pg.State,
			pg.UpdatedAt.Local().Format("2006-01-02 15:04"), pg.Origin)
	}
	_ = tw.Flush()
	return rcOK
}

func runPageClose(name string, stdout, stderr io.Writer) int {
	if err := DaemonRequest(http.MethodDelete, "/v1/pages/"+url.PathEscape(name), nil, nil, DaemonOpts{}); err != nil {
		return boardErr(stderr, err)
	}
	fmt.Fprintf(stdout, "Closed page %q.\n", name)
	return rcOK
}
