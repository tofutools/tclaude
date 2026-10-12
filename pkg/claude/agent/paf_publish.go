package agent

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
)

// publishFlags are the output and the optional redactions a publisher adds.
type publishFlags struct {
	Out             string
	StubToolResults bool
	KeepTurns       int
	DropMedia       bool
	DropReasoning   bool
	ConfirmDigest   string
}

const publishLong = "Publishing writes a PAF meant for anyone to read. It always masks suspected credentials, replaces\n" +
	"paths with placeholders, strips identity (agent, instance and conversation ids and titles, groups, media\n" +
	"paths) and drops the native history and extension data. You review the redactions and what remains, then\n" +
	"type 'publish'; your consent to a public audience is recorded and the file is signed with this node's\n" +
	"pseudonymous publisher key, not its federation key. The output is a file: tclaude hosts nothing. Only the\n" +
	"human operator can publish."

type pafPublishParams struct {
	File            string `pos:"true" help:"One-agent PAF to publish."`
	Out             string `long:"out" short:"o" help:"Write the published PAF here."`
	StubToolResults bool   `long:"stub-tool-results" help:"Replace tool results older than --keep-turns human turns with a stub."`
	KeepTurns       int    `long:"keep-turns" optional:"true" help:"With --stub-tool-results: keep the tool results of this many recent human turns."`
	DropMedia       bool   `long:"drop-media" help:"Replace images and documents with a stub."`
	DropReasoning   bool   `long:"drop-reasoning" help:"Remove thinking blocks and reasoning state."`
	ConfirmDigest   string `long:"confirm-digest" optional:"true" help:"Publish without the prompt: the content digest shown by a previous run, confirming you reviewed exactly this content."`
}

func (p *pafPublishParams) flags() *publishFlags {
	return &publishFlags{p.Out, p.StubToolResults, p.KeepTurns, p.DropMedia, p.DropReasoning, p.ConfirmDigest}
}

// PAFPublishCmd is `tclaude paf publish`.
func PAFPublishCmd() *cobra.Command {
	return boa.CmdT[pafPublishParams]{Use: "publish <file>", Short: "Write a redacted, pseudonymous PAF for public sharing (human only)",
		Long:        publishLong + "\n\n  tclaude paf publish reviewer.paf --stub-tool-results --keep-turns 5 -o reviewer.public.paf",
		ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *pafPublishParams, _ *cobra.Command, _ []string) {
			os.Exit(runPAFPublish(p.File, p.File, p.flags(), os.Stdin, os.Stdout, os.Stderr, term.IsTerminal(int(os.Stdin.Fd()))))
		}}.ToCobra()
}

type snapshotPublishParams struct {
	Name            string `pos:"true" help:"Library snapshot to publish."`
	Out             string `long:"out" short:"o" help:"Write the published PAF here."`
	StubToolResults bool   `long:"stub-tool-results" help:"Replace tool results older than --keep-turns human turns with a stub."`
	KeepTurns       int    `long:"keep-turns" optional:"true" help:"With --stub-tool-results: keep the tool results of this many recent human turns."`
	DropMedia       bool   `long:"drop-media" help:"Replace images and documents with a stub."`
	DropReasoning   bool   `long:"drop-reasoning" help:"Remove thinking blocks and reasoning state."`
	ConfirmDigest   string `long:"confirm-digest" optional:"true" help:"Publish without the prompt: the content digest shown by a previous run, confirming you reviewed exactly this content."`
}

func (p *snapshotPublishParams) flags() *publishFlags {
	return &publishFlags{p.Out, p.StubToolResults, p.KeepTurns, p.DropMedia, p.DropReasoning, p.ConfirmDigest}
}

func snapshotPublishCmd() *cobra.Command {
	return boa.CmdT[snapshotPublishParams]{Use: "publish <name>", Short: "Write a snapshot as a redacted, pseudonymous PAF for public sharing (human only)",
		Long:        publishLong + "\n\n  tclaude agent snapshot publish before-refactor --drop-reasoning -o reviewer.public.paf",
		ParamEnrich: common.DefaultParamEnricher(), RunFunc: func(p *snapshotPublishParams, _ *cobra.Command, _ []string) {
			os.Exit(runSnapshotPublish(p, os.Stdin, os.Stdout, os.Stderr, term.IsTerminal(int(os.Stdin.Fd()))))
		}}.ToCobra()
}

func runSnapshotPublish(p *snapshotPublishParams, stdin io.Reader, stdout, stderr io.Writer, interactive bool) int {
	if p.Out == "" {
		fmt.Fprintln(stderr, "--out is required")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	dir, err := os.MkdirTemp("", ".snapshot-publish-")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return rcIOFailure
	}
	defer func() { _ = os.RemoveAll(dir) }()
	src := filepath.Join(dir, "snapshot.paf")
	if err := downloadSnapshot(p.Name, src); err != nil {
		fmt.Fprintln(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	return runPAFPublish(src, p.Name, p.flags(), stdin, stdout, stderr, interactive)
}

func runPAFPublish(file, label string, p *publishFlags, stdin io.Reader, stdout, stderr io.Writer, interactive bool) int {
	if p.Out == "" {
		fmt.Fprintln(stderr, "--out is required")
		return rcInvalidArg
	}
	if p.KeepTurns != 0 && !p.StubToolResults {
		fmt.Fprintln(stderr, "--keep-turns needs --stub-tool-results")
		return rcInvalidArg
	}
	in, err := os.Open(file)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return rcInvalidArg
	}
	defer func() { _ = in.Close() }()
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	q := url.Values{}
	if p.StubToolResults {
		q.Set("stub_tool_results", "true")
		q.Set("keep_turns", strconv.Itoa(p.KeepTurns))
	}
	if p.DropMedia {
		q.Set("drop_media", "true")
	}
	if p.DropReasoning {
		q.Set("drop_reasoning", "true")
	}
	raw, err := io.ReadAll(in)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return rcIOFailure
	}
	var preview struct {
		Report  privacy.RedactReport   `json:"report"`
		Summary privacy.ConsentSummary `json:"summary"`
	}
	if err := DaemonPostRaw("/v1/paf/publish?"+q.Encode(), paf.MediaType, raw, &preview); err != nil {
		fmt.Fprintln(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	printPublishPreview(stdout, label, preview.Report, &preview.Summary)
	digest := p.ConfirmDigest
	if digest == "" {
		if !interactive {
			fmt.Fprintln(stderr, "not a terminal: review the summary and pass --confirm-digest "+preview.Summary.Digest)
			return rcInvalidArg
		}
		fmt.Fprint(stdout, "\nAnyone with the file can read all of the above. Type 'publish' to write it: ")
		answer, _ := bufio.NewReader(stdin).ReadString('\n')
		if strings.TrimSpace(answer) != "publish" {
			fmt.Fprintln(stdout, "Nothing published.")
			return rcOK
		}
		digest = preview.Summary.Digest
	}
	q.Set("content_digest", digest)
	published, err := exchangePAF("/v1/paf/publish?"+q.Encode(), in)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return MapDaemonErrorToRC(err)
	}
	if err := writeFileAtomic(p.Out, published); err != nil {
		fmt.Fprintln(stderr, err)
		return rcIOFailure
	}
	fmt.Fprintln(stdout, "Published", consentSafe(p.Out))
	return rcOK
}

func printPublishPreview(out io.Writer, label string, rep privacy.RedactReport, s *privacy.ConsentSummary) {
	s.Redactions = rep.Redactions
	printConsentSummary(out, &pafConsentParams{File: label, Audience: "public"}, s)
	if len(rep.Placeholders) > 0 {
		fmt.Fprintf(out, "  %-12s %s\n", "Placeholders:", consentSafe(strings.Join(rep.Placeholders, ", ")))
	}
	fmt.Fprintf(out, "  %-12s %s\n", "Signed by:", "this node's pseudonymous publisher key")
	fmt.Fprintf(out, "  %-12s %s\n", "Check:", "the conversation text itself is published as shown by tclaude paf render; names, hostnames, URLs and other details in it are not removed")
}
