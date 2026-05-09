package conv

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common"
)

type RenameParams struct {
	Selector string `pos:"true" help:"Conversation: full UUID, prefix, current title, or '.' for $TCLAUDE_SESSION_ID"`
	Name     string `pos:"true" optional:"true" help:"New title (required unless --strip)"`
	Global   bool   `short:"g" help:"Search all projects, not just cwd"`
	Yes      bool   `short:"y" help:"Don't prompt when overwriting an existing custom title"`
	Strip    bool   `short:"s" long:"strip" help:"Clear the custom title (revert to summary/first-prompt)"`
}

// renameMaxLen caps title length to avoid pathological inputs. 200 is generous;
// `conv ls` truncates to fit the terminal anyway.
const renameMaxLen = 200

func RenameCmd() *cobra.Command {
	return boa.CmdT[RenameParams]{
		Use:     "rename",
		Aliases: []string{"rn"},
		Short:   "Rename a Claude conversation (set custom-title)",
		Long: `Set a custom title on a conversation.

If the conversation has a live tmux session, drives Claude Code's own
'/rename' slash command via tmux send-keys so the UI updates immediately.
Otherwise appends a 'custom-title' line to the .jsonl file.

Selector resolution order:
  1. Full conversation UUID
  2. Unique short ID prefix
  3. '.' or '-'  - the current session: tries to read the parent Claude
                  process's session file, then falls back to
                  $TCLAUDE_SESSION_ID
  4. Exact match against current custom title or summary

Examples:
  tclaude conv rename abc12345 "[PR:my-org/my-repo/pull/417] fix river ctx"
  tclaude conv rename . "[Merged:my-org/my-repo/pull/417] fix river ctx"
  tclaude conv rename --strip abc12345`,
		ParamEnrich: common.DefaultParamEnricher(),
		ValidArgsFunc: func(p *RenameParams, cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			if len(args) == 0 {
				global, _ := cmd.Flags().GetBool("global")
				return clcommon.GetConversationCompletions(global), cobra.ShellCompDirectiveKeepOrder | cobra.ShellCompDirectiveNoFileComp
			}
			return nil, cobra.ShellCompDirectiveNoFileComp
		},
		RunFunc: func(params *RenameParams, cmd *cobra.Command, args []string) {
			exitCode := RunRename(params, os.Stdout, os.Stderr, os.Stdin)
			if exitCode != 0 {
				os.Exit(exitCode)
			}
		},
	}.ToCobra()
}

// Exit codes match what's documented in docs/plans/conv-rename.md.
const (
	rcOK           = 0
	rcNotFound     = 1
	rcAmbiguous    = 2
	rcInvalidName  = 3
	rcIOFailure    = 4
	rcMissingArg   = 5
)

// RunRename is the testable entry point for `conv rename`.
func RunRename(params *RenameParams, stdout, stderr, stdin *os.File) int {
	// Validate args
	if !params.Strip && params.Name == "" {
		fmt.Fprintf(stderr, "Error: new name is required (or pass --strip)\n")
		return rcMissingArg
	}
	if params.Strip && params.Name != "" {
		fmt.Fprintf(stderr, "Error: --strip cannot be combined with a new name\n")
		return rcMissingArg
	}

	newName := params.Name
	if !params.Strip {
		if err := validateTitle(newName); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return rcInvalidName
		}
	}

	// Resolve selector to a single conversation
	resolved, status := resolveSelectorForRename(params.Selector, params.Global, stderr)
	if status != rcOK {
		return status
	}

	jsonlPath := filepath.Join(resolved.projectDir, resolved.convID+".jsonl")
	if _, err := os.Stat(jsonlPath); err != nil {
		fmt.Fprintf(stderr, "Error: conversation file not found: %s\n", jsonlPath)
		return rcNotFound
	}

	// Confirm overwrite of existing custom title
	if !params.Yes && resolved.row != nil && resolved.row.CustomTitle != "" && !params.Strip {
		fmt.Fprintf(stdout, "Conversation %s currently has title: %s\n", resolved.convID[:8], resolved.row.CustomTitle)
		fmt.Fprintf(stdout, "Overwrite with: %s? [y/N]: ", newName)
		reader := bufio.NewReader(stdin)
		response, _ := reader.ReadString('\n')
		response = strings.TrimSpace(strings.ToLower(response))
		if response != "y" && response != "yes" {
			fmt.Fprintf(stdout, "Aborted.\n")
			return rcOK
		}
	}

	// Preferred path: if the conv has an alive tmux session, drive CC's own
	// `/rename` slash command via tmux send-keys. CC then writes the
	// custom-title/agent-name lines and refreshes its in-memory cache, so
	// the UI updates live. We can't do this for --strip (no tmux equivalent).
	if !params.Strip {
		if pushRenameToTmux(resolved.convID, newName) {
			fmt.Fprintf(stdout, "Renamed %s -> %s (via /rename in live CC session)\n", resolved.convID[:8], newName)
			return rcOK
		}
	}

	// Fallback: append a custom-title line directly. Empty title acts as a
	// "strip" tombstone — the parser's last-write-wins semantics treats it
	// as cleared.
	if err := appendCustomTitle(jsonlPath, resolved.convID, newName); err != nil {
		fmt.Fprintf(stderr, "Error writing to conversation file: %v\n", err)
		return rcIOFailure
	}

	// Refresh DB cache so subsequent `conv ls` reflects the new title
	// without rescanning the .jsonl on next listing.
	if entry := ScanAndUpsertFile(jsonlPath); entry == nil {
		fmt.Fprintf(stderr, "Warning: title written but DB cache refresh failed\n")
	}

	if params.Strip {
		fmt.Fprintf(stdout, "Stripped custom title from %s\n", resolved.convID[:8])
	} else {
		fmt.Fprintf(stdout, "Renamed %s -> %s\n", resolved.convID[:8], newName)
	}
	return rcOK
}

// pushRenameToTmux finds the live tmux pane for this conversation and sends
// `/rename <name>` followed by Enter. Returns true if the keys were
// successfully delivered. Caller falls back to file-based rename on false.
//
// Caveat: keystrokes interleave with whatever the user is typing in CC's
// input box. The agent that triggers this (via a skill) should be aware.
func pushRenameToTmux(convID, newName string) bool {
	row, err := db.FindSessionByConvID(convID)
	if err != nil || row == nil || row.TmuxSession == "" {
		return false
	}
	if !session.IsTmuxSessionAlive(row.TmuxSession) {
		return false
	}

	target := row.TmuxSession + ":0.0"
	// Send the slash command as a literal string, then Enter.
	cmd := clcommon.TmuxCommand("send-keys", "-t", target, "/rename "+newName, "Enter")
	if err := cmd.Run(); err != nil {
		return false
	}
	return true
}

// ccSessionFile returns the path to ~/.claude/sessions/<pid>.json — CC's
// per-process session state file. We read it (for `sessionId`) but no longer
// write to it; the tmux send-keys path handles live UI updates.
func ccSessionFile(pid int) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "sessions", fmt.Sprintf("%d.json", pid))
}

// readCCSessionFile loads ~/.claude/sessions/<pid>.json into a generic map.
func readCCSessionFile(pid int) (map[string]any, error) {
	data, err := os.ReadFile(ccSessionFile(pid))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// currentCCSessionID returns the conv UUID for the parent CC process, or ""
// if it can't be determined.
func currentCCSessionID() string {
	pid := session.FindClaudePID()
	if pid == 0 {
		return ""
	}
	m, err := readCCSessionFile(pid)
	if err != nil {
		return ""
	}
	id, _ := m["sessionId"].(string)
	return id
}

// validateTitle checks the name fits the constraints documented in the plan.
func validateTitle(name string) error {
	if name == "" {
		return fmt.Errorf("title is empty")
	}
	if len(name) > renameMaxLen {
		return fmt.Errorf("title is too long (%d > %d chars)", len(name), renameMaxLen)
	}
	if strings.ContainsAny(name, "\n\r\x00") {
		return fmt.Errorf("title contains newline or null byte")
	}
	return nil
}

// resolvedConv is a minimal handle to a conversation we're about to mutate.
type resolvedConv struct {
	convID     string         // full UUID
	projectDir string         // absolute path to ~/.claude/projects/<slug>
	row        *db.ConvIndexRow // may be nil if conv not yet indexed
}

func resolveSelectorForRename(selector string, global bool, stderr *os.File) (*resolvedConv, int) {
	if selector == "" {
		fmt.Fprintf(stderr, "Error: selector is required\n")
		return nil, rcMissingArg
	}

	// Strip autocomplete suffix (e.g. "abc12345_[title]_prompt..." -> "abc12345").
	selector = clcommon.ExtractIDFromCompletion(selector)

	// '.' or '-' = current session. We try, in order:
	//   1. Walk up the process tree to find the parent CC PID, then read its
	//      ~/.claude/sessions/<pid>.json — that's the actual current conv,
	//      which can differ from $TCLAUDE_SESSION_ID after `/clear` or
	//      `/resume` inside the tmux session.
	//   2. Fall back to $TCLAUDE_SESSION_ID, which only matches the conv ID
	//      tclaude originally launched the session with.
	if selector == "." || selector == "-" {
		if id := currentCCSessionID(); id != "" {
			selector = id
		} else if envID := os.Getenv("TCLAUDE_SESSION_ID"); envID != "" {
			selector = envID
		} else {
			fmt.Fprintf(stderr, "Error: could not detect current Claude Code session; pass an explicit conv ID\n")
			return nil, rcNotFound
		}
	}

	// Strategy 1: lookup as UUID (full or prefix)
	if r := lookupByID(selector, global); r != nil {
		return r, rcOK
	}

	// Strategy 2: match by current title (custom title, summary, or first prompt).
	matches := lookupByTitle(selector, global)
	if len(matches) == 1 {
		return matches[0], rcOK
	}
	if len(matches) > 1 {
		fmt.Fprintf(stderr, "Error: selector %q matches %d conversations:\n", selector, len(matches))
		for _, m := range matches {
			title := ""
			if m.row != nil {
				title = displayTitleForRow(m.row)
			}
			fmt.Fprintf(stderr, "  %s  %s\n", m.convID[:8], title)
		}
		fmt.Fprintf(stderr, "Disambiguate by ID prefix.\n")
		return nil, rcAmbiguous
	}

	fmt.Fprintf(stderr, "Error: no conversation matches %q\n", selector)
	if !global {
		fmt.Fprintf(stderr, "Hint: use -g to search across all projects\n")
	}
	return nil, rcNotFound
}

// lookupByID resolves a UUID or prefix using the SQLite cache.
func lookupByID(idOrPrefix string, global bool) *resolvedConv {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	cwdProjectDir := GetClaudeProjectPath(cwd)

	// Exact match first
	if row, err := db.GetConvIndex(idOrPrefix); err == nil && row != nil {
		if global || row.ProjectDir == cwdProjectDir {
			return &resolvedConv{convID: row.ConvID, projectDir: row.ProjectDir, row: row}
		}
	}

	// Prefix match (uniqueness enforced by db.FindConvIndexByPrefix)
	row, err := db.FindConvIndexByPrefix(idOrPrefix)
	if err != nil || row == nil {
		return nil
	}
	if !global && row.ProjectDir != cwdProjectDir {
		return nil
	}
	return &resolvedConv{convID: row.ConvID, projectDir: row.ProjectDir, row: row}
}

// lookupByTitle finds entries whose current display title (custom-title, summary,
// or first prompt) matches selector exactly.
func lookupByTitle(selector string, global bool) []*resolvedConv {
	var rows []*db.ConvIndexRow
	if global {
		rows, _ = db.ListAllConvIndex()
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return nil
		}
		rows, _ = db.ListConvIndex(GetClaudeProjectPath(cwd))
	}

	var matches []*resolvedConv
	for _, r := range rows {
		if titleMatches(r, selector) {
			matches = append(matches, &resolvedConv{convID: r.ConvID, projectDir: r.ProjectDir, row: r})
		}
	}
	return matches
}

func titleMatches(r *db.ConvIndexRow, selector string) bool {
	if r.CustomTitle != "" && r.CustomTitle == selector {
		return true
	}
	if r.Summary != "" && r.Summary == selector {
		return true
	}
	if r.FirstPrompt != "" && r.FirstPrompt == selector {
		return true
	}
	return false
}

func displayTitleForRow(r *db.ConvIndexRow) string {
	if r.CustomTitle != "" {
		return r.CustomTitle
	}
	if r.Summary != "" {
		return r.Summary
	}
	return r.FirstPrompt
}

// On-disk shapes Claude Code uses for title state. CC's `/title` writes both
// lines paired; `custom-title` is what tclaude/external tools read, and
// `agent-name` is what CC's UI reads as the session display name. We emit
// both so the rename takes effect everywhere.
type customTitleLine struct {
	Type        string `json:"type"`
	CustomTitle string `json:"customTitle"`
	SessionID   string `json:"sessionId"`
}

type agentNameLine struct {
	Type      string `json:"type"`
	AgentName string `json:"agentName"`
	SessionID string `json:"sessionId"`
}

// appendCustomTitle appends paired `custom-title` and `agent-name` lines to
// the conversation file. Uses O_APPEND so concurrent writes from a running
// Claude Code session are safe (each line is small and append is atomic on
// POSIX). Writes them in a single Write so they can't interleave with CC's
// own writes.
func appendCustomTitle(jsonlPath, sessionID, title string) error {
	f, err := os.OpenFile(jsonlPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()

	custom, err := json.Marshal(customTitleLine{
		Type:        "custom-title",
		CustomTitle: title,
		SessionID:   sessionID,
	})
	if err != nil {
		return err
	}
	agent, err := json.Marshal(agentNameLine{
		Type:      "agent-name",
		AgentName: title,
		SessionID: sessionID,
	})
	if err != nil {
		return err
	}

	payload := append(custom, '\n')
	payload = append(payload, agent...)
	payload = append(payload, '\n')
	if _, err := f.Write(payload); err != nil {
		return err
	}

	// Bump mtime explicitly in case the write happened quickly enough to land
	// in the same filesystem-second as the last cached mtime (rare but
	// possible on coarse-resolution filesystems).
	now := time.Now()
	_ = os.Chtimes(jsonlPath, now, now)
	return nil
}
