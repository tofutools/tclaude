// Package pafcmd implements `tclaude paf`: read-only tools that verify,
// describe and render Portable Agent Format files (and legacy agent
// bundles) without a daemon, harness, model or network.
package pafcmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/legacy"
	"github.com/tofutools/tclaude/pkg/paf/privacy"
	"github.com/tofutools/tclaude/pkg/paf/render"
	"github.com/tofutools/tclaude/pkg/pwf"
)

type Params struct{}

// Cmd returns the `paf` command group.
func Cmd() *cobra.Command {
	return boa.CmdT[Params]{
		Use:   "paf",
		Short: "Export, import, verify, inspect and render Portable Agent Format files",
		Long: "Export and import agents as Portable Agent Format (PAF) files, and verify,\n" +
			"inspect and render them. validate, inspect and render also read legacy agent\n" +
			"bundles; they run locally, need no daemon, harness, model or network, and\n" +
			"never change the file. See docs/paf.md.",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds:     []*cobra.Command{agent.PAFExportCmd(), agent.PAFImportCmd(), agent.PAFRevealCmd(), agent.PAFConsentCmd(), agent.PAFPublishCmd(), validateCmd(), inspectCmd(), renderCmd(), redactCmd(), encryptCmd(), decryptCmd()},
		RunFunc: func(_ *Params, cmd *cobra.Command, _ []string) {
			_ = cmd.Help()
		},
	}.ToCobra()
}

type validateParams struct {
	File string `descr:"PAF file or legacy agent bundle" positional:"true"`
	JSON bool   `long:"json" help:"Print the result as JSON."`
}

func validateCmd() *cobra.Command {
	return boa.CmdT[validateParams]{
		Use:   "validate",
		Short: "Verify a PAF's structure, schemas, hashes and signatures",
		Long: "Verify a PAF completely: ZIP layout, manifest, schemas, entry hashes and\n" +
			"sizes, semantic rules and signatures. Exits 1 when the file is invalid.\n" +
			"A file that requires features this tclaude does not understand is still\n" +
			"reported valid for reading, with a warning, because import would refuse it.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *validateParams, cmd *cobra.Command, _ []string) {
			os.Exit(runValidate(cmd.OutOrStdout(), p))
		},
	}.ToCobra()
}

type validateResult struct {
	Valid       bool                      `json:"valid"`
	Source      string                    `json:"source,omitempty"`
	Error       string                    `json:"error,omitempty"`
	Unsupported []string                  `json:"unsupported,omitempty"`
	Warnings    []string                  `json:"warnings,omitempty"`
	Signatures  []render.SignatureSummary `json:"signatures,omitempty"`
}

func runValidate(out io.Writer, p *validateParams) int {
	res := validateResult{}
	o, err := open(p.File)
	if err != nil {
		res.Error = err.Error()
	} else {
		defer func() { _ = o.Close() }()
		s := summarize(o)
		res.Valid, res.Source, res.Unsupported, res.Warnings, res.Signatures = true, o.Source, s.Unsupported, s.Warnings, s.Signatures
		for _, sig := range s.Signatures {
			if sig.Status == string(paf.SignatureInvalid) {
				res.Valid, res.Error = false, "signature "+sig.Path+" is invalid"
			}
		}
		if err := checkWorkflows(o.Archive); err != nil {
			res.Valid, res.Error = false, err.Error()
		}
	}
	if p.JSON {
		writeJSON(out, res)
	} else if res.Valid {
		fmt.Fprintf(out, "valid: %s (%s)\n", termSafe(p.File), sourceLabel(res.Source))
		if len(res.Unsupported) > 0 {
			fmt.Fprintf(out, "warning: requires features this tclaude does not understand: %s; it can be read but not imported\n", termSafe(strings.Join(res.Unsupported, ", ")))
		}
		for _, w := range res.Warnings {
			fmt.Fprintf(out, "warning: %s\n", termSafe(w))
		}
		switch {
		case len(res.Signatures) == 0:
			fmt.Fprintln(out, "unsigned")
		case !render.Verifiable(res.Signatures):
			fmt.Fprintln(out, "unsigned: no signature uses an algorithm this tclaude can verify")
		}
	} else {
		fmt.Fprintf(out, "invalid: %s: %s\n", termSafe(p.File), termSafe(res.Error))
	}
	if !p.JSON {
		for _, sig := range res.Signatures {
			fmt.Fprintf(out, "signature %s: %s\n", termSafe(sig.Path), termSafe(render.SignatureLine(sig)))
		}
	}
	if !res.Valid {
		return 1
	}
	return 0
}

// checkWorkflows runs a workflow reader's checks on what the file carries.
// pkg/paf only checks their hashes and links.
func checkWorkflows(a *paf.Archive) error {
	if len(a.Unsupported) > 0 || len(a.Warnings) > 0 {
		return nil
	}
	for _, wf := range a.Manifest.Workflows {
		if _, err := pwf.OpenEmbedded(a, wf.Key); err != nil {
			return err
		}
	}
	return nil
}

type inspectParams struct {
	File string `descr:"PAF file or legacy agent bundle" positional:"true"`
	JSON bool   `long:"json" help:"Print the summary as JSON."`
	Scan bool   `long:"scan" help:"Also scan the whole content for suspected credentials on this node, rather than trusting the producer's recorded scan."`
}

func inspectCmd() *cobra.Command {
	return boa.CmdT[inspectParams]{
		Use:   "inspect",
		Short: "Verify a PAF and describe it without showing the conversation",
		Long: "Verify a PAF, then show what it holds: format and producer, signatures,\n" +
			"extensions, privacy findings, and for each agent its harness, model,\n" +
			"working directory, native history, neutral context and recorded losses.\n" +
			"Conversation content is not shown; use `tclaude paf render` for that.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *inspectParams, cmd *cobra.Command, _ []string) {
			if err := runInspect(cmd.OutOrStdout(), p); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runInspect(out io.Writer, p *inspectParams) error {
	o, err := open(p.File)
	if err != nil {
		return err
	}
	defer func() { _ = o.Close() }()
	s := summarize(o)
	if p.Scan {
		findings, err := privacy.ScanArchive(o.Archive, harness.MaxHistoryRecordBytes)
		if err != nil {
			return err
		}
		s.Rescan = findings
	}
	if p.JSON {
		writeJSON(out, s)
		return nil
	}
	fmt.Fprintf(out, "%s (%s)\n", termSafe(p.File), sourceLabel(s.Source))
	if render.SignatureInvalid(s.Signatures) {
		fmt.Fprintln(out, "  warning: a signature is invalid; the manifest may have been changed after it was signed")
	}
	for _, f := range render.ArchiveFacts(s) {
		fmt.Fprintf(out, "  %-19s %s\n", f[0]+":", termSafe(f[1]))
	}
	if p.Scan {
		if len(s.Rescan) == 0 {
			fmt.Fprintf(out, "  %-19s %s\n", "Scan (this node):", "no suspected credentials")
		}
		for _, f := range s.Rescan {
			fmt.Fprintf(out, "  %-19s %s\n", "Scan (this node):", termSafe(render.FindingLine(f)))
		}
	}
	for _, wf := range s.Workflows {
		fmt.Fprintf(out, "\nworkflow %s (%s)\n", termSafe(wf.Root), termSafe(wf.Key))
		for _, role := range slices.Sorted(maps.Keys(wf.Performers)) {
			fmt.Fprintf(out, "  %-19s %s\n", "Performer:", termSafe(role+": agent "+wf.Performers[role]))
		}
		for _, key := range wf.CarriedBy {
			fmt.Fprintf(out, "  %-19s %s\n", "Carried by:", termSafe(key))
		}
		fmt.Fprintf(out, "  %-19s %s\n", "Review:", "tclaude agent process-templates inspect "+termSafe(p.File)+" --workflow "+termSafe(wf.Key))
	}
	for _, ag := range s.Agents {
		fmt.Fprintf(out, "\nagent %s (%s)\n", termSafe(ag.Name), termSafe(ag.Key))
		for _, f := range render.AgentFacts(ag) {
			fmt.Fprintf(out, "  %-19s %s\n", f[0]+":", termSafe(f[1]))
		}
		if ag.LandingRung != "" {
			fmt.Fprintf(out, "  %-19s %s\n", "Landing rung:", termSafe(ag.LandingRung))
		}
		for _, l := range ag.Losses {
			fmt.Fprintf(out, "  %-19s %s\n", "Loss:", termSafe(render.LossLine(l)))
		}
		if ag.PriorReports > 0 {
			fmt.Fprintf(out, "  %-19s %d\n", "Earlier reports:", ag.PriorReports)
		}
	}
	return nil
}

type renderParams struct {
	File   string `descr:"PAF file or legacy agent bundle" positional:"true"`
	Out    string `long:"out" short:"o" optional:"true" help:"Write to this file instead of stdout. A .html name selects HTML."`
	Format string `long:"format" optional:"true" help:"md or html. Defaults to html for a .html --out, otherwise md."`
	Scope  string `long:"scope" optional:"true" help:"Which context to show: full or effective. Defaults to full when present, then effective, then the decoded native history."`
}

func renderCmd() *cobra.Command {
	return boa.CmdT[renderParams]{
		Use:   "render",
		Short: "Render a PAF as a readable Markdown or HTML document",
		Long: "Verify a PAF and render it for reading: the archive facts, each agent's\n" +
			"definition, environment and losses, and its conversation. This is the\n" +
			"archive landing rung: it needs no harness, model or network. HTML output\n" +
			"is one self-contained file that loads nothing and runs no script.\n\n" +
			"  tclaude paf render reviewer.paf > reviewer.md\n" +
			"  tclaude paf render reviewer.paf --out reviewer.html",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *renderParams, cmd *cobra.Command, _ []string) {
			if err := runRender(cmd.OutOrStdout(), p); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runRender(stdout io.Writer, p *renderParams) error {
	format := p.Format
	if format == "" {
		format = "md"
		if ext := strings.ToLower(filepath.Ext(p.Out)); ext == ".html" || ext == ".htm" {
			format = "html"
		}
	}
	if format != "md" && format != "html" {
		return fmt.Errorf("--format must be md or html, not %q", format)
	}
	if p.Scope != "" && p.Scope != paf.ScopeFull && p.Scope != paf.ScopeEffective {
		return fmt.Errorf("--scope must be full or effective, not %q", p.Scope)
	}
	o, err := open(p.File)
	if err != nil {
		return err
	}
	defer func() { _ = o.Close() }()
	out, finish := stdout, func() error { return nil }
	if p.Out != "" {
		// The archive is read lazily from its file, so writing over it
		// would destroy the input mid-render.
		if in, err := os.Stat(p.File); err == nil {
			if dst, err := os.Stat(p.Out); err == nil && os.SameFile(in, dst) {
				return errors.New("--out is the input file")
			}
		}
		f, err := os.OpenFile(p.Out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		out, finish = f, f.Close
	}
	opts := render.Options{Scope: p.Scope, Native: decodeNative}
	if format == "html" {
		err = render.HTML(out, o.Archive, o.Source, opts)
	} else {
		err = render.Markdown(out, o.Archive, o.Source, opts)
	}
	return errors.Join(err, finish())
}

// open verifies a PAF, an encrypted PAF addressed to this node, or a
// converted legacy agent bundle, for reading. Unknown required features degrade instead of refusing.
func open(path string) (*legacy.Opened, error) {
	if path == "" {
		return nil, errors.New("a file is required")
	}
	return openMaybeEncrypted(path, paf.Options{Purpose: paf.PurposeInspect})
}

// summarize describes the archive and says who holds each validly signing
// key: this node, a trusted federation peer, or nobody this node knows.
// The key itself is compared, never the key id the file claims.
func summarize(o *legacy.Opened) render.Summary {
	s := render.Summarize(o.Archive, o.Source)
	var self string
	if id, err := proto.LoadIdentity(filepath.Join(config.DataDir(), "federation", "instance.key")); err == nil {
		self = id.ID()
	}
	// Peer lookup reads an existing database only; a read-only command
	// never creates one.
	var peers []db.FederationPeer
	if _, err := os.Stat(db.DBPath()); err == nil {
		peers, _ = db.ListFederationPeers()
	}
	for i, r := range o.Signatures {
		if r.Status != paf.SignatureValid || r.Signature.Alg != paf.AlgEd25519 {
			continue
		}
		pub := r.Signature.PublicKey
		id := proto.InstanceID(pub)
		s.Signatures[i].Trust = "unknown key"
		if id == self {
			s.Signatures[i].Trust = "this node"
			continue
		}
		for _, p := range peers {
			if p.InstanceID == id && bytes.Equal(p.PubKey, pub) {
				name := p.Label
				if name == "" {
					name = p.Name
				}
				s.Signatures[i].Trust = strings.TrimSpace("known peer " + name)
			}
		}
	}
	s.VouchConsent()
	return s
}

func producer() paf.Producer {
	return paf.Producer{Name: "tclaude", Version: buildversion.AppVersion()}
}

// decodeNative reads a native history through its harness's transcript
// codec. The codec is plain Go; the harness need not be installed.
func decodeNative(n paf.Native, r io.Reader) (*transcript.Transcript, error) {
	if n.Harness == "" {
		return nil, fmt.Errorf("native %s history names no harness", n.Format)
	}
	h, err := harness.Resolve(n.Harness)
	if err != nil {
		return nil, err
	}
	if h.Transcript == nil {
		return nil, fmt.Errorf("tclaude cannot read %s native history", h.Name)
	}
	return h.Transcript.Decode(r, n.SourceConvID)
}

// termSafe keeps archive text from controlling the terminal: control
// characters (including ESC and C1) and bidirectional overrides become
// U+FFFD, so a value prints on its own line exactly as it reads.
func termSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) || r == 0x2028 || r == 0x2029 {
			return '\uFFFD'
		}
		return r
	}, s)
}

func sourceLabel(source string) string {
	if source == paf.Format {
		return "PAF"
	}
	return source + ", read as PAF"
}

func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
