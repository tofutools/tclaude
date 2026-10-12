package agent

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
)

// boardSnapshotTimeout bounds a board snapshot transfer through the daemon.
const boardSnapshotTimeout = 10 * time.Minute

type snapshotShareParams struct {
	Snapshot      string   `pos:"true" optional:"true" help:"Library snapshot to share (or use --file)."`
	File          string   `long:"file" optional:"true" help:"Share this PAF file instead, for example a redacted export."`
	Board         string   `long:"board" help:"Board ID."`
	To            []string `long:"to" help:"A board member to share with: instance id or trusted peer label. Repeatable."`
	Expires       string   `long:"expires" optional:"true" help:"Expiry: a duration (72h, 7d) or an RFC 3339 time; default 7d, at most 30d."`
	Name          string   `long:"name" optional:"true" help:"Name shown on the board. Defaults to the snapshot name."`
	ConfirmDigest string   `long:"confirm-digest" optional:"true" help:"Publish without the prompt: the content digest shown by a previous run, confirming you reviewed exactly this content."`
}

func snapshotShareCmd() *cobra.Command {
	return boa.CmdT[snapshotShareParams]{Use: "share [<name>]", Short: "Share a snapshot on a Board with named members (human only)",
		Long: "Publish a snapshot to a Board, encrypted to the named members only (and this node). Other members see that\n" +
			"it exists but cannot open it. The Board hub deletes it at expiry: 7 days by default, at most 30. Members who\n" +
			"fetched it keep their copy.\n\n" +
			"You review what would be shared (size, turns, time range, credential scan, redactions, recipients, expiry)\n" +
			"and type 'share' to confirm; the confirmation is recorded in the PAF. A snapshot with suspected credentials\n" +
			"is refused: export it, run tclaude paf redact, and share the file with --file.\n\n" +
			"  tclaude agent snapshot share before-refactor --board BOARD --to lab --expires 3d",
		ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *snapshotShareParams, _ *cobra.Command, _ []string) {
			os.Exit(runSnapshotShare(p, os.Stdin, os.Stdout, os.Stderr, term.IsTerminal(int(os.Stdin.Fd()))))
		}}.ToCobra()
}

func runSnapshotShare(p *snapshotShareParams, stdin io.Reader, stdout, stderr io.Writer, interactive bool) int {
	if (p.Snapshot == "") == (p.File == "") {
		fmt.Fprintln(stderr, "Error: name a library snapshot or pass --file, not both")
		return rcInvalidArg
	}
	var body []byte
	if p.File != "" {
		raw, err := os.ReadFile(p.File)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return rcInvalidArg
		}
		body = raw
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	q := url.Values{"to": p.To}
	for k, v := range map[string]string{"snapshot": p.Snapshot, "expires": p.Expires, "name": p.Name} {
		if v != "" {
			q.Set(k, v)
		}
	}
	path := "/v1/federation/boards/" + url.PathEscape(p.Board) + "/snapshots?"
	var preview boardSnapshotShare
	if err := DaemonPostRaw(path+q.Encode(), paf.MediaType, body, &preview); err != nil {
		fmt.Fprintln(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	printBoardSnapshotPreview(stdout, preview)
	digest := p.ConfirmDigest
	if digest == "" {
		if !interactive {
			fmt.Fprintln(stderr, "not a terminal: review the summary and pass --confirm-digest "+preview.Summary.Digest)
			return rcInvalidArg
		}
		fmt.Fprint(stdout, "\nType 'share' to publish: ")
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		if strings.TrimSpace(answer) != "share" {
			fmt.Fprintln(stdout, "Nothing published.")
			return rcOK
		}
		digest = preview.Summary.Digest
	}
	q.Set("content_digest", digest)
	var done boardSnapshotShare
	if err := DaemonPostRaw(path+q.Encode(), paf.MediaType, body, &done); err != nil {
		fmt.Fprintln(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Shared %s on board %s: item %s version %s, expires %s\n", consentSafe(done.Name), done.Board, done.Item, done.Version, done.ExpiresAt.Local().Format(time.RFC3339))
	return rcOK
}

type boardSnapshotShare struct {
	Board      string                  `json:"board"`
	Name       string                  `json:"name"`
	Recipients []string                `json:"recipients"`
	ExpiresAt  time.Time               `json:"expires_at"`
	Summary    *privacy.ConsentSummary `json:"summary"`
	Item       string                  `json:"item"`
	Version    string                  `json:"version"`
}

func printBoardSnapshotPreview(out io.Writer, s boardSnapshotShare) {
	if s.Summary == nil {
		s.Summary = &privacy.ConsentSummary{}
	}
	printConsentSummary(out, &pafConsentParams{File: s.Name, Audience: "board " + s.Board + " members", Recipient: s.Recipients}, s.Summary)
	fmt.Fprintf(out, "  %-12s %s (the hub deletes it then; members who fetched it keep their copy)\n", "Expires:", s.ExpiresAt.Local().Format(time.RFC3339))
	fmt.Fprintf(out, "  %-12s %s\n", "Encryption:", "only the recipients above can open it; this node is always one")
}

type snapshotBoardParams struct {
	Action       string   `pos:"true" help:"ls, fetch, preview, import or rm"`
	Board        string   `long:"board" help:"Board ID."`
	Item         string   `long:"item" optional:"true" help:"Snapshot item ID."`
	Version      string   `long:"version" optional:"true" help:"Snapshot version ID."`
	Group        string   `long:"group" optional:"true" help:"preview/import: the receiving group."`
	AgentName    string   `long:"agent-name" optional:"true" help:"preview/import: name for the new agent."`
	Cwd          string   `long:"cwd" optional:"true" help:"preview/import: working directory; without it the recorded one is kept."`
	Harness      string   `long:"harness" optional:"true" help:"preview/import: landing harness."`
	Model        string   `long:"model" optional:"true" help:"preview/import: landing model."`
	PreviewToken string   `long:"preview-token" optional:"true" help:"import: the token from a matching preview."`
	Set          []string `long:"set" optional:"true" help:"preview/import: placeholder name=value."`
	Cursor       string   `long:"cursor" optional:"true" help:"ls: page cursor."`
	Local        bool     `long:"local" help:"rm: only remove this node's fetched copy, not the snapshot on the board."`
	JSON         bool     `long:"json" help:"Print raw JSON."`
}

func snapshotBoardCmd() *cobra.Command {
	return boa.CmdT[snapshotBoardParams]{Use: "board <ls|fetch|preview|import|rm>", Short: "Receive or remove snapshots shared on a Board (human only)",
		Long: "ls lists a board's snapshots and which are addressed to this node. fetch downloads, decrypts, validates and\n" +
			"rescans one; it never imports. preview shows the agent it would create and prints a preview token; import\n" +
			"needs that token with the same options and creates a new agent, briefed that the received history is\n" +
			"untrusted. rm deletes a snapshot from the board (its publisher or a board owner) and this node's fetched\n" +
			"copy; rm --local removes only the fetched copy. Fetched copies are also removed when the snapshot expires.",
		ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *snapshotBoardParams, _ *cobra.Command, _ []string) {
			os.Exit(runSnapshotBoard(p, os.Stdout, os.Stderr))
		}}.ToCobra()
}

func runSnapshotBoard(p *snapshotBoardParams, stdout, stderr io.Writer) int {
	base := "/v1/federation/boards/" + url.PathEscape(p.Board) + "/snapshots"
	ref := base + "/" + url.PathEscape(p.Item) + "/" + url.PathEscape(p.Version)
	needRef := func() bool {
		if p.Item == "" || p.Version == "" {
			fmt.Fprintln(stderr, "Error: --item and --version are required")
			return false
		}
		return true
	}
	var out map[string]any
	var err error
	opts := DaemonOpts{Timeout: boardSnapshotTimeout}
	switch p.Action {
	case "ls":
		path := base
		if p.Cursor != "" {
			path += "?cursor=" + url.QueryEscape(p.Cursor)
		}
		if rc := RequireDaemonOrExit(stderr); rc != rcOK {
			return rc
		}
		var page struct {
			Snapshots []struct {
				Item       string    `json:"item"`
				Version    string    `json:"version"`
				Name       string    `json:"name"`
				Publisher  string    `json:"publisher"`
				ForMe      bool      `json:"for_me"`
				Fetched    bool      `json:"fetched"`
				ExpiresAt  time.Time `json:"expires_at"`
				Bytes      int64     `json:"bytes"`
				Invalid    bool      `json:"invalid"`
				Recipients []string  `json:"recipients"`
			} `json:"snapshots"`
			Cursor string `json:"next_cursor"`
		}
		if err = DaemonGet(path, &page); err != nil {
			break
		}
		if p.JSON {
			return printJSON(stdout, page)
		}
		if len(page.Snapshots) == 0 {
			fmt.Fprintln(stdout, "No snapshots.")
			return rcOK
		}
		tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tITEM\tVERSION\tPUBLISHER\tFOR YOU\tSIZE\tEXPIRES")
		for _, s := range page.Snapshots {
			if s.Invalid {
				fmt.Fprintf(tw, "(invalid)\t%s\t%s\t\t\t\t\n", s.Item, s.Version)
				continue
			}
			forMe := "no"
			if s.ForMe {
				forMe = "yes"
				if s.Fetched {
					forMe = "yes (fetched)"
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", termClean(s.Name), s.Item, s.Version, s.Publisher, forMe, humanSize(s.Bytes), s.ExpiresAt.Local().Format("2006-01-02 15:04"))
		}
		_ = tw.Flush()
		if page.Cursor != "" {
			fmt.Fprintln(stdout, "More: --cursor", page.Cursor)
		}
		return rcOK
	case "fetch":
		if !needRef() {
			return rcInvalidArg
		}
		if rc := RequireDaemonOrExit(stderr); rc != rcOK {
			return rc
		}
		err = DaemonRequest(http.MethodPost, ref+"/fetch", map[string]any{}, &out, opts)
	case "preview", "import":
		if !needRef() {
			return rcInvalidArg
		}
		if p.Group == "" {
			fmt.Fprintln(stderr, "Error: --group is required: the receiving group")
			return rcInvalidArg
		}
		if p.Action == "import" && p.PreviewToken == "" {
			fmt.Fprintln(stderr, "Error: --preview-token from a matching preview is required")
			return rcInvalidArg
		}
		body := map[string]any{"group": p.Group, "name": p.AgentName, "harness": p.Harness, "model": p.Model, "set": p.Set, "preview_token": p.PreviewToken}
		if p.Cwd != "" {
			body["cwd"] = p.Cwd
		} else {
			body["keep_paths"] = true
		}
		if rc := RequireDaemonOrExit(stderr); rc != rcOK {
			return rc
		}
		err = DaemonRequest(http.MethodPost, ref+"/"+p.Action, body, &out, opts)
	case "rm":
		if !needRef() {
			return rcInvalidArg
		}
		if rc := RequireDaemonOrExit(stderr); rc != rcOK {
			return rc
		}
		path := ref
		if p.Local {
			path += "?local=true"
		}
		err = DaemonRequest(http.MethodDelete, path, nil, &out, opts)
	default:
		fmt.Fprintln(stderr, "Error: action must be ls, fetch, preview, import or rm")
		return rcInvalidArg
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	return printJSON(stdout, out)
}
