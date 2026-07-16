package processcmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	processengine "github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/common"
)

type runParams struct {
	Template      string   `pos:"true" help:"Template YAML path, or stored template ref id@sha256:<hash>"`
	StoreRoot     string   `long:"store-root" help:"Filesystem process store root"`
	RunID         string   `long:"run-id" optional:"true" help:"Run id to create (default: template id plus timestamp)"`
	Param         []string `long:"param" optional:"true" help:"Template parameter as key=value; may be repeated"`
	AllowPrograms bool     `long:"allow-programs" optional:"true" help:"Explicitly allow program performers to execute for this run"`
}

func runCmd() *cobra.Command {
	return boa.CmdT[runParams]{
		Use:         "run",
		Short:       "Instantiate a process template",
		Long:        "Instantiate a process template into a local filesystem process store.",
		ParamEnrich: common.DefaultParamEnricher(),
		Args:        cobra.ExactArgs(1),
		PreExecuteFunc: func(p *runParams, _ *cobra.Command, _ []string) error {
			if err := requireProcessesEnabled(); err != nil {
				return err
			}
			if strings.TrimSpace(p.StoreRoot) == "" {
				return fmt.Errorf("--store-root is required")
			}
			return nil
		},
		RunFunc: func(p *runParams, cmd *cobra.Command, _ []string) {
			exitWithError(runRun(cmd, p, os.Stdout))
		},
	}.ToCobra()
}

func runRun(cmd *cobra.Command, p *runParams, out io.Writer) error {
	fs, err := openStore(p.StoreRoot, false)
	if err != nil {
		return err
	}
	loaded, err := loadTemplate(cmd, fs, p.Template)
	if err != nil {
		return err
	}
	params, err := parseParams(p.Param)
	if err != nil {
		return err
	}
	now := processNow()
	templateRef := loaded.Ref
	if templateRef == "" {
		if err := processengine.ValidateInstantiation(loaded.Template, processengine.InstantiateRequest{
			RunID: p.RunID, Params: params, Now: now, EngineCapabilities: processengine.ProductionEngineCapabilities(),
		}); err != nil {
			return err
		}
		record, err := fs.PutTemplateVersion(cmd.Context(), loaded.Template)
		if err != nil {
			return err
		}
		templateRef = record.Ref
	}
	run, err := processengine.Instantiate(cmd.Context(), fs, processengine.InstantiateRequest{
		TemplateRef:        templateRef,
		RunID:              p.RunID,
		Params:             params,
		Now:                now,
		EngineCapabilities: processengine.ProductionEngineCapabilities(),
	})
	if err != nil {
		return err
	}
	if p.AllowPrograms {
		at := processNow().UTC()
		_, err = fs.Append(cmd.Context(), run.ID, 0, []evidence.LogEntry{
			runLogEntry(evidence.EntryKindAdmin, state.Event{
				Type:   state.EventAdminProgramsAllowed,
				Actor:  defaultActor(),
				Reason: "program execution explicitly allowed at instantiation (--allow-programs)",
			}, "", at),
		})
		if err != nil {
			return fmt.Errorf("record --allow-programs audit entry for run %q: %w", run.ID, err)
		}
		run, err = fs.SetProgramsAllowed(cmd.Context(), run.ID)
		if err != nil {
			return fmt.Errorf("enable programs for run %q after audit: %w", run.ID, err)
		}
	}
	fmt.Fprintf(out, "Created run %s\n", run.ID)
	fmt.Fprintf(out, "Template: %s\n", run.TemplateRef)
	fmt.Fprintf(out, "Store: %s\n", p.StoreRoot)
	return nil
}

type loadedTemplate struct {
	Template *model.Template
	Ref      string
}

func loadTemplate(cmd *cobra.Command, fs *store.FS, source string) (loadedTemplate, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return loadedTemplate{}, fmt.Errorf("template path or ref is required")
	}
	if _, err := os.Stat(source); err == nil {
		data, err := os.ReadFile(filepath.Clean(source))
		if err != nil {
			return loadedTemplate{}, fmt.Errorf("read template: %w", err)
		}
		parsed, err := model.Parse(data)
		if err != nil {
			return loadedTemplate{}, err
		}
		if parsed.Diagnostics.HasErrors() {
			printDiagnostics(os.Stderr, parsed.Diagnostics.Errors())
			return loadedTemplate{}, fmt.Errorf("template %q failed validation", source)
		}
		return loadedTemplate{Template: parsed.Template}, nil
	} else if !os.IsNotExist(err) {
		return loadedTemplate{}, fmt.Errorf("stat template: %w", err)
	}
	tmpl, err := fs.GetTemplate(cmd.Context(), source)
	if err != nil {
		return loadedTemplate{}, fmt.Errorf("load stored template: %w", err)
	}
	return loadedTemplate{Template: tmpl, Ref: source}, nil
}

func parseParams(values []string) (map[string]string, error) {
	params := map[string]string{}
	for _, value := range values {
		key, val, ok := strings.Cut(value, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("--param must be key=value, got %q", value)
		}
		if _, exists := params[key]; exists {
			return nil, fmt.Errorf("duplicate template param %q", key)
		}
		params[key] = val
	}
	return params, nil
}
