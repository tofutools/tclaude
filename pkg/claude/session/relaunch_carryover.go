package session

import (
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// launchCarryoverField is one row of the relaunch contract: a launch parameter
// that `tclaude session new -r` reproduces from the conversation's recorded
// posture unless the caller overrides it explicitly.
//
// The table below is the single place that list lives for this path. Before
// TCL-730 there was no list at all — the resume path wrote whatever params
// happened to hold, so every omitted flag was recorded as asserted intent and
// the original value was gone for good on the next relaunch.
type launchCarryoverField struct {
	// flag is the `session new` flag this parameter is set with. The Cobra entry
	// point marks it in explicitLaunchFields via Changed(), which is the only
	// place "omitted" and "explicitly set to the default" are still distinct.
	flag string
	// recorded is the db.AgentRelaunchProfile field this parameter is carried
	// from. Declaring it lets TestLaunchCarryoverCoversEveryRecordedField check
	// the table against the profile struct by reflection, so a field added there
	// cannot quietly go uncarried.
	recorded string
	// containment marks a parameter whose DROP direction is less containment —
	// the launch ends up with a weaker sandbox or a more automatic approval
	// posture than the record proves. Those drops are reported to the operator
	// rather than only logged; runNew applies the same rule to its own
	// sandbox/approval gates.
	containment bool
	// supplied reports whether params already carries a caller value, for
	// callers that did not come through Cobra. It is a fallback, not a
	// substitute: a bool flag explicitly set to false is indistinguishable from
	// an omitted one here, which is exactly why `explicit` exists.
	supplied func(*NewParams) bool
	// unpinned lists the values that mean "nothing pinned" for this parameter
	// BEYOND its type's zero. Three axes carry a first-class `inherit` sentinel
	// that survives validation precisely so an explicit inherit stays
	// distinguishable from an omitted flag — and then collapses to "emit no
	// flag" at the last layer. It is a property of the axis, not of the Go type,
	// so it is declared here rather than hand-written into each carry closure:
	// that is what lets classify and its structural guard enumerate every
	// spelling instead of depending on someone remembering a fourth one.
	unpinned []string
	// carry writes the recorded value onto params and reports the value it
	// applied together with what happened. carryUnrecorded means the posture
	// holds nothing for this field (nil = unknown, and unknown must stay
	// unknown); carryDropped means a value WAS recorded but the harness this
	// relaunch will use cannot honour it. Deciding whether an applied value is
	// worth disclosing is classify's job, not the closure's.
	carry func(*harness.Harness, *db.AgentRelaunchProfile, *NewParams) (any, carryOutcome)
}

// classify refines what carry reported: an applied value that means "nothing
// pinned" — the type's zero, or one of this row's sentinel spellings — produces
// the same launch as an omitted flag, so it is carried but not disclosed.
//
// It lives here rather than in each carry closure so there is exactly one place
// the rule exists, and so the structural guard can drive every spelling of
// unpinned through it.
func (f launchCarryoverField) classify(applied any, outcome carryOutcome) carryOutcome {
	if outcome != carryApplied {
		return outcome
	}
	switch v := applied.(type) {
	case string:
		if v == "" || slices.Contains(f.unpinned, v) {
			return carryAppliedDefault
		}
	case bool:
		if !v {
			return carryAppliedDefault
		}
	}
	return carryApplied
}

// carryOutcome is what one launchCarryoverField did with the recorded posture.
// The four cases exist because they are told to the operator differently:
// nothing recorded and a recorded no-op are both silent, a recorded value that
// this harness cannot honour is a warning, and a recorded value that genuinely
// changes the launch is the one worth a disclosure.
type carryOutcome int

const (
	carryUnrecorded carryOutcome = iota
	// carryApplied: a recorded value landed on params AND differs from what a
	// flagless launch would have produced.
	carryApplied
	// carryAppliedDefault: a recorded value landed on params but is that field's
	// no-op — "known: nothing pinned", which resolves to exactly the launch an
	// omitted flag gives. Still applied (the record must stay asserted rather
	// than decaying to unknown), but never disclosed: a banner that fires on
	// every ordinary resume listing postures that changed nothing is noise the
	// operator learns to skip, and then --sandbox off arrives inside it.
	carryAppliedDefault
	carryDropped
)

// launchCarryoverExcused names the db.AgentRelaunchProfile fields `session new
// -r` deliberately does NOT carry, with the reason. Together with
// launchCarryoverFields it must account for every field on that struct;
// TestLaunchCarryoverCoversEveryRecordedField enforces it, so adding a recorded
// launch parameter forces a decision here instead of letting it drift.
var launchCarryoverExcused = map[string]string{
	"Version": "profile bookkeeping, not a launch parameter",
	"ModelID": "Claude Code and Codex both remember which model a conversation runs on " +
		"across a resume, and the status line re-records it on every render, so an " +
		"omitted --model resumes on the same model instead of reverting",
	"Effort": "recorded by the status line on every render, like ModelID",
	"SandboxModeSource": "not a launch parameter of its own — it is the attribution " +
		"for SandboxMode and is carried by that field's own carry(), so a separate " +
		"entry here would be a second, desynchronizable copy of the same decision",
	"TemporarySandboxMode": "an agent-owned override folded into SandboxMode's carry; " +
		"it is not a second independent session-new flag",
	"ContextWindowSize": "an OBSERVED statusline value, not operator intent; carrying " +
		"it would mean re-deriving Claude's \"[1m]\" model suffix rather than replaying " +
		"a decision",
	"SSHWorkaround": "agentd materializes this internal Codex sandbox capability from " +
		"the effective sandbox snapshot; it is not a session-new launch flag",
}

// launchCarryoverFields is the relaunch contract for `session new -r`: the
// launch parameters a resume reproduces from the conversation's recorded
// posture whenever the caller did not pass the flag itself.
var launchCarryoverFields = []launchCarryoverField{
	{
		flag:        "sandbox",
		recorded:    "SandboxMode",
		containment: true,
		unpinned:    []string{harness.ClaudeSandboxInherit},
		// --permission-profile is the same decision spelled differently (and is
		// mutually exclusive with --sandbox), so a caller who passed it owns the
		// containment posture and must not have a recorded --sandbox added under it.
		supplied: func(p *NewParams) bool {
			return strings.TrimSpace(p.Sandbox) != "" || strings.TrimSpace(p.PermissionProfile) != ""
		},
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) (any, carryOutcome) {
			recordedMode := rec.SandboxMode
			recordedSource := rec.SandboxModeSource
			if rec.TemporarySandboxMode != nil {
				recordedMode = rec.TemporarySandboxMode
				source := db.TemporarySandboxModeSource
				recordedSource = &source
			}
			if recordedMode == nil {
				return nil, carryUnrecorded
			}
			mode, err := harness.ValidateSandboxMode(h, strings.TrimSpace(*recordedMode))
			if err != nil {
				return nil, carryDropped
			}
			p.Sandbox = mode
			// The attribution rides with the mode it explains, so a resumed agent
			// keeps naming the profile that chose its containment instead of
			// degrading to an anonymous "this launch" on its first restart.
			if recordedSource != nil {
				p.SandboxChosenBy = strings.TrimSpace(*recordedSource)
			}
			return mode, carryApplied
		},
	},
	{
		flag:        "ask-for-approval",
		recorded:    "ApprovalPolicy",
		containment: true,
		unpinned:    []string{harness.ClaudePermissionInherit},
		supplied:    func(p *NewParams) bool { return strings.TrimSpace(p.Approval) != "" },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) (any, carryOutcome) {
			if rec.ApprovalPolicy == nil {
				return nil, carryUnrecorded
			}
			policy, err := harness.ValidateApprovalPolicy(h, strings.TrimSpace(*rec.ApprovalPolicy))
			if err != nil {
				return nil, carryDropped
			}
			p.Approval = policy
			return policy, carryApplied
		},
	},
	{
		flag:     "auto-review",
		recorded: "ApprovalAutoReview",
		supplied: func(p *NewParams) bool { return p.AutoReview },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) (any, carryOutcome) {
			if rec.ApprovalAutoReview == nil {
				return nil, carryUnrecorded
			}
			autoReview, err := harness.ResolveAutoReview(h, *rec.ApprovalAutoReview)
			if err != nil {
				return nil, carryDropped
			}
			p.AutoReview = autoReview
			return autoReview, carryApplied
		},
	},
	{
		flag:     "tools",
		recorded: "ToolGovernance",
		supplied: func(p *NewParams) bool { return strings.TrimSpace(p.ToolGovernance) != "" },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) (any, carryOutcome) {
			if rec.ToolGovernance == nil {
				return nil, carryUnrecorded
			}
			governance, err := harness.ValidateToolGovernance(h, strings.TrimSpace(*rec.ToolGovernance))
			if err != nil {
				return nil, carryDropped
			}
			p.ToolGovernance = governance
			return governance, carryApplied
		},
	},
	{
		flag:     "ask-user-question-timeout",
		recorded: "AskUserQuestionTimeout",
		unpinned: []string{harness.ClaudeAskTimeoutInherit},
		supplied: func(p *NewParams) bool { return strings.TrimSpace(p.AskUserQuestionTimeout) != "" },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) (any, carryOutcome) {
			if rec.AskUserQuestionTimeout == nil {
				return nil, carryUnrecorded
			}
			timeout, err := harness.ResolveAskTimeoutMode(h, strings.TrimSpace(*rec.AskUserQuestionTimeout))
			if err != nil {
				return nil, carryDropped
			}
			p.AskUserQuestionTimeout = timeout
			return timeout, carryApplied
		},
	},
	{
		flag:     "remote-control",
		recorded: "RemoteControl",
		supplied: func(p *NewParams) bool { return p.RemoteControl },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) (any, carryOutcome) {
			if rec.RemoteControl == nil {
				return nil, carryUnrecorded
			}
			remoteControl, err := harness.ResolveRemoteControl(h, *rec.RemoteControl)
			if err != nil {
				return nil, carryDropped
			}
			p.RemoteControl = remoteControl
			return remoteControl, carryApplied
		},
	},
	{
		flag:     "auto-memory",
		recorded: "AutoMemory",
		supplied: func(p *NewParams) bool { return p.AutoMemory },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) (any, carryOutcome) {
			if rec.AutoMemory == nil {
				return nil, carryUnrecorded
			}
			autoMemory, err := harness.ResolveAutoMemory(h, rec.AutoMemory)
			if err != nil {
				return nil, carryDropped
			}
			p.AutoMemory = autoMemory
			return autoMemory, carryApplied
		},
	},
	{
		flag:     "context-features",
		recorded: "ContextFeatures",
		supplied: func(p *NewParams) bool { return strings.TrimSpace(p.ContextFeatures) != "" },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) (any, carryOutcome) {
			if rec.ContextFeatures == nil {
				return nil, carryUnrecorded
			}
			// Re-resolve against the harness this relaunch will use, dropping
			// entries a catalog change has retired. Losing a trim costs context,
			// never correctness, so failing soft beats wedging the resume — the
			// same trade the daemon relaunch path makes.
			kept := map[string]string{}
			for slug, state := range *rec.ContextFeatures {
				if _, ok := harness.LookupContextFeature(slug); !ok {
					continue
				}
				if normalized, err := harness.ValidateContextFeatureState(state); err == nil && normalized != "" {
					kept[slug] = normalized
				}
			}
			resolved, err := harness.ResolveContextFeatures(h, kept)
			if err != nil {
				return nil, carryDropped
			}
			p.ContextFeatures = harness.FormatContextFeatures(resolved)
			return p.ContextFeatures, carryApplied
		},
	},
	{
		flag:     "auto-compact-window",
		recorded: "AutoCompactWindow",
		supplied: func(p *NewParams) bool { return strings.TrimSpace(p.AutoCompactWindow) != "" },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) (any, carryOutcome) {
			if rec.AutoCompactWindow == nil {
				return nil, carryUnrecorded
			}
			window, err := harness.ResolveAutoCompactWindow(h, strings.TrimSpace(*rec.AutoCompactWindow))
			if err != nil {
				return nil, carryDropped
			}
			p.AutoCompactWindow = window
			return window, carryApplied
		},
	},
}

// applyRecordedLaunchPosture fills every launch flag the caller left out of a
// `session new -r` with the value the conversation was last launched with.
//
// It runs before runNew rather than inside it because runNew both resolves the
// launch AND records it: a field still blank when runNew reaches its post-row
// writes is persisted as asserted intent ("known: nothing pinned"), and the
// durable relaunch profile then hands that assertion to the NEXT relaunch. So
// an omitted flag does not merely fall back to a default for one launch — it
// destroys the recorded value permanently. See TCL-730.
//
// Resolving the conversation here duplicates the lookup runNew performs later.
// Both are pure reads, and paying for one extra index lookup is a better trade
// than reordering a launch sequence whose validation, capability gates, and
// row-rollback ordering are load-bearing.
func applyRecordedLaunchPosture(params *NewParams, explicit explicitLaunchFields) error {
	if strings.TrimSpace(params.Resume) == "" {
		return nil
	}
	// Agentd resolves the whole relaunch profile itself and passes every field
	// as an explicit flag, so an omitted flag there is a deliberate default, not
	// an absent one. Re-deriving it from the recorded posture would override the
	// daemon's own decision. A plain shell has no harness posture at all.
	if params.ManagedLaunch || params.Shell || strings.TrimSpace(params.Harness) == ShellHarnessName {
		return nil
	}
	h, err := harness.Resolve(strings.TrimSpace(params.Harness))
	if err != nil {
		return nil // runNew reports the unknown harness with its own message
	}
	cwd, err := resolveSessionDir(params.Dir)
	if err != nil {
		return nil // likewise for an unusable --dir
	}
	shortID := resumeIDFromParam(h, params.Resume)
	if shortID == "" {
		return nil
	}
	convID, _, err := resolveResumeConv(h, shortID, params.Global, cwd)
	if err != nil || strings.TrimSpace(convID) == "" {
		return nil // an unresolvable conversation is runNew's error to report
	}
	recorded, err := db.RecordedLaunchPostureForConv(convID)
	if err != nil {
		// Deliberately NOT fail-open. Launching without the recorded posture is
		// not "one launch with defaults" — it overwrites the durable record with
		// those defaults, so a transient read failure would cost the operator the
		// posture permanently. Refusing is recoverable; erasing is not.
		return fmt.Errorf("load recorded launch posture for conversation %s: %w", convID, err)
	}
	if recorded == nil {
		return nil
	}
	carried := make([]string, 0, len(launchCarryoverFields))
	for _, field := range launchCarryoverFields {
		if explicit.has(field.flag) || field.supplied(params) {
			continue
		}
		switch field.classify(field.carry(h, recorded, params)) {
		case carryApplied:
			carried = append(carried, "--"+field.flag)
		case carryDropped:
			// A recorded value this harness cannot honour. Always logged; for a
			// containment parameter also told to the operator, because the drop
			// leaves the launch LESS confined than the record proves it was and a
			// debug line is not a disclosure.
			slog.Warn("session new: dropped a recorded launch parameter the resume harness cannot honour",
				"conv_id", convID, "harness", h.Name, "flag", field.flag)
			if field.containment {
				fmt.Fprintf(os.Stderr,
					"Warning: this conversation's recorded --%s does not apply to harness %q; "+
						"resuming without it. Pass --%s to choose the posture yourself.\n",
					field.flag, h.Name, field.flag)
			}
		case carryUnrecorded, carryAppliedDefault:
		}
	}
	// Tell the operator what this resume is reproducing — but only the postures
	// that make this launch differ from a fresh one. Carrying a launch posture
	// they did not type is the correct behaviour, and it must not be invisible:
	// --sandbox and --ask-for-approval in particular decide how confined the
	// pane is, and a human who typed a bare `-r` deserves to see which postures
	// came back with it. Which is exactly why the line has to stay rare. Most
	// conversations pin nothing, so listing their carried no-ops would put this
	// banner on every ordinary resume, and an operator who has learned to skip
	// it will skip the one that says `--sandbox off` too.
	if len(carried) > 0 {
		fmt.Fprintf(os.Stderr, "Resuming with this conversation's recorded launch posture (%s). "+
			"Pass a flag explicitly to override it.\n", strings.Join(carried, " "))
		slog.Debug("session new: carried recorded launch posture into resume",
			"conv_id", convID, "harness", h.Name, "fields", strings.Join(carried, ","))
	}
	return nil
}

// LaunchPosture is what a launch actually resolved to. It is the WRITE side of
// the relaunch contract; db.AgentRelaunchProfile is the read side, and every
// field here must have a counterpart there or the value cannot survive a hop.
type LaunchPosture struct {
	AutoMemory        bool
	ContextFeatures   map[string]string
	AutoCompactWindow string
	RemoteControl     bool
}

// RecordLaunchPosture persists the launch postures SaveSession's UPSERT
// deliberately does not own, so the durable relaunch profile keeps them for the
// next hop. Every relaunch path must call it, not just the fresh-spawn one: a
// resume mints a NEW session row, and a row that was never annotated reports
// "nothing recorded" — which is how a posture that survived one relaunch
// evaporated at the second (TCL-730).
//
// Every field is written unconditionally, including its zero value, so the row
// records "known: nothing pinned" rather than a legacy unknown. That is only
// safe because every relaunch path now CARRIES these values first — a path that
// resolved a blank default and recorded it here would erase the record, which
// is the bug this all exists to fix.
//
// Best-effort by design. The pane is already running with the right
// environment, so a failed write costs a future relaunch its posture, not this
// session — never worth tearing down a healthy pane over.
func RecordLaunchPosture(sessionID string, h *harness.Harness, posture LaunchPosture) {
	if h == nil {
		return
	}
	if h.SupportsAutoMemory() {
		if err := db.SetSessionAutoMemory(sessionID, posture.AutoMemory); err != nil {
			slog.Warn("failed to record session auto-memory posture",
				"session_id", sessionID, "auto_memory", posture.AutoMemory, "error", err)
		}
	}
	if h.SupportsContextFeatures() {
		if err := db.SetSessionContextFeatures(sessionID, posture.ContextFeatures); err != nil {
			slog.Warn("failed to record session context features",
				"session_id", sessionID,
				"context_features", harness.FormatContextFeatures(posture.ContextFeatures), "error", err)
		}
	}
	if h.SupportsAutoCompactWindow() {
		if err := db.SetSessionAutoCompactWindow(sessionID, posture.AutoCompactWindow); err != nil {
			slog.Warn("failed to record session auto-compact window",
				"session_id", sessionID, "auto_compact_window", posture.AutoCompactWindow, "error", err)
		}
	}
	// Remote Access is the one carried posture whose column no launch used to
	// write: it survived only because a resume reuses the conv-id as its row PK,
	// so the column was never reset. With --label the PK moves, the fresh row
	// starts at 0 and becomes the conv's newest — and the posture is gone at the
	// next hop. Recording it here closes that, and is safe now that every
	// relaunch path carries the value rather than re-defaulting it.
	//
	// ToolGovernance has no session column at all, so nothing can overwrite it and
	// it needs no twin here; it lives only in the durable relaunch profile.
	if h.CanRemoteControl() {
		if err := db.SetSessionRemoteControl(sessionID, posture.RemoteControl); err != nil {
			slog.Warn("failed to record session remote-access posture",
				"session_id", sessionID, "remote_control", posture.RemoteControl, "error", err)
		}
	}
}
