package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent knowledge` — the docs and source of the running build, for the
// companion Steward. Everything here reads; the daemon holds the copy.

const knowledgeNeeds = "Needs knowledge.read (granted to the companion Steward)."

func knowledgeCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:         "knowledge",
		Short:       "Read the docs and source of the running build",
		Long:        "A read-only knowledge base pinned to the build the daemon is running. The docs ship in the binary; the source is fetched once by commit. Each answer says how exact it is.\n\n" + knowledgeNeeds,
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			knowledgeStatusCmd(), knowledgeLsCmd(), knowledgeReadCmd(), knowledgeSearchCmd(),
		},
	}.ToCobra()
}

type knowledgeHeader struct {
	Label  string `json:"label"`
	State  string `json:"state"`
	Source struct {
		State  string `json:"state"`
		Commit string `json:"commit"`
		Files  int    `json:"files"`
		Detail string `json:"detail"`
	} `json:"source"`
}

// printKnowledgeHeader leads every answer with the pin, and with the state of
// the source when it is not there to answer.
func printKnowledgeHeader(w io.Writer, h *knowledgeHeader) {
	fmt.Fprintf(w, "[%s]\n", h.Label)
	if h.Source.State != "ready" && h.Source.Detail != "" {
		fmt.Fprintf(w, "[source %s: %s]\n", h.Source.State, h.Source.Detail)
	}
}

func printKnowledgeJSON(w io.Writer, raw json.RawMessage) int {
	_, _ = w.Write(raw)
	fmt.Fprintln(w)
	return rcOK
}

// knowledgeGet fetches path into out and keeps the raw answer for --json.
func knowledgeGet(path string, out any, stderr io.Writer) (json.RawMessage, int) {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return nil, rc
	}
	var raw json.RawMessage
	if err := DaemonGet(path, &raw); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return nil, MapDaemonErrorToRC(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return nil, rcIOFailure
	}
	return raw, rcOK
}

type knowledgeStatusParams struct {
	JSON bool `long:"json" help:"Output JSON"`
}

func knowledgeStatusCmd() *cobra.Command {
	return boa.CmdT[knowledgeStatusParams]{
		Use:         "status",
		Short:       "Show which build the knowledge base is pinned to and how exact it is",
		Long:        "Prints the running version and commit, whether the build is exact, dirty or unknown, and how the source stands. A fetch that has not happened is started.\n\n" + knowledgeNeeds,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *knowledgeStatusParams, _ *cobra.Command, _ []string) {
			os.Exit(runKnowledgeStatus(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runKnowledgeStatus(p *knowledgeStatusParams, stdout, stderr io.Writer) int {
	var resp struct {
		knowledgeHeader
		Build struct {
			Version string `json:"version"`
			State   string `json:"state"`
			Commit  string `json:"commit"`
		} `json:"build"`
		Pages int `json:"pages"`
	}
	raw, rc := knowledgeGet("/v1/knowledge/status", &resp, stderr)
	if rc != rcOK {
		return rc
	}
	if p.JSON {
		return printKnowledgeJSON(stdout, raw)
	}
	fmt.Fprintf(stdout, "Build:  %s", resp.Build.Version)
	if resp.Build.Commit != "" {
		fmt.Fprintf(stdout, " (%s)", resp.Build.Commit)
	}
	fmt.Fprintf(stdout, "\nState:  %s\nDocs:   %d pages, in the binary\n", resp.Label, resp.Pages)
	fmt.Fprintf(stdout, "Source: %s", resp.Source.State)
	if resp.Source.Files > 0 {
		fmt.Fprintf(stdout, ", %d files", resp.Source.Files)
	}
	if resp.Source.Detail != "" {
		fmt.Fprintf(stdout, " (%s)", resp.Source.Detail)
	}
	fmt.Fprintln(stdout)
	return rcOK
}

type knowledgeLsParams struct {
	Path string `pos:"true" optional:"true" help:"Directory to list, such as docs or pkg/claude/agentd"`
	JSON bool   `long:"json" help:"Output JSON"`
}

func knowledgeLsCmd() *cobra.Command {
	return boa.CmdT[knowledgeLsParams]{
		Use:         "ls",
		Short:       "List a directory of the knowledge base",
		Long:        "Lists what is directly under a directory. With no path, the top of the tree.\n\n" + knowledgeNeeds,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *knowledgeLsParams, _ *cobra.Command, _ []string) {
			os.Exit(runKnowledgeLs(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runKnowledgeLs(p *knowledgeLsParams, stdout, stderr io.Writer) int {
	var resp struct {
		knowledgeHeader
		Entries []struct {
			Path string `json:"path"`
			Dir  bool   `json:"dir"`
		} `json:"entries"`
	}
	raw, rc := knowledgeGet("/v1/knowledge/ls?path="+url.QueryEscape(p.Path), &resp, stderr)
	if rc != rcOK {
		return rc
	}
	if p.JSON {
		return printKnowledgeJSON(stdout, raw)
	}
	printKnowledgeHeader(stdout, &resp.knowledgeHeader)
	for _, entry := range resp.Entries {
		if entry.Dir {
			fmt.Fprintf(stdout, "%s/\n", entry.Path)
		} else {
			fmt.Fprintln(stdout, entry.Path)
		}
	}
	return rcOK
}

type knowledgeReadParams struct {
	Path  string `pos:"true" help:"File to read: companion.md, docs/companion.md#capture or pkg/claude/agentd/steward.go"`
	Lines string `long:"lines" optional:"true" help:"Only these lines: N or N-M"`
	JSON  bool   `long:"json" help:"Output JSON"`
}

func knowledgeReadCmd() *cobra.Command {
	return boa.CmdT[knowledgeReadParams]{
		Use:   "read",
		Short: "Read a page, a section of one, or a source file",
		Long: "Prints a file from the knowledge base. A bare page name means the docs, and #section reads one section, as a page's links do. " +
			"A docs page ends with the pages it links to and those linking to it, so you can walk from a topic to related pages. " +
			"A long file comes back in part, and says which lines to ask for next.\n\n" + knowledgeNeeds,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *knowledgeReadParams, _ *cobra.Command, _ []string) {
			os.Exit(runKnowledgeRead(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

type knowledgeLink struct {
	Path    string `json:"path"`
	Section string `json:"section"`
	Text    string `json:"text"`
}

func (l knowledgeLink) String() string {
	target := strings.TrimPrefix(l.Path, "docs/")
	if l.Section != "" {
		target += "#" + l.Section
	}
	if l.Text != "" {
		return target + " (" + l.Text + ")"
	}
	return target
}

func runKnowledgeRead(p *knowledgeReadParams, stdout, stderr io.Writer) int {
	var resp struct {
		knowledgeHeader
		Path       string          `json:"path"`
		Content    string          `json:"content"`
		FromLine   int             `json:"from_line"`
		ToLine     int             `json:"to_line"`
		TotalLines int             `json:"total_lines"`
		Truncated  bool            `json:"truncated"`
		Links      []knowledgeLink `json:"links"`
		LinkedFrom []knowledgeLink `json:"linked_from"`
	}
	query := url.Values{"path": {p.Path}}
	if p.Lines != "" {
		query.Set("lines", p.Lines)
	}
	raw, rc := knowledgeGet("/v1/knowledge/read?"+query.Encode(), &resp, stderr)
	if rc != rcOK {
		return rc
	}
	if p.JSON {
		return printKnowledgeJSON(stdout, raw)
	}
	printKnowledgeHeader(stdout, &resp.knowledgeHeader)
	fmt.Fprintf(stdout, "[%s, lines %d-%d of %d]\n\n%s", resp.Path, resp.FromLine, resp.ToLine, resp.TotalLines, resp.Content)
	if resp.Truncated {
		fmt.Fprintf(stdout, "\n[cut at line %d: continue with --lines %d-%d]\n", resp.ToLine, resp.ToLine+1, min(resp.ToLine+2000, resp.TotalLines))
	}
	if len(resp.Links) > 0 {
		fmt.Fprintln(stdout, "\nLinks to:")
		for _, link := range resp.Links {
			fmt.Fprintf(stdout, "  %s\n", link)
		}
	}
	if len(resp.LinkedFrom) > 0 {
		fmt.Fprintln(stdout, "\nLinked from:")
		for _, link := range resp.LinkedFrom {
			fmt.Fprintf(stdout, "  %s\n", link)
		}
	}
	return rcOK
}

type knowledgeSearchParams struct {
	Pattern    string `pos:"true" help:"Regular expression, in Go syntax"`
	Path       string `long:"path" optional:"true" help:"Only under this path, such as docs or pkg/claude/agentd"`
	IgnoreCase bool   `short:"i" long:"ignore-case" optional:"true" help:"Ignore case"`
	Limit      int    `long:"limit" optional:"true" help:"Most matches to show (default 100, at most 500)"`
	JSON       bool   `long:"json" help:"Output JSON"`
}

func knowledgeSearchCmd() *cobra.Command {
	return boa.CmdT[knowledgeSearchParams]{
		Use:         "search",
		Short:       "Search the docs, then the source",
		Long:        "Prints matching lines as path:line:text, docs first.\n\n" + knowledgeNeeds,
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *knowledgeSearchParams, _ *cobra.Command, _ []string) {
			os.Exit(runKnowledgeSearch(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runKnowledgeSearch(p *knowledgeSearchParams, stdout, stderr io.Writer) int {
	var resp struct {
		knowledgeHeader
		Matches []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Text string `json:"text"`
		} `json:"matches"`
		Truncated bool `json:"truncated"`
	}
	query := url.Values{"q": {p.Pattern}}
	if p.Path != "" {
		query.Set("path", p.Path)
	}
	if p.IgnoreCase {
		query.Set("i", "1")
	}
	if p.Limit > 0 {
		query.Set("limit", fmt.Sprint(p.Limit))
	}
	raw, rc := knowledgeGet("/v1/knowledge/search?"+query.Encode(), &resp, stderr)
	if rc != rcOK {
		return rc
	}
	if p.JSON {
		return printKnowledgeJSON(stdout, raw)
	}
	printKnowledgeHeader(stdout, &resp.knowledgeHeader)
	for _, match := range resp.Matches {
		fmt.Fprintf(stdout, "%s:%d:%s\n", match.Path, match.Line, match.Text)
	}
	if len(resp.Matches) == 0 {
		fmt.Fprintln(stdout, "No matches.")
	}
	if resp.Truncated {
		fmt.Fprintln(stdout, "[more matches: narrow it with --path, or raise --limit]")
	}
	return rcOK
}
