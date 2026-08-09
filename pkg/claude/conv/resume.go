package conv

import (
	"fmt"
	"os"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common"
)

type ResumeParams struct {
	ConvID   string `pos:"true" help:"Conversation ID to resume (can be short prefix)"`
	Global   bool   `short:"g" help:"Search for conversation across all projects"`
	Detached bool   `short:"d" long:"detached" help:"Start detached (don't attach to session)"`
	// SendKeys is the deliberate override for the Copilot API drive refusal.
	// First-class rather than an escape hatch: `tclaude agent resume` needs a
	// running daemon, so without this a refusal would wall a human out of a pane
	// at exactly the moment agentd is the thing that is broken. See
	// resumeCopilotDriveGate.
	SendKeys bool `long:"send-keys" help:"Proceed even though this conversation chose the Copilot API drive: this resume is on tmux send-keys either way, and without the flag a live managed agent is refused rather than launched. The API channel exists only under tclaude agentd, so an agent resumed this way keeps holding its mail until it is relaunched with 'tclaude agent resume'"`
}

func ResumeCmd() *cobra.Command {
	return boa.CmdT[ResumeParams]{
		Use:         "resume",
		Short:       "Resume a conversation",
		Long:        "Resume a conversation by ID. Finds the conversation, changes to its project directory, and relaunches it through its own harness (claude --resume / codex resume).",
		ParamEnrich: common.DefaultParamEnricher(),
		ValidArgsFunc: func(p *ResumeParams, cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			// Check if -g flag is set (p.Global may not be populated during completion)
			global, _ := cmd.Flags().GetBool("global")
			return clcommon.GetConversationCompletions(global), cobra.ShellCompDirectiveKeepOrder | cobra.ShellCompDirectiveNoFileComp
		},
		RunFunc: func(params *ResumeParams, cmd *cobra.Command, args []string) {
			exitCode := RunResume(params, os.Stdout, os.Stderr)
			if exitCode != 0 {
				os.Exit(exitCode)
			}
		},
	}.ToCobra()
}

// resolvedConv is the harness-agnostic result of resolving a conversation id
// for `tclaude conv resume`: just enough to relaunch the conv through its own
// harness. It unifies the two resolution paths — Claude Code's rich
// conv_index resolver and any non-CC harness's ConvStore — behind one shape
// so runResumeWithSession stays harness-agnostic.
type resolvedConv struct {
	ConvID      string // full conversation id
	ProjectPath string // real working directory to resume in
	DisplayName string // title / summary / first prompt, for the status line
	Harness     string // owning harness ("claude", "codex"); "" coalesces to default
}

// resolveConvForResume maps a (possibly short) conversation id to the conv to
// resume, across every registered harness. Claude Code is tried first through
// its rich conv_index resolver (clcommon.ResolveConvID) — the overwhelmingly
// common case, and the path that carries titles / branch history. If CC
// misses, each non-CC harness's ConvStore.Resolve is consulted in turn
// (same iteration as appendNonClaudeHarnessEntries / agentd's resume path).
//
// The clcommon resolver lives in a package the harness registry imports, so it
// can't reach the registry itself (import cycle) — hence this conv-package
// wrapper is where the two paths are fused.
//
// ConvStore.Resolve's tri-state contract is honored: a resolve error
// (ambiguous prefix OR an unreadable store) is surfaced to the caller, never
// collapsed into "not found". Returns (nil, nil) when no harness recognises
// the id.
func resolveConvForResume(convID string, global bool, cwd string) (*resolvedConv, error) {
	// Claude Code first: its conv_index path is the rich one and the common case.
	if info := clcommon.ResolveConvID(convID, global, cwd); info != nil {
		displayName := info.DisplayTitle
		if displayName == "" {
			displayName = info.FirstPrompt
		}
		return &resolvedConv{
			ConvID:      info.SessionID,
			ProjectPath: info.ProjectPath,
			DisplayName: displayName,
			Harness:     harness.DefaultName,
		}, nil
	}

	// Fall back to every other registered harness's ConvStore.
	for _, name := range harness.Names() {
		if name == harness.DefaultName {
			continue
		}
		h, ok := harness.Get(name)
		if !ok || h.Convs == nil {
			continue
		}
		ref, err := h.Convs.Resolve(convID, cwd, global)
		if err != nil {
			// Ambiguous prefix or unreadable store — surface it rather than
			// swallowing it into the generic "not found" below.
			return nil, err
		}
		if ref == nil {
			continue
		}
		// Title is cosmetic; a lookup miss leaves the status line blank.
		title, _ := h.Convs.Title(ref.ConvID)
		return &resolvedConv{
			ConvID:      ref.ConvID,
			ProjectPath: ref.ProjectPath,
			DisplayName: title,
			Harness:     ref.Harness,
		}, nil
	}

	return nil, nil
}

func RunResume(params *ResumeParams, stdout, stderr *os.File) int {
	// Extract just the ID from autocomplete format (e.g., "0459cd73_[myproject]_prompt..." -> "0459cd73")
	convID := clcommon.ExtractIDFromCompletion(params.ConvID)

	// Get current directory for local search
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "Error getting current directory: %v\n", err)
		return 1
	}

	// Resolve conversation ID to full info, across every harness.
	rc, err := resolveConvForResume(convID, params.Global, cwd)
	if err != nil {
		fmt.Fprintf(stderr, "Error resolving conversation %s: %v\n", convID, err)
		return 1
	}
	if rc == nil {
		fmt.Fprintf(stderr, "Conversation %s not found\n", convID)
		if !params.Global {
			fmt.Fprintf(stderr, "Hint: use -g to search all projects\n")
		}
		return 1
	}

	return runResumeWithSession(rc, !params.Detached, params.SendKeys, stdout, stderr)
}

func runResumeWithSession(rc *resolvedConv, attach, sendKeys bool, stdout, stderr *os.File) int {
	// Check tmux is installed
	if err := session.CheckTmuxInstalled(); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	// Check if hooks are installed (warn if not)
	session.EnsureHooksInstalled(false, stdout, stderr)

	// Sync the configured Claude Code transcript-retention override
	// (claude_cleanup_period_days) into ~/.claude/settings.json. No-op unless
	// set; logs and continues on failure.
	_ = session.EnsureClaudeCleanupPeriod()

	// The session PK carries the FULL conversation identity — never a
	// truncation (two conversations sharing an 8-char prefix would collide on
	// the PK; SaveSession's ON CONFLICT silently overwrites). The tmux name is
	// the short, human-facing handle. See JOH-248.
	sessionID := rc.ConvID

	// Reserve the conversation before launching: this rejects an already-live
	// conv AND serializes against a concurrent resume (otherwise two resumes
	// could both `claude --resume` the same .jsonl → corruption). Keyed on
	// conv_id, it catches the live session whatever its PK shape; the lock is
	// held until the launch returns and the OS frees it if this process dies.
	// See JOH-332.
	release, reject := session.ReserveConvForLaunch(sessionID)
	if reject != nil {
		fmt.Fprintln(stderr, reject.Error())
		return 1
	}
	defer release()

	tmuxSession := session.UniqueTmuxSessionName(session.TmuxNameBase(sessionID, "", rc.ProjectPath))

	// Build the in-tmux launch command via the conv's own harness, mirroring
	// the watch-mode resume (createSessionForConv): a Codex conv relaunches
	// with `codex resume <id>`, Claude Code with `claude --resume <id>`.
	// Resolution failures (an unknown / unspawnable harness) surface here
	// rather than spawning a broken command (JOH-218).
	var stackedProof *session.StackedSandboxProof
	var launchEffectiveSandbox *sandboxpolicy.Snapshot
	launchCmd, profilePath, h, err := resumeLaunchCmdWithStackedProof(
		rc.Harness,
		sessionID,
		rc.ConvID,
		clcommon.ExtractClaudeExtraArgs(),
		&stackedProof,
		&launchEffectiveSandbox,
	)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	if launchEffectiveSandbox != nil {
		for _, notice := range launchEffectiveSandbox.Effective.AccessNotices {
			fmt.Fprintf(stdout, "Warning: %s\n", notice.Detail)
		}
	}
	if stackedProof != nil {
		defer stackedProof.Cleanup()
	}
	// Before anything is launched: a conversation that chose the Copilot API
	// drive cannot get it from here, so this either refuses with the daemon
	// command that can, or discloses the downgrade. Inert — "" and no error —
	// for every conversation that did not choose it. See copilot_drive.go.
	driveNotice, err := resumeCopilotDriveGate(h, rc.ConvID, sendKeys, resumeOverrideHintCLI)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	if driveNotice != "" {
		fmt.Fprintln(stdout, driveNotice)
	}
	approvalState, err := resumeApprovalState(h, rc.ConvID)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	approvalPolicy, autoReview := approvalState.Policy, approvalState.AutoReview
	if notice := describeResumedApproval(h, approvalState); notice != "" {
		fmt.Fprintln(stdout, notice)
	}
	// Read the auto-memory posture BEFORE this resume writes its own session
	// row: AutoMemoryForConv resolves the conv's most-recently-updated row, so
	// reading after the write would just echo the new row's column default and
	// decay an opt-in to off on the next resume. resumeLaunchCmd already
	// applied this same recorded posture to the launch env; re-recording it
	// below keeps the value alive across repeated resumes.
	autoMemory, err := db.AutoMemoryForConv(rc.ConvID)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	// Same read-before-write ordering for the startup-context trims, for exactly
	// the same reason: reading after this resume's own row exists would echo the
	// new row's empty default and quietly un-trim the agent on the next resume.
	contextFeatures, err := db.ContextFeaturesForConv(rc.ConvID)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	// Same read-before-write ordering for the auto-compaction window: reading
	// after this resume's own row exists would echo the new row's empty default
	// and hand the next resume the model's full window back.
	autoCompactWindow, err := db.AutoCompactWindowForConv(rc.ConvID)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	// Same read-before-write ordering for the AskUserQuestion idle-timeout.
	// resumeLaunchCmd already applied it to the launch; recording it on the row
	// below is what keeps it from being asserted away as "known: inherit".
	askTimeout, err := resumeAskTimeout(h, rc.ConvID)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	remoteControl, err := resumeRemoteControl(h, rc.ConvID)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	// Launch through the shared script mechanism, not an inline `sh -c`: the
	// resume command carries the same env exports and sandbox dir lists as a
	// fresh launch, so it has the same tmux ~16KB argv cliff and the same
	// ps-visible-credentials exposure the spawn path already fixed.
	if stackedProof != nil {
		if err := stackedProof.Revalidate(); err != nil {
			fmt.Fprintf(stderr, "%v\n", session.StackedEngineBindingRefusal(h, err))
			return 1
		}
	}
	if err := session.LaunchDetachedTmuxSession(tmuxSession, rc.ProjectPath, launchCmd,
		session.CodexProfileMarkerArgs(profilePath)...); err != nil {
		fmt.Fprintf(stderr, "Failed to create tmux session: %v\n", err)
		return 1
	}
	if stackedProof != nil {
		if err := session.WaitForStackedBindingReadiness(stackedProof.ReadyPath); err != nil {
			_ = clcommon.TmuxCommand(
				"kill-session",
				"-t",
				clcommon.ExactTarget(tmuxSession),
			).Run()
			fmt.Fprintf(stderr, "%v\n", session.StackedEngineBindingRefusal(h, err))
			return 1
		}
		stackedProof.Cleanup()
		stackedProof = nil
	}

	// Get PID and save state (starts as idle, waiting for user input).
	// Carry the resolved harness onto the saved row so a non-claude tag is
	// not coalesced back to "claude" by the DB layer (JOH-218).
	pid := session.ParsePIDFromTmux(tmuxSession)
	// A resume is a fresh launch: re-resolve whether the OS sandbox actually
	// confines it rather than carrying the predecessor's verdict, because the
	// operator may have changed settings.json since (TCL-729).
	resumeMode := resumeHarnessBuiltinMode(rc.ConvID)
	resumeChosenBy := resumeSandboxChosenBy(rc.ConvID)
	resumeImplementation, err := resumeSandboxImplementation(rc.ConvID)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	launchOSSandbox := harness.ResolveLaunchOSSandbox(h, resumeMode, resumeChosenBy, rc.ProjectPath)
	switch resumeImplementation {
	case sandboxpolicy.ImplementationTclaudeLayer:
		launchOSSandbox = resumeTclaudeLayerLaunchOSSandbox(rc.ConvID)
	case sandboxpolicy.ImplementationStacked:
		launchOSSandbox = session.StackedLaunchOSSandbox(
			h, resumeTclaudeLayerNetworkPosture(rc.ConvID),
		)
	}
	state := &session.SessionState{
		ID:                       sessionID,
		TmuxSession:              tmuxSession,
		PID:                      pid,
		Cwd:                      rc.ProjectPath,
		ConvID:                   rc.ConvID,
		Status:                   session.StatusIdle,
		Harness:                  h.Name,
		HarnessBuiltinMode:       resumeMode,
		SandboxImplementation:    string(resumeImplementation),
		HarnessBuiltinModeSource: resumeChosenBy,
		OSSandboxState:           launchOSSandbox.State,
		OSSandboxSource:          launchOSSandbox.Source,
		OSSandboxUnverified:      launchOSSandbox.Unverified,
		EffectiveSandbox:         launchEffectiveSandbox,
		ApprovalPolicy:           approvalPolicy,
		ApprovalAutoReview:       autoReview,
		AskUserQuestionTimeout:   askTimeout,
		Created:                  time.Now(),
		Updated:                  time.Now(),
	}

	if err := session.SaveSessionState(state); err != nil {
		fmt.Fprintf(stderr, "Failed to save session state: %v\n", err)
		return 1
	}
	// Carry the posture onto the row this resume just created. Out-of-band,
	// like the spawn path: SaveSession's UPSERT does not own these columns.
	// Best-effort — the pane is already running with the right env, and a lost
	// write only costs a FUTURE resume its opt-in.
	// What this surface may and may not assert lives in resumeLaunchPosture,
	// shared with the watch-mode resume: the same launch, the same limits, and
	// one place for the decision rather than two copies of it.
	session.RecordLaunchPosture(sessionID, h,
		resumeLaunchPosture(autoMemory, contextFeatures, autoCompactWindow, remoteControl))

	displayName := rc.DisplayName
	if len(displayName) > 50 {
		displayName = displayName[:47] + "..."
	}
	fmt.Fprintf(stdout, "Resuming [%s] in session %s\n", displayName, tmuxSession)
	fmt.Fprintf(stdout, "  Directory: %s\n", rc.ProjectPath)

	if attach {
		fmt.Fprintf(stdout, "\nAttaching... (Ctrl+B D to detach)\n")
		if err := session.AttachToSession(sessionID, tmuxSession, false); err != nil {
			return 1
		}
		return 0
	}

	fmt.Fprintf(stdout, "\nAttach with: tclaude session attach %s\n", tmuxSession)
	return 0
}
