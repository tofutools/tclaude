package agentd

import (
	"net/http"
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

// The launch bool fields are the tri-state (unset/true/false) launch settings
// that every spawn path resolves the same way: an explicit value wins, else the
// first profile tier that sets the field and targets the resolved harness, else
// the harness default (see resolveBoolLaunchField). remote_control adds the
// group's remote-control policy between the explicit value and the profiles;
// see resolveRemoteControlLaunch.
//
// launchBoolFieldSpecs is the one place that names each field's profile column
// and harness validator. Unlike the value fields, the three callers resolve
// these in different orders, interleaved with bool fields that are not (yet)
// shared, so resolveLaunchBoolFields takes the caller's order explicitly
// rather than imposing one: note order and first-failure precedence stay as
// each caller had them.
//
// Not in this table on purpose: the caller-local auto_focus and is_owner.
//
// auto_memory, peer_messaging, remote_control and include_group_context joined
// with TCL-1107: before it, only the direct spawn consulted the default
// profiles for them, and trigger/process/scribe workers never resolved them at
// all. Every path now resolves them here, over its own tier stack; what each
// path falls back to when no tier speaks stays its own (see spawnParams'
// IncludeGroupContextDefault).
type launchBoolField int

const (
	launchBoolAutoReview launchBoolField = iota
	launchBoolTrustDir
	launchBoolCopilotAPI
	launchBoolCodexAppServer
	launchBoolFastMode
	launchBoolSSHWorkaround
	launchBoolAutoMemory
	launchBoolPeerMessaging
	launchBoolRemoteControl
	launchBoolIncludeGroupContext
	launchBoolFieldCount
)

type launchBoolFieldSpec struct {
	name     string
	profile  func(*db.SpawnProfile) *bool
	validate func(*harness.Harness) func(bool) (bool, error)
}

var launchBoolFieldSpecs = [launchBoolFieldCount]launchBoolFieldSpec{
	launchBoolAutoReview: {
		name:    "auto_review",
		profile: func(p *db.SpawnProfile) *bool { return p.AutoReview },
		validate: func(h *harness.Harness) func(bool) (bool, error) {
			return func(v bool) (bool, error) { return harness.ResolveAutoReview(h, v) }
		},
	},
	launchBoolTrustDir: {
		name:    "trust_dir",
		profile: func(p *db.SpawnProfile) *bool { return p.TrustDir },
		validate: func(h *harness.Harness) func(bool) (bool, error) {
			return func(v bool) (bool, error) { return harness.ResolveTrustDir(h, v) }
		},
	},
	launchBoolCopilotAPI: {
		name:    "copilot_api",
		profile: func(p *db.SpawnProfile) *bool { return p.CopilotAPI },
		validate: func(h *harness.Harness) func(bool) (bool, error) {
			return func(v bool) (bool, error) { return harness.ResolveCopilotAPI(h, &v) }
		},
	},
	launchBoolCodexAppServer: {
		name:    "codex_app_server",
		profile: func(p *db.SpawnProfile) *bool { return p.CodexAppServer },
		validate: func(h *harness.Harness) func(bool) (bool, error) {
			return func(v bool) (bool, error) { return harness.ResolveCodexAppServer(h, &v) }
		},
	},
	// ResolveFastMode returns the harness's own spelling; the bool itself is
	// what the launch carries, so only its error is used.
	launchBoolFastMode: {
		name:    "fast_mode",
		profile: func(p *db.SpawnProfile) *bool { return p.FastMode },
		validate: func(h *harness.Harness) func(bool) (bool, error) {
			return func(v bool) (bool, error) {
				_, err := harness.ResolveFastMode(h, &v)
				return v, err
			}
		},
	},
	launchBoolSSHWorkaround: {
		name:    "ssh_workaround",
		profile: func(p *db.SpawnProfile) *bool { return p.SSHWorkaround },
		validate: func(h *harness.Harness) func(bool) (bool, error) {
			return func(v bool) (bool, error) { return harness.ResolveSSHWorkaround(h, &v) }
		},
	},
	// Unset everywhere resolves to FALSE, and false means tclaude injects
	// CLAUDE_CODE_DISABLE_AUTO_MEMORY=1: Claude Code's per-project memory store
	// is shared by every agent on the repo, so leaving it on cross-pollutes
	// their notes. Only an opt-in turns it back on.
	launchBoolAutoMemory: {
		name:    "auto_memory",
		profile: func(p *db.SpawnProfile) *bool { return p.AutoMemory },
		validate: func(h *harness.Harness) func(bool) (bool, error) {
			return func(v bool) (bool, error) { return harness.ResolveAutoMemory(h, &v) }
		},
	},
	// Same fallback, closely related reason: Claude Code's own cross-session
	// messaging is a second coordination channel with none of tclaude's group
	// membership, permission slugs or audit trail.
	launchBoolPeerMessaging: {
		name:    "peer_messaging",
		profile: func(p *db.SpawnProfile) *bool { return p.PeerMessaging },
		validate: func(h *harness.Harness) func(bool) (bool, error) {
			return func(v bool) (bool, error) { return harness.ResolvePeerMessaging(h, &v) }
		},
	},
	// Resolve through resolveRemoteControlLaunch, not resolveLaunchBoolFields:
	// the group policy sits between the explicit value and the profiles.
	launchBoolRemoteControl: {
		name:    "remote_control",
		profile: func(p *db.SpawnProfile) *bool { return p.RemoteControl },
		validate: func(h *harness.Harness) func(bool) (bool, error) {
			return func(v bool) (bool, error) { return harness.ResolveRemoteControl(h, v) }
		},
	},
	// Harness-agnostic (harnessAgnosticLaunchField): it decides what the new
	// agent is TOLD, not how its harness runs. Unset at every tier falls back to
	// the calling path's own default.
	launchBoolIncludeGroupContext: {
		name:     includeGroupContextField,
		profile:  func(p *db.SpawnProfile) *bool { return p.IncludeGroupDefaultContext },
		validate: func(*harness.Harness) func(bool) (bool, error) { return func(v bool) (bool, error) { return v, nil } },
	},
}

// launchBoolResolution carries the launch bool fields in and out. Going in,
// Value/Set are the explicit request (Set false means "not requested"); coming
// out they are the resolved value and whether any tier decided it, with the
// deciding tier's source and any skip note.
type launchBoolResolution struct {
	Value  [launchBoolFieldCount]bool
	Set    [launchBoolFieldCount]bool
	Source [launchBoolFieldCount]string
	Note   [launchBoolFieldCount]string
}

// resolveLaunchBoolFields resolves the named fields in the order given. It
// returns the non-empty notes of the fields it resolved, in that order; on
// failure it stops at the failing field and still returns the notes so far.
func resolveLaunchBoolFields(h *harness.Harness, tiers []launchProfileTier, res *launchBoolResolution,
	fields ...launchBoolField) ([]string, *spawnFailure) {
	var notes []string
	for _, f := range fields {
		spec := launchBoolFieldSpecs[f]
		value, set, source, note, fail := resolveBoolLaunchField(spec.name, res.Value[f], res.Set[f], h.Name, tiers,
			spec.profile, spec.validate(h))
		if fail != nil {
			return notes, fail
		}
		res.Value[f], res.Set[f], res.Source[f], res.Note[f] = value, set, source, note
		if strings.TrimSpace(note) != "" {
			notes = append(notes, note)
		}
	}
	return notes, nil
}

// resolveRemoteControlLaunch resolves remote_control for every spawn path
// (TCL-1107). Precedence, highest first:
//
//	explicit  >  group remote-control policy  >  profile tiers  >  off
//
// An explicit value is validated against the harness (an explicit opt-in for a
// harness with no Remote Access is a 400). The profile tiers are walked even
// when a higher layer wins, so a skipped or invalid profile value is disclosed
// or refused the same way whoever decides. A value the policy or a profile
// derived is then clamped to off for a harness with no Remote Access: a group
// or default-profile setting must not fail a Codex spawn.
func resolveRemoteControlLaunch(h *harness.Harness, tiers []launchProfileTier, groupPolicy, explicit *bool) (bool, string, *spawnFailure) {
	spec := launchBoolFieldSpecs[launchBoolRemoteControl]
	if explicit != nil {
		if _, err := spec.validate(h)(*explicit); err != nil {
			return false, "", &spawnFailure{http.StatusBadRequest, "invalid_remote_control", err.Error()}
		}
	}
	profileValue, profileSet, _, note, fail := resolveBoolLaunchField(spec.name, false, false, h.Name, tiers,
		spec.profile, spec.validate(h))
	if fail != nil {
		return false, note, fail
	}
	var profileDefault *bool
	if profileSet {
		profileDefault = &profileValue
	}
	value := resolveRemoteControlIntent(groupPolicy, profileDefault, explicit)
	if value && !h.CanRemoteControl() {
		value = false
	}
	return value, note, nil
}
