package session

import (
	"fmt"
	"log/slog"
	"os"
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
	// carry writes the recorded value onto params and reports what happened.
	// carryUnrecorded means the posture holds nothing for this field (nil =
	// unknown, and unknown must stay unknown); carryDropped means a value WAS
	// recorded but the harness this relaunch will use cannot honour it.
	carry func(*harness.Harness, *db.AgentRelaunchProfile, *NewParams) carryOutcome
}

// carryOutcome is what one launchCarryoverField did with the recorded posture.
// Distinguishing "nothing was recorded" from "a recorded value was dropped"
// matters: the first is silent and normal, the second is a launch that differs
// from the record and the operator should hear about it.
type carryOutcome int

const (
	carryUnrecorded carryOutcome = iota
	carryApplied
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
	"ContextWindowSize": "an OBSERVED statusline value, not operator intent; carrying " +
		"it would mean re-deriving Claude's \"[1m]\" model suffix rather than replaying " +
		"a decision",
}

// launchCarryoverFields is the relaunch contract for `session new -r`: the
// launch parameters a resume reproduces from the conversation's recorded
// posture whenever the caller did not pass the flag itself.
var launchCarryoverFields = []launchCarryoverField{
	{
		flag:        "sandbox",
		recorded:    "SandboxMode",
		containment: true,
		// --permission-profile is the same decision spelled differently (and is
		// mutually exclusive with --sandbox), so a caller who passed it owns the
		// containment posture and must not have a recorded --sandbox added under it.
		supplied: func(p *NewParams) bool {
			return strings.TrimSpace(p.Sandbox) != "" || strings.TrimSpace(p.PermissionProfile) != ""
		},
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) carryOutcome {
			if rec.SandboxMode == nil {
				return carryUnrecorded
			}
			mode, err := harness.ValidateSandboxMode(h, strings.TrimSpace(*rec.SandboxMode))
			if err != nil {
				return carryDropped
			}
			p.Sandbox = mode
			return carryApplied
		},
	},
	{
		flag:        "ask-for-approval",
		recorded:    "ApprovalPolicy",
		containment: true,
		supplied:    func(p *NewParams) bool { return strings.TrimSpace(p.Approval) != "" },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) carryOutcome {
			if rec.ApprovalPolicy == nil {
				return carryUnrecorded
			}
			policy, err := harness.ValidateApprovalPolicy(h, strings.TrimSpace(*rec.ApprovalPolicy))
			if err != nil {
				return carryDropped
			}
			p.Approval = policy
			return carryApplied
		},
	},
	{
		flag:     "auto-review",
		recorded: "ApprovalAutoReview",
		supplied: func(p *NewParams) bool { return p.AutoReview },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) carryOutcome {
			if rec.ApprovalAutoReview == nil {
				return carryUnrecorded
			}
			autoReview, err := harness.ResolveAutoReview(h, *rec.ApprovalAutoReview)
			if err != nil {
				return carryDropped
			}
			p.AutoReview = autoReview
			return carryApplied
		},
	},
	{
		flag:     "tools",
		recorded: "ToolGovernance",
		supplied: func(p *NewParams) bool { return strings.TrimSpace(p.ToolGovernance) != "" },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) carryOutcome {
			if rec.ToolGovernance == nil {
				return carryUnrecorded
			}
			governance, err := harness.ValidateToolGovernance(h, strings.TrimSpace(*rec.ToolGovernance))
			if err != nil {
				return carryDropped
			}
			p.ToolGovernance = governance
			return carryApplied
		},
	},
	{
		flag:     "ask-user-question-timeout",
		recorded: "AskUserQuestionTimeout",
		supplied: func(p *NewParams) bool { return strings.TrimSpace(p.AskUserQuestionTimeout) != "" },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) carryOutcome {
			if rec.AskUserQuestionTimeout == nil {
				return carryUnrecorded
			}
			timeout, err := harness.ResolveAskTimeoutMode(h, strings.TrimSpace(*rec.AskUserQuestionTimeout))
			if err != nil {
				return carryDropped
			}
			p.AskUserQuestionTimeout = timeout
			return carryApplied
		},
	},
	{
		flag:     "remote-control",
		recorded: "RemoteControl",
		supplied: func(p *NewParams) bool { return p.RemoteControl },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) carryOutcome {
			if rec.RemoteControl == nil {
				return carryUnrecorded
			}
			remoteControl, err := harness.ResolveRemoteControl(h, *rec.RemoteControl)
			if err != nil {
				return carryDropped
			}
			p.RemoteControl = remoteControl
			return carryApplied
		},
	},
	{
		flag:     "auto-memory",
		recorded: "AutoMemory",
		supplied: func(p *NewParams) bool { return p.AutoMemory },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) carryOutcome {
			if rec.AutoMemory == nil {
				return carryUnrecorded
			}
			autoMemory, err := harness.ResolveAutoMemory(h, rec.AutoMemory)
			if err != nil {
				return carryDropped
			}
			p.AutoMemory = autoMemory
			return carryApplied
		},
	},
	{
		flag:     "context-features",
		recorded: "ContextFeatures",
		supplied: func(p *NewParams) bool { return strings.TrimSpace(p.ContextFeatures) != "" },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) carryOutcome {
			if rec.ContextFeatures == nil {
				return carryUnrecorded
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
				return carryDropped
			}
			p.ContextFeatures = harness.FormatContextFeatures(resolved)
			return carryApplied
		},
	},
	{
		flag:     "auto-compact-window",
		recorded: "AutoCompactWindow",
		supplied: func(p *NewParams) bool { return strings.TrimSpace(p.AutoCompactWindow) != "" },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) carryOutcome {
			if rec.AutoCompactWindow == nil {
				return carryUnrecorded
			}
			window, err := harness.ResolveAutoCompactWindow(h, strings.TrimSpace(*rec.AutoCompactWindow))
			if err != nil {
				return carryDropped
			}
			p.AutoCompactWindow = window
			return carryApplied
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
		switch field.carry(h, recorded, params) {
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
		case carryUnrecorded:
		}
	}
	// Tell the operator what this resume is reproducing. Carrying a launch
	// posture they did not type is the correct behaviour, but it must not be
	// invisible: --sandbox and --ask-for-approval in particular decide how
	// confined the pane is, and a human who typed a bare `-r` deserves to see
	// which postures came back with it.
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
