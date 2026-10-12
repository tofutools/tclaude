package pafcmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/legacy"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
	"github.com/tofutools/tclaude/pkg/paf/render"
)

type redactParams struct {
	File          string `descr:"PAF file or legacy agent bundle" positional:"true"`
	Out           string `long:"out" short:"o" optional:"true" help:"Write the redacted PAF here. Required unless --preview."`
	Preview       bool   `long:"preview" help:"Show what the redactions would change without writing a file."`
	MaskFlagged   bool   `long:"mask-flagged" help:"Replace suspected credentials the scan flags with [REDACTED:<kind>]."`
	StubTools     bool   `long:"stub-tool-results" help:"Replace tool results older than the last --keep-turns human turns with a stub."`
	KeepTurns     int    `long:"keep-turns" default:"3" help:"Human turns whose tool results --stub-tool-results keeps."`
	DropMedia     bool   `long:"drop-media" help:"Replace images and documents with a stub."`
	DropReasoning bool   `long:"drop-reasoning" help:"Remove thinking blocks and provider reasoning state."`
	DropNative    bool   `long:"drop-native" help:"Drop the native history blob (every redaction does)."`
	Paths         bool   `long:"paths-to-placeholders" help:"Replace the home and working directories and the user name with ${NAME} placeholders, without their values."`
	JSON          bool   `long:"json" help:"Print the report as JSON."`
}

func redactCmd() *cobra.Command {
	return boa.CmdT[redactParams]{
		Use:   "redact <file>",
		Short: "Write a redacted copy of a PAF, or preview the redactions",
		Long: "Write a redacted copy of a PAF. Each redaction applies to every copy of\n" +
			"the content: the contexts, text attachments, definition, environment,\n" +
			"group and tclaude extension data. Extension data tclaude does not\n" +
			"understand, and content it cannot rewrite safely, is dropped. The native\n" +
			"blob is never edited, so it is always dropped; an agent with only native\n" +
			"history gets a neutral context decoded from it first.\n\n" +
			"The copy is a new file: the redactions are recorded in privacy.json and the\n" +
			"loss report, the scan is redone on the output, it is signed by this node\n" +
			"when it has a federation identity, and earlier signatures and consent do\n" +
			"not carry over.\n\n" +
			"  tclaude paf redact reviewer.paf --mask-flagged --paths-to-placeholders --preview\n" +
			"  tclaude paf redact reviewer.paf --mask-flagged --stub-tool-results -o reviewer.redacted.paf",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *redactParams, cmd *cobra.Command, _ []string) {
			if err := runRedact(cmd.OutOrStdout(), p); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runRedact(stdout io.Writer, p *redactParams) error {
	if p.Preview == (p.Out != "") {
		return errors.New("give either --out or --preview")
	}
	if p.KeepTurns < 0 {
		return errors.New("--keep-turns must not be negative")
	}
	opts := privacy.RedactOptions{
		MaskFlagged: p.MaskFlagged, StubToolResults: p.StubTools, KeepTurns: p.KeepTurns,
		DropMedia: p.DropMedia, DropReasoning: p.DropReasoning, DropNative: p.DropNative,
		PathsToPlaceholders: p.Paths, Native: decodeNative,
	}
	if p.File == "" {
		return errors.New("a file is required")
	}
	// Rewriting needs every required feature, as import does.
	o, err := legacy.OpenFile(p.File, "", producer(), paf.Options{Purpose: paf.PurposeImport})
	if err != nil {
		return err
	}
	defer func() { _ = o.Close() }()

	var rep *privacy.RedactReport
	if p.Preview {
		rep, err = privacy.Redact(o.Archive, io.Discard, opts, producer())
		if err != nil {
			return err
		}
	} else {
		rep, err = writeRedacted(o.Archive, p.File, p.Out, opts)
		if err != nil {
			return err
		}
	}
	if p.JSON {
		writeJSON(stdout, rep)
		return nil
	}
	if p.Preview {
		fmt.Fprintln(stdout, "preview: no file written")
	} else {
		fmt.Fprintf(stdout, "wrote %s\n", termSafe(p.Out))
	}
	for _, r := range rep.Redactions {
		line := fmt.Sprintf("%s: %d", r.Kind, r.Count)
		if r.Detail != "" {
			line += " (" + r.Detail + ")"
		}
		fmt.Fprintf(stdout, "  %-12s %s\n", "Redaction:", termSafe(line))
	}
	if len(rep.Findings) == 0 {
		fmt.Fprintf(stdout, "  %-12s %s\n", "Scan:", "no suspected credentials remain")
	}
	for _, f := range rep.Findings {
		fmt.Fprintf(stdout, "  %-12s %s\n", "Scan:", termSafe(render.FindingLine(f)))
	}
	return nil
}

// writeRedacted writes the copy beside its destination and renames it into
// place, so a failed redaction leaves no partial file.
func writeRedacted(a *paf.Archive, in, out string, opts privacy.RedactOptions) (*privacy.RedactReport, error) {
	if st, err := os.Stat(in); err == nil {
		if dst, err := os.Stat(out); err == nil && os.SameFile(st, dst) {
			return nil, errors.New("--out is the input file")
		}
	}
	f, err := os.CreateTemp(filepath.Dir(out), ".paf-redact-")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	var keys []paf.SigningKey
	// The copy is signed only when this node already has a federation
	// identity; redacting never creates one.
	if id, err := proto.LoadIdentity(filepath.Join(config.DataDir(), "federation", "instance.key")); err == nil {
		keys = []paf.SigningKey{{Key: id.Priv, KeyID: id.ID(), Signer: &paf.Signer{Kind: "instance"}}}
	}
	rep, err := privacy.Redact(a, f, opts, producer(), keys...)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return nil, err
	}
	return rep, os.Rename(tmp, out)
}
