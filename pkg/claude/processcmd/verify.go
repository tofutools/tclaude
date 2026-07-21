package processcmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
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
		Use:         "verify",
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
			exitWithError(runVerifyDispatch(cmd.Context(), p, os.Stdout))
		},
	}.ToCobra()
}

func runVerifyDispatch(ctx context.Context, p *verifyParams, out io.Writer) error {
	canonical := requireCanonicalProcessStore(p.StoreRoot) == nil
	kind, probeErr := localRunSchema(ctx, p.StoreRoot, p.RunID)
	if !canonical {
		if probeErr == nil && kind == store.RunSchemaEpochV8 {
			return requireCanonicalProcessStore(p.StoreRoot)
		}
		return runVerify(ctx, p, out)
	}
	if probeErr == nil && kind != store.RunSchemaEpochV8 {
		return runVerify(ctx, p, out)
	}
	var response struct {
		Verified bool `json:"verified"`
		View     struct {
			Run struct{ ID, EffectiveStatus string } `json:"run"`
		} `json:"view"`
	}
	if err := agent.DaemonRequest("GET", "/v1/process/runs/"+p.RunID+"/verify", nil, &response, agent.DaemonOpts{Timeout: schema8DaemonTimeout}); err != nil {
		return err
	}
	if !response.Verified {
		return fmt.Errorf("process run %q failed verification", p.RunID)
	}
	fmt.Fprintf(out, "Run: %s\nEffective status: %s\n", response.View.Run.ID, response.View.Run.EffectiveStatus)
	return nil
}

func runVerify(ctx context.Context, p *verifyParams, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	if err := preflightStoreRoot(p.StoreRoot); err != nil {
		return err
	}
	fs, err := store.NewFS(p.StoreRoot)
	if err != nil {
		return err
	}
	kind, err := fs.RunStateSchemaKind(ctx, p.RunID)
	if err != nil {
		report := processverify.LoadError(p.RunID, err)
		renderReport(out, report)
		return fmt.Errorf("process run %q failed verification", p.RunID)
	}
	var report processverify.Report
	switch kind {
	case store.RunSchemaResetRequired:
		return fmt.Errorf("%w: process run %q", store.ErrRunResetRequired, p.RunID)
	case store.RunSchemaEpochV8:
		if _, loadErr := fs.LoadEpochV8RunView(ctx, p.RunID); loadErr != nil {
			report = processverify.LoadError(p.RunID, loadErr)
		} else {
			fmt.Fprintf(out, "Run: %s\nEffective status: epoch_v8\n", p.RunID)
			return nil
		}
	case store.RunSchemaLegacy:
		report = processverify.StoreRun(ctx, fs, p.RunID)
	}
	renderReport(out, report)
	if report.HasErrors() {
		return fmt.Errorf("process run %q failed verification", p.RunID)
	}
	return nil
}

func preflightStoreRoot(root string) error {
	info, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("process store root does not exist: %s", root)
	}
	if err != nil {
		return fmt.Errorf("stat process store root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("process store root is not a directory: %s", root)
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
