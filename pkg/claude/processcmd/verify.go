package processcmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
	"github.com/tofutools/tclaude/pkg/common"
)

type verifyParams struct {
	RunID     string `pos:"true" help:"Process run id to verify"`
	StoreRoot string `long:"store-root" help:"Filesystem process store root"`
}

func verifyCmd() *cobra.Command {
	return boa.CmdT[verifyParams]{
		Use:         "verify <run>",
		Short:       "Verify a process run without modifying it",
		Long:        "Verify a process run's evidence integrity and semantic state invariants without modifying the run.",
		ParamEnrich: common.DefaultParamEnricher(),
		Args:        cobra.ExactArgs(1),
		PreExecuteFunc: func(p *verifyParams, _ *cobra.Command, _ []string) error {
			if err := requireProcessesEnabled(); err != nil {
				return err
			}
			if strings.TrimSpace(p.StoreRoot) == "" {
				return fmt.Errorf("--store-root is required")
			}
			return nil
		},
		RunFunc: func(p *verifyParams, cmd *cobra.Command, _ []string) {
			exitWithError(runVerify(cmd.Context(), p, os.Stdout))
		},
	}.ToCobra()
}

func runVerify(ctx context.Context, p *verifyParams, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	fs, err := store.NewFS(p.StoreRoot)
	if err != nil {
		return err
	}
	report := processverify.StoreRun(ctx, fs, p.RunID)
	renderReport(out, report)
	if report.HasErrors() {
		return fmt.Errorf("process run %q failed verification", p.RunID)
	}
	return nil
}

func renderReport(w io.Writer, report processverify.Report) {
	fmt.Fprintf(w, "Run: %s\n", report.RunID)
	if report.StoredStatus != "" {
		fmt.Fprintf(w, "Stored status: %s\n", report.StoredStatus)
	}
	fmt.Fprintf(w, "Effective status: %s\n", report.EffectiveStatus)
	if report.Dirty {
		fmt.Fprintln(w, "Dirty: yes (state has semantic violations while evidence anchors match; likely hand-edited state)")
	} else {
		fmt.Fprintln(w, "Dirty: no")
	}
	if len(report.Diagnostics) == 0 {
		fmt.Fprintln(w, "Diagnostics: none")
		return
	}
	fmt.Fprintln(w, "Diagnostics:")
	for _, layer := range []processverify.Layer{processverify.LayerLoad, processverify.LayerEvidence, processverify.LayerSemantic} {
		diagnostics := report.DiagnosticsForLayer(layer)
		if len(diagnostics) == 0 {
			continue
		}
		fmt.Fprintf(w, "  %s:\n", layer)
		for _, diag := range diagnostics {
			path := diag.Path
			if path == "" {
				path = "-"
			}
			severity := diag.Severity
			if severity == "" {
				severity = model.SeverityError
			}
			fmt.Fprintf(w, "    [%s] %s %s: %s\n", severity, diag.Code, path, diag.Message)
		}
	}
}
