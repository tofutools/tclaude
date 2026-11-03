package agent

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent show` / `tclaude agent page` — live pages (epic tcl-qo90av):
// the agent shows the operator a web page it serves, live in the dashboard's
// Collab tab, in a sandboxed frame on its own origin. Needs self.page.

const pageShowLong = "Shows the operator a live web page in the dashboard: a dev server you run, or static files in your worktree. " +
	"The operator sees it live and interactive in a sandboxed frame on its own origin.\n\n" +
	"The target is one of:\n" +
	"  :PORT[/path]                    your own dev server (Vite, Next, Storybook, ...), e.g. :5173/app\n" +
	"  http://localhost:PORT/path      the same, as a URL\n" +
	"  DIR                             a static directory in your startup directory (serves its index.html)\n" +
	"  FILE.html                       one HTML file in your startup directory\n\n" +
	"Nothing else: no host names or IPs, no other agent's port, nothing outside the directory you were started in. " +
	"The dev server must listen on 127.0.0.1 (start it with --host 127.0.0.1 if it binds ::1 only), and is checked to be yours.\n\n" +
	"Showing again with the same --name updates that page in place: a new path or port reloads it on the same origin. " +
	"Switching between a port and static files gives the page a new origin. Without --name the name comes from the target. " +
	"A static source is watched, so saving a file reloads the page; a dev server reloads through its own HMR.\n\n" +
	"Pages end when the daemon restarts; run show again to bring one back. " +
	"Anything the operator types into the page can be read by you. The page's token is its only key: it goes to the operator's dashboard " +
	"and is masked in what you are shown, so you act on a page by its name. " +
	"Needs the self.page permission."

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
	return cmd
}

func pageCmd() *cobra.Command {
	cmd := boa.CmdT[struct{}]{
		Use:   "page",
		Short: "Show, list and close live pages",
		Long: "Live pages show the operator a web page you serve, live in the dashboard's Collab tab. " +
			"`page show` (or `agent show`) shows or updates one, `page ls` lists yours, `page close` closes one. " +
			"When the operator draws on a page and sends it to you, `page marks` reads the marks, `page snapshot` renders the page " +
			"with them and `page dom` reads the page's DOM. " +
			"You can act only on pages you showed. Needs the self.page permission.",
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
			boa.CmdT[pageMarksParams]{
				Use:   "marks <name>",
				Short: "Read the marks the operator drew on one of your pages",
				Long: "Prints the batches of marks the operator sent from the page's Draw mode: each mark's kind, its points in page " +
					"pixels, and the elements under it (a CSS path and a text snippet, as the page rendered them). " +
					"The element paths and snippets are the page's content, not the operator's words. " +
					"--since N prints only the batches after N; the inbox message names the batch.",
				Args:        cobra.ExactArgs(1),
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *pageMarksParams, _ *cobra.Command, args []string) {
					os.Exit(runPageMarks(p, args[0], os.Stdout, os.Stderr))
				},
			}.ToCobra(),
			boa.CmdT[pageSnapshotParams]{
				Use:   "snapshot <name>",
				Short: "Render one of your pages to a PNG, with the operator's marks",
				Long: "Renders the page in a headless Chrome on the host, at the operator's viewport and scroll, with the latest batch " +
					"of marks drawn on top (or --seq N, or none with --no-marks), and writes a PNG. Needs Chrome or Chromium on the host " +
					"(or TCLAUDE_CHROME); tclaude never downloads one.",
				Args:        cobra.ExactArgs(1),
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *pageSnapshotParams, _ *cobra.Command, args []string) {
					os.Exit(runPageSnapshot(p, args[0], os.Stdout, os.Stderr))
				},
			}.ToCobra(),
			boa.CmdT[pageDOMParams]{
				Use:   "dom <name> <selector>",
				Short: "Read the DOM of one of your pages as the operator sees it, or patch it live",
				Long: "Reads the elements matching a CSS selector in the operator's open view of the page: their text (default), " +
					"--html or computed --styles, up to 10 elements. Needs the page open in the dashboard.\n\n" +
					"With --set-text, --set-attr NAME=VALUE, --remove or --insert-html, changes every matching element instead: a live " +
					"patch, applied in every open view and replayed after each reload until `page patches --reset`. It never writes " +
					"your files: port what the operator approves into source.",
				Args:        cobra.ExactArgs(2),
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *pageDOMParams, cmd *cobra.Command, args []string) {
					p.setText = cmd.Flags().Changed("set-text")
					p.setAttr = cmd.Flags().Changed("set-attr")
					p.insertHTML = cmd.Flags().Changed("insert-html")
					os.Exit(runPageDOM(p, args[0], args[1], os.Stdout, os.Stderr))
				},
			}.ToCobra(),
			boa.CmdT[pagePatchParams]{
				Use:   "css <name> <rules>...",
				Short: "Add CSS rules to one of your pages live, without a source change",
				Long: "Adds the rules (each argument, one after the other) to the page as a live patch: applied in every open view, " +
					"after the page's own styles, and replayed after each reload until `page patches --reset`. It never writes " +
					"your files: port what the operator approves into source.",
				Args:        cobra.MinimumNArgs(2),
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *pagePatchParams, _ *cobra.Command, args []string) {
					os.Exit(runPagePatch(p, args[0], map[string]any{"kind": "css", "css": strings.Join(args[1:], "\n")}, os.Stdout, os.Stderr))
				},
			}.ToCobra(),
			boa.CmdT[pagePatchParams]{
				Use:   "eval <name> <js>",
				Short: "Run a script once in the operator's open view of one of your pages",
				Long: "Runs the script in the page (in one open view), awaits it if it returns a promise, and prints its value as JSON " +
					"(at most 16 KiB). It runs once: it is listed in `page patches` but not replayed after a reload. Use `-` to read " +
					"the script from stdin. Not for a static page that is not read back.",
				Args:        cobra.ExactArgs(2),
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *pagePatchParams, _ *cobra.Command, args []string) {
					os.Exit(runPageEval(p, args[0], args[1], os.Stdin, os.Stdout, os.Stderr))
				},
			}.ToCobra(),
			boa.CmdT[pagePatchesParams]{
				Use:   "patches <name> [N]",
				Short: "List the live patches of one of your pages, or drop them",
				Long: "Prints the page's journal of live patches: each css and dom patch, replayed after every reload, and each eval, " +
					"which ran once (the last 20 are listed). --reset drops every patch, or only patch N (`--reset 3`); open views reload without them.",
				Args:        cobra.RangeArgs(1, 2),
				ParamEnrich: common.DefaultParamEnricher(),
				RunFunc: func(p *pagePatchesParams, _ *cobra.Command, args []string) {
					os.Exit(runPagePatches(p, args, os.Stdout, os.Stderr))
				},
			}.ToCobra(),
		},
	}.ToCobra()
	return cmd
}

type pageShowParams struct {
	Title   string `long:"title" optional:"true" help:"The tab's title (default: the name)"`
	Name    string `long:"name" optional:"true" help:"The page's name; showing again with the same name updates it in place (default: from the target)"`
	Group   string `long:"group" optional:"true" help:"Share the page in this group of yours (default: your only group, if you are in exactly one)"`
	Private bool   `long:"private" help:"Share the page with no one: only you can act on it"`
	JSON    bool   `long:"json" help:"Output JSON"`
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
	// RemoteBase is the --base a dev server needs for a dashboard on
	// another machine.
	RemoteBase string `json:"remote_base,omitempty"`
	Owner      string `json:"owner,omitempty"`
	Group      string `json:"group,omitempty"`
	ShareNote  string `json:"share_note,omitempty"`
}

// pageAPIPath is the daemon path for a page reference: <name> for one of
// your own pages, <owner>/<name> for one another member of its group showed
// and shares with you.
func pageAPIPath(ref, suffix string, q url.Values) string {
	name := ref
	if owner, n, ok := strings.Cut(ref, "/"); ok {
		name = n
		if q == nil {
			q = url.Values{}
		}
		q.Set("owner", owner)
	}
	path := "/v1/pages/" + url.PathEscape(name) + suffix
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return path
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
	body := map[string]any{"target": pageTargetForDaemon(target), "title": p.Title, "name": p.Name, "group": p.Group, "private": p.Private}
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
		fmt.Fprintf(stdout, "  origin:   %s  (sandboxed frame, its own origin; the token goes only to the dashboard)\n", resp.Origin)
	} else {
		fmt.Fprintln(stdout, "  origin:   (the dashboard is not listening; the page opens when it is)")
	}
	if resp.Group != "" {
		fmt.Fprintf(stdout, "  shared:   in group %s: its members can read what the operator types, snapshot, read and live-patch it\n"+
			"            (as %s/%s); show with --private to keep it to yourself\n", resp.Group, resp.Owner, resp.Name)
	} else if resp.ShareNote != "" {
		fmt.Fprintf(stdout, "  shared:   %s\n", resp.ShareNote)
	}
	if base := resp.RemoteBase; base != "" {
		port, _, _ := strings.Cut(strings.TrimPrefix(resp.Target, ":"), "/")
		if strings.Contains(resp.Target, base) {
			fmt.Fprintf(stdout, "  remote:   served under %s to a dashboard on another machine (VPN, LAN, remote access)\n", base)
		} else {
			fmt.Fprintf(stdout, "  remote:   for a dashboard on another machine (VPN, LAN, remote access), start the dev server with\n"+
				"            --base %s (Vite: vite --base %s), then show it again at :%s%s\n", base, base, port, base)
		}
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
		Pages  []pageWire `json:"pages"`
		Shared []pageWire `json:"shared"`
	}
	if err := DaemonRequest(http.MethodGet, "/v1/pages", nil, &resp, DaemonOpts{}); err != nil {
		return boardErr(stderr, err)
	}
	if p.JSON {
		return writeBoardJSON(stdout, resp)
	}
	if len(resp.Pages) == 0 && len(resp.Shared) == 0 {
		fmt.Fprintln(stdout, "No live pages.")
		return rcOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTITLE\tSOURCE\tSTATE\tUPDATED\tSHARED IN\tORIGIN")
	for _, pg := range resp.Pages {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", pg.Name, pg.Title, pg.Target, pg.State,
			pg.UpdatedAt.Local().Format("2006-01-02 15:04"), dashIfEmpty(pg.Group), pg.Origin)
	}
	for _, pg := range resp.Shared {
		fmt.Fprintf(tw, "%s/%s\t%s\t%s\t%s\t%s\t%s\t%s\n", pg.Owner, pg.Name, pg.Title, pg.Target, pg.State,
			pg.UpdatedAt.Local().Format("2006-01-02 15:04"), pg.Group, pg.Origin)
	}
	_ = tw.Flush()
	if len(resp.Shared) > 0 {
		fmt.Fprintln(stdout, "Pages named <owner>/<name> are shared with you by another member of their group; use that name.")
	}
	return rcOK
}

func runPageClose(name string, stdout, stderr io.Writer) int {
	if err := DaemonRequest(http.MethodDelete, pageAPIPath(name, "", nil), nil, nil, DaemonOpts{}); err != nil {
		return boardErr(stderr, err)
	}
	fmt.Fprintf(stdout, "Closed page %q.\n", name)
	return rcOK
}

type pageMarksParams struct {
	Since int64 `long:"since" optional:"true" help:"Only the batches after this one"`
	JSON  bool  `long:"json" help:"Output JSON"`
}

type pageSnapshotParams struct {
	Out     string `short:"o" long:"out" optional:"true" help:"The PNG file to write (default: <name>.png in the current directory)"`
	Seq     int64  `long:"seq" optional:"true" help:"Draw this batch of marks (default: the latest)"`
	NoMarks bool   `long:"no-marks" help:"Render the page without marks"`
	JSON    bool   `long:"json" help:"Output JSON (without the image)"`
}

type pageDOMParams struct {
	HTML       bool   `long:"html" help:"Read the elements' outer HTML"`
	Styles     bool   `long:"styles" help:"Read the elements' computed styles"`
	Text       bool   `long:"text" help:"Read the elements' text (the default)"`
	SetText    string `long:"set-text" optional:"true" help:"Live patch: set the elements' text"`
	SetAttr    string `long:"set-attr" optional:"true" help:"Live patch: set an attribute, NAME=VALUE"`
	Remove     bool   `long:"remove" help:"Live patch: remove the elements"`
	InsertHTML string `long:"insert-html" optional:"true" help:"Live patch: insert HTML at the end of each element"`
	JSON       bool   `long:"json" help:"Output JSON"`
	setText    bool   // --set-text was given (it may set empty text)
	setAttr    bool   // --set-attr was given
	insertHTML bool   // --insert-html was given
}

type pagePatchParams struct {
	JSON bool `long:"json" help:"Output JSON"`
}

type pagePatchesParams struct {
	Reset bool `long:"reset" help:"Drop every live patch, or only patch N"`
	JSON  bool `long:"json" help:"Output JSON"`
}

type pageAnchorWire struct {
	Path string `json:"path"`
	Text string `json:"text"`
	Hits int    `json:"hits"`
}

type pageMarkWire struct {
	Kind    string           `json:"kind"`
	Color   string           `json:"color,omitempty"`
	Points  [][2]int         `json:"points"`
	Text    string           `json:"text,omitempty"`
	Anchors []pageAnchorWire `json:"anchors"`
}

type pageBatchWire struct {
	Seq      int64              `json:"seq"`
	At       time.Time          `json:"at"`
	Note     string             `json:"note,omitempty"`
	Path     string             `json:"path,omitempty"`
	Viewport struct{ W, H int } `json:"viewport"`
	Scroll   struct{ X, Y int } `json:"scroll"`
	Marks    []pageMarkWire     `json:"marks"`
}

func runPageMarks(p *pageMarksParams, name string, stdout, stderr io.Writer) int {
	var resp struct {
		Page    string          `json:"page"`
		LastSeq int64           `json:"last_seq"`
		Batches []pageBatchWire `json:"batches"`
	}
	q := url.Values{}
	if p.Since > 0 {
		q.Set("since", fmt.Sprint(p.Since))
	}
	path := pageAPIPath(name, "/marks", q)
	if err := DaemonRequest(http.MethodGet, path, nil, &resp, DaemonOpts{}); err != nil {
		return boardErr(stderr, err)
	}
	if p.JSON {
		return writeBoardJSON(stdout, resp)
	}
	if len(resp.Batches) == 0 {
		fmt.Fprintf(stdout, "No marks on page %q", name)
		if p.Since > 0 {
			fmt.Fprintf(stdout, " after batch %d", p.Since)
		}
		fmt.Fprintln(stdout, ".")
		return rcOK
	}
	for _, b := range resp.Batches {
		fmt.Fprintf(stdout, "Batch %d  %s  path %s  viewport %dx%d  scroll %d,%d\n", b.Seq, b.At.Local().Format("2006-01-02 15:04:05"),
			b.Path, b.Viewport.W, b.Viewport.H, b.Scroll.X, b.Scroll.Y)
		if b.Note != "" {
			fmt.Fprintf(stdout, "  note (operator): %s\n", b.Note)
		}
		for i, m := range b.Marks {
			fmt.Fprintf(stdout, "  %d. %s %s", i+1, m.Kind, pageMarkWhere(m))
			if m.Text != "" {
				fmt.Fprintf(stdout, " %q", m.Text)
			}
			fmt.Fprintln(stdout)
			for _, a := range m.Anchors {
				fmt.Fprintf(stdout, "       %s", a.Path)
				if a.Text != "" {
					fmt.Fprintf(stdout, "  %q", a.Text)
				}
				fmt.Fprintln(stdout)
			}
		}
	}
	return rcOK
}

// pageMarkWhere says where a mark is, in page pixels.
func pageMarkWhere(m pageMarkWire) string {
	if len(m.Points) == 0 {
		return ""
	}
	if m.Kind == "box" && len(m.Points) == 2 {
		a, b := m.Points[0], m.Points[1]
		return fmt.Sprintf("at %d,%d to %d,%d", a[0], a[1], b[0], b[1])
	}
	a, b := m.Points[0], m.Points[len(m.Points)-1]
	if len(m.Points) == 1 {
		return fmt.Sprintf("at %d,%d", a[0], a[1])
	}
	return fmt.Sprintf("from %d,%d to %d,%d (%d points)", a[0], a[1], b[0], b[1], len(m.Points))
}

func runPageSnapshot(p *pageSnapshotParams, name string, stdout, stderr io.Writer) int {
	if strings.TrimSpace(p.Out) == "" {
		base := name
		if _, n, ok := strings.Cut(name, "/"); ok {
			base = n
		}
		p.Out = base + ".png"
	}
	var resp struct {
		PNG      string `json:"png"`
		Width    int    `json:"width"`
		Height   int    `json:"height"`
		ScrollX  int    `json:"scroll_x"`
		ScrollY  int    `json:"scroll_y"`
		Path     string `json:"path"`
		MarksSeq int64  `json:"marks_seq,omitempty"`
	}
	body := map[string]any{"seq": p.Seq, "no_marks": p.NoMarks}
	if err := DaemonRequest(http.MethodPost, pageAPIPath(name, "/snapshot", nil), body, &resp, DaemonOpts{Timeout: 60 * time.Second}); err != nil {
		return boardErr(stderr, err)
	}
	png, err := base64.StdEncoding.DecodeString(resp.PNG)
	if err != nil {
		fmt.Fprintf(stderr, "error: the daemon sent an unreadable image: %v\n", err)
		return rcIOFailure
	}
	if err := os.WriteFile(p.Out, png, 0o600); err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return rcIOFailure
	}
	resp.PNG = ""
	if p.JSON {
		return writeBoardJSON(stdout, map[string]any{"file": p.Out, "width": resp.Width, "height": resp.Height,
			"scroll_x": resp.ScrollX, "scroll_y": resp.ScrollY, "path": resp.Path, "marks_seq": resp.MarksSeq})
	}
	marks := "no marks"
	if resp.MarksSeq > 0 {
		marks = fmt.Sprintf("marks of batch %d", resp.MarksSeq)
	}
	fmt.Fprintf(stdout, "Wrote %s: %s at %dx%d, scrolled to %d,%d, %s.\n", p.Out, resp.Path, resp.Width, resp.Height, resp.ScrollX, resp.ScrollY, marks)
	return rcOK
}

func runPageDOM(p *pageDOMParams, name, selector string, stdout, stderr io.Writer) int {
	var ops []string
	if p.setText {
		ops = append(ops, "set-text")
	}
	if p.setAttr || p.SetAttr != "" {
		ops = append(ops, "set-attr")
	}
	if p.Remove {
		ops = append(ops, "remove")
	}
	if p.insertHTML || p.InsertHTML != "" {
		ops = append(ops, "insert-html")
	}
	if len(ops) > 1 || (len(ops) == 1 && (p.HTML || p.Styles || p.Text)) {
		fmt.Fprintln(stderr, "error: a DOM patch is one of --set-text, --set-attr, --remove and --insert-html, and reads nothing")
		return rcInvalidArg
	}
	if len(ops) == 1 {
		body := map[string]any{"kind": "dom", "selector": selector, "op": ops[0]}
		switch ops[0] {
		case "set-text":
			body["value"] = p.SetText
		case "set-attr":
			attr, value, ok := strings.Cut(p.SetAttr, "=")
			if !ok || attr == "" {
				fmt.Fprintln(stderr, "error: --set-attr takes NAME=VALUE")
				return rcInvalidArg
			}
			body["name"], body["value"] = attr, value
		case "insert-html":
			body["value"] = p.InsertHTML
		}
		return runPagePatch(&pagePatchParams{JSON: p.JSON}, name, body, stdout, stderr)
	}
	mode := "text"
	switch {
	case p.HTML && p.Styles, (p.HTML || p.Styles) && p.Text:
		fmt.Fprintln(stderr, "error: choose one of --text, --html and --styles")
		return rcInvalidArg
	case p.HTML:
		mode = "html"
	case p.Styles:
		mode = "styles"
	}
	var resp struct {
		Count int `json:"count"`
		Items []struct {
			Path  string                   `json:"path"`
			Rect  struct{ X, Y, W, H int } `json:"rect"`
			Value string                   `json:"value"`
		} `json:"items"`
		Truncated bool   `json:"truncated,omitempty"`
		Error     string `json:"error,omitempty"`
	}
	body := map[string]any{"selector": selector, "mode": mode}
	if err := DaemonRequest(http.MethodPost, pageAPIPath(name, "/dom", nil), body, &resp, DaemonOpts{Timeout: 20 * time.Second}); err != nil {
		return boardErr(stderr, err)
	}
	if p.JSON {
		return writeBoardJSON(stdout, resp)
	}
	if resp.Error != "" {
		fmt.Fprintf(stderr, "error: the page: %s\n", resp.Error)
		return rcIOFailure
	}
	if resp.Count == 0 {
		fmt.Fprintf(stdout, "Nothing matches %s.\n", selector)
		return rcOK
	}
	fmt.Fprintf(stdout, "%d match", resp.Count)
	if resp.Count != 1 {
		fmt.Fprint(stdout, "es")
	}
	if resp.Count > len(resp.Items) || resp.Truncated {
		fmt.Fprintf(stdout, " (the first %d shown; narrow the selector for more)", len(resp.Items))
	}
	fmt.Fprintln(stdout, ".")
	for _, it := range resp.Items {
		fmt.Fprintf(stdout, "\n%s  at %d,%d size %dx%d\n", it.Path, it.Rect.X, it.Rect.Y, it.Rect.W, it.Rect.H)
		for _, line := range strings.Split(strings.TrimRight(it.Value, "\n"), "\n") {
			fmt.Fprintf(stdout, "  %s\n", line)
		}
	}
	return rcOK
}

type pagePatchResultWire struct {
	Applied int    `json:"applied"`
	State   string `json:"state,omitempty"`
	Error   string `json:"error,omitempty"`
}

// runPagePatch adds one live patch and says where it applied.
func runPagePatch(p *pagePatchParams, name string, body map[string]any, stdout, stderr io.Writer) int {
	var resp struct {
		Page     string                `json:"page"`
		Patch    int64                 `json:"patch"`
		Views    int                   `json:"views"`
		Results  []pagePatchResultWire `json:"results,omitempty"`
		Withheld bool                  `json:"withheld,omitempty"`
	}
	if err := DaemonRequest(http.MethodPost, pageAPIPath(name, "/patches", nil), body, &resp, DaemonOpts{Timeout: 20 * time.Second}); err != nil {
		return boardErr(stderr, err)
	}
	if p.JSON {
		return writeBoardJSON(stdout, resp)
	}
	fmt.Fprintf(stdout, "Live patch #%d on %s: %s.\n", resp.Patch, resp.Page, pagePatchWhere(resp.Views, resp.Results, resp.Withheld))
	fmt.Fprintln(stdout, "It is not in your source: port what the operator approves, then `tclaude agent page patches "+resp.Page+" --reset`.")
	for _, r := range resp.Results {
		if r.Error != "" {
			return rcIOFailure
		}
	}
	return rcOK
}

func pagePatchWhere(views int, results []pagePatchResultWire, withheld bool) string {
	if views == 0 {
		return "not open (no dashboard view of the page is open; it applies when the operator opens it)"
	}
	plural := func(n int, one, many string) string {
		if n == 1 {
			return fmt.Sprintf("%d %s", n, one)
		}
		return fmt.Sprintf("%d %s", n, many)
	}
	if withheld {
		return "sent to " + plural(views, "open view", "open views") + " (this static page is not read back, so what it did is not shown)"
	}
	var parts []string
	for _, r := range results {
		switch {
		case r.Error != "":
			parts = append(parts, "failed: "+r.Error)
		case r.State == "loading":
			parts = append(parts, "the page is loading; it applies when it has loaded")
		case r.State == "without":
			parts = append(parts, "the operator is viewing the page without live patches")
		default:
			parts = append(parts, "applied to "+plural(r.Applied, "element", "elements"))
		}
	}
	return "in " + plural(views, "open view", "open views") + ": " + strings.Join(parts, "; ")
}

func runPageEval(p *pagePatchParams, name, js string, stdin io.Reader, stdout, stderr io.Writer) int {
	if js == "-" {
		data, err := io.ReadAll(io.LimitReader(stdin, 64<<10))
		if err != nil {
			fmt.Fprintf(stderr, "error: reading the script: %v\n", err)
			return rcIOFailure
		}
		js = string(data)
	}
	var resp struct {
		Page  string          `json:"page"`
		Patch int64           `json:"patch"`
		JSON  json.RawMessage `json:"json"`
		Error string          `json:"error,omitempty"`
	}
	if err := DaemonRequest(http.MethodPost, pageAPIPath(name, "/eval", nil), map[string]any{"js": js}, &resp, DaemonOpts{Timeout: 20 * time.Second}); err != nil {
		return boardErr(stderr, err)
	}
	if p.JSON {
		if rc := writeBoardJSON(stdout, resp); rc != rcOK || resp.Error == "" {
			return rc
		}
		return rcIOFailure
	}
	if resp.Error != "" {
		fmt.Fprintf(stderr, "error: the page (eval #%d): %s\n", resp.Patch, resp.Error)
		return rcIOFailure
	}
	fmt.Fprintln(stdout, string(resp.JSON))
	return rcOK
}

type pagePatchWire struct {
	N        int64     `json:"n"`
	Kind     string    `json:"kind"`
	At       time.Time `json:"at"`
	Replay   bool      `json:"replay"`
	CSS      string    `json:"css,omitempty"`
	Selector string    `json:"selector,omitempty"`
	Op       string    `json:"op,omitempty"`
	Name     string    `json:"name,omitempty"`
	Value    string    `json:"value,omitempty"`
	JS       string    `json:"js,omitempty"`
}

func runPagePatches(p *pagePatchesParams, args []string, stdout, stderr io.Writer) int {
	name := args[0]
	if len(args) == 2 && !p.Reset {
		fmt.Fprintln(stderr, "error: a patch number goes with --reset")
		return rcInvalidArg
	}
	if p.Reset {
		q := url.Values{}
		if len(args) == 2 {
			n := strings.TrimPrefix(args[1], "#")
			if _, err := strconv.ParseInt(n, 10, 64); err != nil {
				fmt.Fprintln(stderr, "error: N is a patch number")
				return rcInvalidArg
			}
			q.Set("n", n)
		}
		path := pageAPIPath(name, "/patches", q)
		var resp struct {
			Page    string  `json:"page"`
			Dropped []int64 `json:"dropped"`
			Left    int     `json:"left"`
		}
		if err := DaemonRequest(http.MethodDelete, path, nil, &resp, DaemonOpts{}); err != nil {
			return boardErr(stderr, err)
		}
		if p.JSON {
			return writeBoardJSON(stdout, resp)
		}
		if len(resp.Dropped) == 0 {
			fmt.Fprintf(stdout, "%s has no live patches.\n", resp.Page)
			return rcOK
		}
		fmt.Fprintf(stdout, "Dropped %d live patch(es) from %s; %d left. Open views reload without them.\n", len(resp.Dropped), resp.Page, resp.Left)
		return rcOK
	}
	var resp struct {
		Page    string          `json:"page"`
		Seq     int64           `json:"seq"`
		Views   int             `json:"views"`
		Patches []pagePatchWire `json:"patches"`
	}
	if err := DaemonRequest(http.MethodGet, pageAPIPath(name, "/patches", nil), nil, &resp, DaemonOpts{}); err != nil {
		return boardErr(stderr, err)
	}
	if p.JSON {
		return writeBoardJSON(stdout, resp)
	}
	if len(resp.Patches) == 0 {
		fmt.Fprintf(stdout, "%s has no live patches: it shows its source.\n", resp.Page)
		return rcOK
	}
	tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "#\tKIND\tAT\tREPLAY\tPATCH")
	for _, pt := range resp.Patches {
		replay := "replayed"
		if !pt.Replay {
			replay = "not replayed"
		}
		fmt.Fprintf(tw, "#%d\t%s\t%s\t%s\t%s\n", pt.N, pt.Kind, pt.At.Local().Format("15:04:05"), replay, pagePatchSummary(pt))
	}
	_ = tw.Flush()
	return rcOK
}

// pagePatchSummary is one line of a patch, clipped.
func pagePatchSummary(p pagePatchWire) string {
	var s string
	switch p.Kind {
	case "css":
		s = p.CSS
	case "eval":
		s = p.JS
	default:
		s = p.Selector + " " + p.Op
		switch p.Op {
		case "set-attr":
			s += " " + p.Name + "=" + p.Value
		case "set-text", "insert-html":
			s += " " + p.Value
		}
	}
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 80 {
		s = string(r[:79]) + "…"
	}
	return s
}

// dashIfEmpty is s, or "-" for an empty table cell.
func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
