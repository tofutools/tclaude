package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

var runIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// InstantiateInputError identifies caller-controlled request failures without
// forcing HTTP callers to parse error strings (which may contain param names).
type InstantiateInputError struct{ Err error }

func (e *InstantiateInputError) Error() string { return e.Err.Error() }
func (e *InstantiateInputError) Unwrap() error { return e.Err }

func IsInstantiateInputError(err error) bool {
	var input *InstantiateInputError
	return errors.As(err, &input)
}

// ValidateInstantiation checks every caller-controlled input that can be
// rejected before a local source template is published to the store. It is
// the same preparation path Instantiate uses after loading an exact ref, so
// CLI failure atomicity does not require a second params/defaults validator.
func ValidateInstantiation(tmpl *model.Template, request InstantiateRequest) error {
	_, _, _, err := prepareInstantiation(tmpl, request)
	return err
}

// InstantiateRequest is the shared run-creation contract used by the manual
// CLI and the agentd REST surface. TemplateRef must name one immutable stored
// version; callers that start from a source file store it before calling here.
type InstantiateRequest struct {
	TemplateRef string
	RunID       string
	Params      map[string]string
	Now         time.Time
	// ReplayExisting makes an explicit RunID an idempotency boundary: an
	// existing run is returned only when its pinned template and resolved
	// params are identical. Generated IDs still suffix collisions, and the
	// zero value preserves the CLI's duplicate-ID error semantics.
	ReplayExisting bool
}

// Instantiate creates the durable run snapshot that the engine host discovers
// on its next tick. Keeping defaults, required-param checks, initial state, and
// run-id generation here prevents REST-created runs from becoming a sibling
// flavor of the CLI-created records.
func Instantiate(ctx context.Context, st store.Store, request InstantiateRequest) (store.RunRecord, error) {
	if st == nil {
		return store.RunRecord{}, fmt.Errorf("process store is required")
	}
	templateRef := strings.TrimSpace(request.TemplateRef)
	if templateRef == "" {
		return store.RunRecord{}, &InstantiateInputError{Err: fmt.Errorf("template ref is required")}
	}
	tmpl, err := st.GetTemplate(ctx, templateRef)
	if err != nil {
		return store.RunRecord{}, fmt.Errorf("load stored template: %w", err)
	}
	params, runID, generatedID, err := prepareInstantiation(tmpl, request)
	if err != nil {
		return store.RunRecord{}, err
	}
	const maxGeneratedIDAttempts = 1000
	for attempt := 1; attempt <= maxGeneratedIDAttempts; attempt++ {
		candidate := runID
		if attempt > 1 {
			candidate = fmt.Sprintf("%s-%d", runID, attempt)
		}
		created, err := st.CreateRun(ctx, store.RunRecord{
			ID: candidate, TemplateRef: templateRef, Params: params,
		}, initialState(candidate, templateRef, tmpl))
		if err == nil {
			return created, nil
		}
		if request.ReplayExisting && !generatedID && errors.Is(err, store.ErrRunExists) {
			existing, loadErr := st.GetRun(ctx, candidate)
			if loadErr == nil && existing.TemplateRef == templateRef && maps.Equal(existing.Params, params) {
				return existing, nil
			}
		}
		if !generatedID || !errors.Is(err, store.ErrRunExists) {
			return store.RunRecord{}, err
		}
	}
	return store.RunRecord{}, fmt.Errorf("generate unique run id after %d attempts", maxGeneratedIDAttempts)
}

func prepareInstantiation(tmpl *model.Template, request InstantiateRequest) (map[string]string, string, bool, error) {
	if diagnostics := model.Validate(tmpl, model.NormalizeEdges(tmpl)); diagnostics.HasErrors() {
		return nil, "", false, &InstantiateInputError{Err: fmt.Errorf("template has validation errors")}
	}
	params, err := applyParamDefaults(tmpl, request.Params)
	if err != nil {
		return nil, "", false, &InstantiateInputError{Err: err}
	}
	runID := strings.TrimSpace(request.RunID)
	generatedID := runID == ""
	if generatedID {
		now := request.Now
		if now.IsZero() {
			now = time.Now()
		}
		runID = defaultRunID(tmpl.ID, now)
	}
	if !runIDPattern.MatchString(runID) {
		return nil, "", false, &InstantiateInputError{Err: fmt.Errorf("run id must match %s", runIDPattern.String())}
	}
	return params, runID, generatedID, nil
}

func applyParamDefaults(tmpl *model.Template, params map[string]string) (map[string]string, error) {
	next := make(map[string]string, len(params)+len(tmpl.Params))
	for key, value := range params {
		if _, ok := tmpl.Params[key]; !ok {
			return nil, fmt.Errorf("unknown template param %q", key)
		}
		next[key] = value
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

func defaultRunID(templateID string, now time.Time) string {
	base := strings.TrimSpace(templateID)
	if base == "" {
		base = "run"
	}
	return base + "-" + now.UTC().Format("20060102-150405")
}
