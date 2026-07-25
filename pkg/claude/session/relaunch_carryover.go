package session

import (
	"fmt"
	"log/slog"
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
	// supplied reports whether params already carries a caller value, for
	// callers that did not come through Cobra. It is a fallback, not a
	// substitute: a bool flag explicitly set to false is indistinguishable from
	// an omitted one here, which is exactly why `explicit` exists.
	supplied func(*NewParams) bool
	// carry writes the recorded value onto params, reporting whether it did.
	// It answers false when the posture records nothing for this field (nil =
	// unknown, and unknown must stay unknown) and when the recorded value no
	// longer applies to the harness this relaunch will actually use.
	carry func(*harness.Harness, *db.AgentRelaunchProfile, *NewParams) bool
}

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
		flag:     "sandbox",
		recorded: "SandboxMode",
		// --permission-profile is the same decision spelled differently (and is
		// mutually exclusive with --sandbox), so a caller who passed it owns the
		// containment posture and must not have a recorded --sandbox added under it.
		supplied: func(p *NewParams) bool {
			return strings.TrimSpace(p.Sandbox) != "" || strings.TrimSpace(p.PermissionProfile) != ""
		},
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) bool {
			if rec.SandboxMode == nil {
				return false
			}
			mode, err := harness.ValidateSandboxMode(h, strings.TrimSpace(*rec.SandboxMode))
			if err != nil {
				return false
			}
			p.Sandbox = mode
			return true
		},
	},
	{
		flag:     "ask-for-approval",
		recorded: "ApprovalPolicy",
		supplied: func(p *NewParams) bool { return strings.TrimSpace(p.Approval) != "" },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) bool {
			if rec.ApprovalPolicy == nil {
				return false
			}
			policy, err := harness.ValidateApprovalPolicy(h, strings.TrimSpace(*rec.ApprovalPolicy))
			if err != nil {
				return false
			}
			p.Approval = policy
			return true
		},
	},
	{
		flag:     "auto-review",
		recorded: "ApprovalAutoReview",
		supplied: func(p *NewParams) bool { return p.AutoReview },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) bool {
			if rec.ApprovalAutoReview == nil {
				return false
			}
			autoReview, err := harness.ResolveAutoReview(h, *rec.ApprovalAutoReview)
			if err != nil {
				return false
			}
			p.AutoReview = autoReview
			return true
		},
	},
	{
		flag:     "tools",
		recorded: "ToolGovernance",
		supplied: func(p *NewParams) bool { return strings.TrimSpace(p.ToolGovernance) != "" },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) bool {
			if rec.ToolGovernance == nil {
				return false
			}
			governance, err := harness.ValidateToolGovernance(h, strings.TrimSpace(*rec.ToolGovernance))
			if err != nil {
				return false
			}
			p.ToolGovernance = governance
			return true
		},
	},
	{
		flag:     "ask-user-question-timeout",
		recorded: "AskUserQuestionTimeout",
		supplied: func(p *NewParams) bool { return strings.TrimSpace(p.AskUserQuestionTimeout) != "" },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) bool {
			if rec.AskUserQuestionTimeout == nil {
				return false
			}
			timeout, err := harness.ResolveAskTimeoutMode(h, strings.TrimSpace(*rec.AskUserQuestionTimeout))
			if err != nil {
				return false
			}
			p.AskUserQuestionTimeout = timeout
			return true
		},
	},
	{
		flag:     "remote-control",
		recorded: "RemoteControl",
		supplied: func(p *NewParams) bool { return p.RemoteControl },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) bool {
			if rec.RemoteControl == nil {
				return false
			}
			remoteControl, err := harness.ResolveRemoteControl(h, *rec.RemoteControl)
			if err != nil {
				return false
			}
			p.RemoteControl = remoteControl
			return true
		},
	},
	{
		flag:     "auto-memory",
		recorded: "AutoMemory",
		supplied: func(p *NewParams) bool { return p.AutoMemory },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) bool {
			if rec.AutoMemory == nil {
				return false
			}
			autoMemory, err := harness.ResolveAutoMemory(h, rec.AutoMemory)
			if err != nil {
				return false
			}
			p.AutoMemory = autoMemory
			return true
		},
	},
	{
		flag:     "context-features",
		recorded: "ContextFeatures",
		supplied: func(p *NewParams) bool { return strings.TrimSpace(p.ContextFeatures) != "" },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) bool {
			if rec.ContextFeatures == nil {
				return false
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
				return false
			}
			p.ContextFeatures = harness.FormatContextFeatures(resolved)
			return true
		},
	},
	{
		flag:     "auto-compact-window",
		recorded: "AutoCompactWindow",
		supplied: func(p *NewParams) bool { return strings.TrimSpace(p.AutoCompactWindow) != "" },
		carry: func(h *harness.Harness, rec *db.AgentRelaunchProfile, p *NewParams) bool {
			if rec.AutoCompactWindow == nil {
				return false
			}
			window, err := harness.ResolveAutoCompactWindow(h, strings.TrimSpace(*rec.AutoCompactWindow))
			if err != nil {
				return false
			}
			p.AutoCompactWindow = window
			return true
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
		if field.carry(h, recorded, params) {
			carried = append(carried, field.flag)
		}
	}
	if len(carried) > 0 {
		slog.Debug("session new: carried recorded launch posture into resume",
			"conv_id", convID, "harness", h.Name, "fields", strings.Join(carried, ","))
	}
	return nil
}

// RecordLaunchPosture persists the launch postures SaveSession's UPSERT
// deliberately does not own, so the durable relaunch profile keeps them for the
// next hop. Every relaunch path must call it, not just the fresh-spawn one: a
// resume mints a NEW session row, and a row that was never annotated reports
// "nothing recorded" — which is how a posture that survived one relaunch
// evaporated at the second (TCL-730).
//
// Best-effort by design. The pane is already running with the right
// environment, so a failed write costs a future relaunch its posture, not this
// session — never worth tearing down a healthy pane over.
func RecordLaunchPosture(sessionID string, h *harness.Harness,
	autoMemory bool, contextFeatures map[string]string, autoCompactWindow string) {
	if h == nil {
		return
	}
	if h.SupportsAutoMemory() {
		if err := db.SetSessionAutoMemory(sessionID, autoMemory); err != nil {
			slog.Warn("failed to record session auto-memory posture",
				"session_id", sessionID, "auto_memory", autoMemory, "error", err)
		}
	}
	if h.SupportsContextFeatures() {
		// Written unconditionally, including the empty map, so the row records
		// "known: trims nothing" rather than a legacy unknown.
		if err := db.SetSessionContextFeatures(sessionID, contextFeatures); err != nil {
			slog.Warn("failed to record session context features",
				"session_id", sessionID,
				"context_features", harness.FormatContextFeatures(contextFeatures), "error", err)
		}
	}
	if h.SupportsAutoCompactWindow() {
		// Written unconditionally, including "", for the same reason: the row must
		// distinguish "known: nothing pinned" from a legacy unknown.
		if err := db.SetSessionAutoCompactWindow(sessionID, autoCompactWindow); err != nil {
			slog.Warn("failed to record session auto-compact window",
				"session_id", sessionID, "auto_compact_window", autoCompactWindow, "error", err)
		}
	}
}
