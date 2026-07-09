package processcmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/common"
)

var runIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

type runParams struct {
	Template  string   `pos:"true" help:"Template YAML path, or stored template ref id@sha256:<hash>"`
	StoreRoot string   `long:"store-root" help:"Filesystem process store root"`
	RunID     string   `long:"run-id" optional:"true" help:"Run id to create (default: template id plus timestamp)"`
	Param     []string `long:"param" optional:"true" help:"Template parameter as key=value; may be repeated"`
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
	tmpl := loaded.Template
	params, err := parseParams(p.Param)
	if err != nil {
		return err
	}
	params, err = applyParamDefaults(tmpl, params)
	if err != nil {
		return err
	}
	templateRef := loaded.Ref
	if templateRef == "" {
		record, err := fs.PutTemplate(cmd.Context(), tmpl)
		if err != nil {
			return err
		}
		templateRef = record.Ref
	}
	runID := strings.TrimSpace(p.RunID)
	if runID == "" {
		runID = defaultRunID(tmpl.ID)
	}
	if !runIDPattern.MatchString(runID) {
		return fmt.Errorf("run id must match %s", runIDPattern.String())
	}
	initial := initialState(runID, templateRef, tmpl)
	run, err := fs.CreateRun(cmd.Context(), store.RunRecord{
		ID:          runID,
		TemplateRef: templateRef,
		Params:      params,
	}, initial)
	if err != nil {
		return err
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

func applyParamDefaults(tmpl *model.Template, params map[string]string) (map[string]string, error) {
	next := make(map[string]string, len(params)+len(tmpl.Params))
	for key := range params {
		if _, ok := tmpl.Params[key]; !ok {
			return nil, fmt.Errorf("unknown template param %q", key)
		}
		next[key] = params[key]
	}
	for key, param := range tmpl.Params {
		required := param.Required != nil && *param.Required
		if _, ok := next[key]; ok {
			continue
		}
		if param.Default != nil {
			value, err := defaultParamString(param.Default)
			if err != nil {
				return nil, fmt.Errorf("default for template param %q: %w", key, err)
			}
			next[key] = value
			continue
		}
		if required {
			return nil, fmt.Errorf("missing required template param %q", key)
		}
	}
	return next, nil
}

func defaultParamString(value any) (string, error) {
	switch v := value.(type) {
	case string:
		return v, nil
	case bool:
		if v {
			return "true", nil
		}
		return "false", nil
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return fmt.Sprint(v), nil
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return string(data), nil
	}
}

func initialState(runID, templateRef string, tmpl *model.Template) state.State {
	nodes := make([]state.NodeInit, 0, len(tmpl.Nodes))
	for nodeID, node := range tmpl.Nodes {
		status := state.NodeStatusPending
		if nodeID == tmpl.Start {
			status = state.NodeStatusReady
		}
		nodes = append(nodes, state.NodeInit{ID: nodeID, Type: node.Type, Status: status})
	}
	st := state.New(runID, templateRef, templateRef, nodes)
	st.Status = state.RunStatusRunning
	return st
}

func defaultRunID(templateID string) string {
	base := strings.TrimSpace(templateID)
	if base == "" {
		base = "run"
	}
	stamp := processNow().UTC().Format("20060102-150405")
	return base + "-" + stamp
}
