package agentd

import (
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The launch value fields are the string/int launch settings that every spawn
// path resolves the same way: explicit > named/inline profile > group default
// profile > global default profile > harness default, each validated against
// the resolved harness (see resolveStringLaunchField for the tier rules).
//
// They used to be wired by hand, field by field, in handleGroupSpawn,
// applyDefaultProfile and resolveTemplateAgentLaunch. Those copies drifted and
// had to be realigned more than once. launchValueFieldSpecs is now the one
// place that says which profile column feeds a field and which harness
// validator gates it, and resolveLaunchValueFields walks it in one fixed order.
//
// Adding a string/int launch field means one entry here plus one field on
// launchValueFields. The bool family (resolveBoolLaunchField) is deliberately
// not part of this table yet.
type launchValueField int

const (
	launchValueModelProxy launchValueField = iota
	launchValueModel
	launchValueEffort
	launchValueSandbox
	launchValueApproval
	launchValueTools
	launchValueAskUserQuestionTimeout
	launchValueAutoCompactWindow
	launchValueContextWindowMax
	launchValueFieldCount
)

// launchValueFields carries one value per launch value field: the explicit
// request going in, the resolved launch coming out. "" and 0 mean unset.
type launchValueFields struct {
	ModelProxy             string
	Model                  string
	Effort                 string
	Sandbox                string // the harness builtin sandbox mode
	Approval               string
	Tools                  string
	AskUserQuestionTimeout string
	AutoCompactWindow      string
	ContextWindowMax       int64
}

// launchValueFieldSpec describes one launch value field. Exactly one of the
// string or int accessor sets is populated.
type launchValueFieldSpec struct {
	name string

	str         func(*launchValueFields) *string
	profileStr  func(*db.SpawnProfile) string
	validateStr func(*harness.Harness) func(string) (string, error)

	num         func(*launchValueFields) *int64
	profileNum  func(*db.SpawnProfile) int64
	validateNum func(*harness.Harness) func(int64) (int64, error)
}

// launchValueFieldSpecs is indexed by launchValueField and listed in
// resolution order, which decides whose failure a request with several
// invalid values reports first.
var launchValueFieldSpecs = [launchValueFieldCount]launchValueFieldSpec{
	launchValueModelProxy: {
		name:        "model_proxy",
		str:         func(v *launchValueFields) *string { return &v.ModelProxy },
		profileStr:  func(p *db.SpawnProfile) string { return p.ModelProxy },
		validateStr: validateLaunchModelProxy,
	},
	launchValueModel: {
		name:       modelField,
		str:        func(v *launchValueFields) *string { return &v.Model },
		profileStr: func(p *db.SpawnProfile) string { return p.Model },
		validateStr: func(h *harness.Harness) func(string) (string, error) {
			return h.Models.ValidateModel
		},
	},
	launchValueEffort: {
		name:       effortField,
		str:        func(v *launchValueFields) *string { return &v.Effort },
		profileStr: func(p *db.SpawnProfile) string { return p.Effort },
		validateStr: func(h *harness.Harness) func(string) (string, error) {
			return h.Models.ValidateEffort
		},
	},
	launchValueSandbox: {
		name:       "sandbox",
		str:        func(v *launchValueFields) *string { return &v.Sandbox },
		profileStr: func(p *db.SpawnProfile) string { return p.Sandbox },
		validateStr: func(h *harness.Harness) func(string) (string, error) {
			return func(raw string) (string, error) { return harness.ValidateHarnessBuiltinMode(h, raw) }
		},
	},
	launchValueApproval: {
		name:       "approval",
		str:        func(v *launchValueFields) *string { return &v.Approval },
		profileStr: func(p *db.SpawnProfile) string { return p.Approval },
		validateStr: func(h *harness.Harness) func(string) (string, error) {
			return func(raw string) (string, error) { return harness.ValidateApprovalPolicy(h, raw) }
		},
	},
	launchValueTools: {
		name:       "tools",
		str:        func(v *launchValueFields) *string { return &v.Tools },
		profileStr: func(p *db.SpawnProfile) string { return p.ToolGovernance },
		validateStr: func(h *harness.Harness) func(string) (string, error) {
			return func(raw string) (string, error) { return harness.ValidateToolGovernance(h, raw) }
		},
	},
	launchValueAskUserQuestionTimeout: {
		name:       "ask_user_question_timeout",
		str:        func(v *launchValueFields) *string { return &v.AskUserQuestionTimeout },
		profileStr: func(p *db.SpawnProfile) string { return p.AskUserQuestionTimeout },
		validateStr: func(h *harness.Harness) func(string) (string, error) {
			return func(raw string) (string, error) { return harness.ResolveAskTimeoutMode(h, raw) }
		},
	},
	// The validator both gates the window on the harness and NORMALIZES its
	// spelling, so "450k" and "450000" resolve to the same canonical value.
	launchValueAutoCompactWindow: {
		name:       "auto_compact_window",
		str:        func(v *launchValueFields) *string { return &v.AutoCompactWindow },
		profileStr: func(p *db.SpawnProfile) string { return p.AutoCompactWindow },
		validateStr: func(h *harness.Harness) func(string) (string, error) {
			return func(raw string) (string, error) { return harness.ResolveAutoCompactWindow(h, raw) }
		},
	},
	launchValueContextWindowMax: {
		name:       contextWindowMaxField,
		num:        func(v *launchValueFields) *int64 { return &v.ContextWindowMax },
		profileNum: func(p *db.SpawnProfile) int64 { return p.ContextWindowMax },
		validateNum: func(h *harness.Harness) func(int64) (int64, error) {
			return func(raw int64) (int64, error) { return harness.ResolveCopilotContextWindow(h, raw) }
		},
	},
}

// allLaunchValueFields lists every launch value field in resolution order.
var allLaunchValueFields = func() []launchValueField {
	out := make([]launchValueField, launchValueFieldCount)
	for i := range out {
		out[i] = launchValueField(i)
	}
	return out
}()

// launchValueResolution is the outcome of resolving launch value fields:
// the resolved values plus, per field, the tier that decided it and any
// skip note. Fields not (yet) resolved keep empty source and note.
type launchValueResolution struct {
	Values launchValueFields
	Source [launchValueFieldCount]string
	Note   [launchValueFieldCount]string
}

// Notes returns the non-empty notes in resolution order.
func (r *launchValueResolution) Notes() []string {
	var out []string
	for _, note := range r.Note {
		if strings.TrimSpace(note) != "" {
			out = append(out, note)
		}
	}
	return out
}

// launchValueOptions carries the few caller-specific variations that are part
// of a caller's contract rather than of the field.
type launchValueOptions struct {
	// validateModel replaces the harness model validator. The direct spawn
	// boundary uses it to add a "pass --harness" hint to the error.
	validateModel func(string) (string, error)
}

// resolveLaunchValueFields resolves the given fields (all of them when none
// are named) in table order. res.Values starts as the explicit request and is
// overwritten field by field with the resolved value. On the first failure it
// stops and returns it; res then holds every field resolved before it, so a
// caller can still report their notes.
func resolveLaunchValueFields(h *harness.Harness, tiers []launchProfileTier, res *launchValueResolution,
	opts launchValueOptions, fields ...launchValueField) *spawnFailure {
	if len(fields) == 0 {
		fields = allLaunchValueFields
	}
	for _, f := range fields {
		spec := launchValueFieldSpecs[f]
		var source, note string
		var fail *spawnFailure
		if spec.num != nil {
			ptr := spec.num(&res.Values)
			*ptr, source, note, fail = resolveIntLaunchField(spec.name, *ptr, h.Name, tiers,
				spec.profileNum, spec.validateNum(h))
		} else {
			validate := spec.validateStr(h)
			if f == launchValueModel && opts.validateModel != nil {
				validate = opts.validateModel
			}
			ptr := spec.str(&res.Values)
			*ptr, source, note, fail = resolveStringLaunchField(spec.name, *ptr, h.Name, tiers,
				spec.profileStr, validate)
		}
		if fail != nil {
			return fail
		}
		res.Source[f], res.Note[f] = source, note
	}
	return nil
}

// resolveLaunchValueField resolves one string launch value field from the
// tiers alone (no explicit value), for callers that need a single landing
// value rather than a whole launch.
func resolveLaunchValueField(h *harness.Harness, tiers []launchProfileTier, f launchValueField) (value, source string, fail *spawnFailure) {
	var res launchValueResolution
	if fail := resolveLaunchValueFields(h, tiers, &res, launchValueOptions{}, f); fail != nil {
		return "", "", fail
	}
	return *launchValueFieldSpecs[f].str(&res.Values), res.Source[f], nil
}
