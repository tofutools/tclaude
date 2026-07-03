package conv

import (
	"fmt"
	"os"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common"
)

type ResumeParams struct {
	ConvID   string `pos:"true" help:"Conversation ID to resume (can be short prefix)"`
	Global   bool   `short:"g" help:"Search for conversation across all projects"`
	Detached bool   `short:"d" long:"detached" help:"Start detached (don't attach to session)"`
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

	return runResumeWithSession(rc, !params.Detached, stdout, stderr)
}

func runResumeWithSession(rc *resolvedConv, attach bool, stdout, stderr *os.File) int {
	// Check tmux is installed
	if err := session.CheckTmuxInstalled(); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return 1
	}

	// Check if hooks are installed (warn if not)
	session.EnsureHooksInstalled(false, stdout, stderr)

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
	launchCmd, h, err := resumeLaunchCmd(rc.Harness, sessionID, rc.ConvID, clcommon.ExtractClaudeExtraArgs())
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}

	// Create tmux session
	tmuxArgs := []string{
		"new-session",
		"-d",
		"-s", tmuxSession,
		"-c", rc.ProjectPath,
		"sh", "-c", launchCmd,
	}

	tmuxCmd := clcommon.TmuxCommand(tmuxArgs...)
	if err := tmuxCmd.Run(); err != nil {
		fmt.Fprintf(stderr, "Failed to create tmux session: %v\n", err)
		return 1
	}

	// Get PID and save state (starts as idle, waiting for user input).
	// Carry the resolved harness onto the saved row so a non-claude tag is
	// not coalesced back to "claude" by the DB layer (JOH-218).
	pid := session.ParsePIDFromTmux(tmuxSession)
	state := &session.SessionState{
		ID:          sessionID,
		TmuxSession: tmuxSession,
		PID:         pid,
		Cwd:         rc.ProjectPath,
		ConvID:      rc.ConvID,
		Status:      session.StatusIdle,
		Harness:     h.Name,
		Created:     time.Now(),
		Updated:     time.Now(),
	}

	if err := session.SaveSessionState(state); err != nil {
		fmt.Fprintf(stderr, "Failed to save session state: %v\n", err)
		return 1
	}

	displayName := rc.DisplayName
	if len(displayName) > 50 {
		displayName = displayName[:47] + "..."
	}
	fmt.Fprintf(stdout, "Resuming [%s] in session %s\n", displayName, tmuxSession)
	fmt.Fprintf(stdout, "  Directory: %s\n", rc.ProjectPath)

	if attach {
		fmt.Fprintf(stdout, "\nAttaching... (Ctrl+B D to detach)\n")
		return session.AttachToTmuxSession(tmuxSession)
	}

	fmt.Fprintf(stdout, "\nAttach with: tclaude session attach %s\n", tmuxSession)
	return 0
}
