package agent

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"golang.org/x/term"
)

func init() {
	session.JoinGroupHandler = RunJoinGroup
}

// RunJoinGroup implements explicit --join-group and the directory-discovery
// variants used by a bare `tclaude`. All of them are adapted to SpawnParams and
// handed to RunSpawn, so the terminal entrypoint has the same validation,
// profile precedence, worktree handling, permissions, and wire shape as
// `tclaude agent spawn` and the dashboard modal.
func RunJoinGroup(params *session.NewParams) error {
	if params.Resume != "" {
		return fmt.Errorf("group joining cannot be combined with --resume (spawn always creates a fresh session)")
	}
	if params.Label != "" {
		return fmt.Errorf("group joining picks its own label; --label is not supported")
	}

	group := strings.TrimSpace(params.JoinGroup)
	explicitGroup := group != ""
	if group == "" {
		var err error
		group, err = automaticGroupForDir(params)
		if err != nil {
			return err
		}
		if group == "" {
			return session.ErrNoAutomaticGroupMatch
		}
	}
	if !explicitGroup && !DaemonAvailable() {
		return automaticDaemonFallback(os.Stdin, os.Stderr, term.IsTerminal(int(os.Stdin.Fd())))
	}
	if !explicitGroup {
		canonicalDir, err := canonicalGroupDir(launchDir(params))
		if err != nil {
			return fmt.Errorf("normalize launch directory: %w", err)
		}
		params.Dir = canonicalDir
	}
	params.JoinGroup = group

	spawn := spawnParamsForJoinedSession(params, group)
	resp, rc := RunSpawn(spawn, os.Stdout, os.Stderr, os.Stdin)
	if rc != rcOK {
		return fmt.Errorf("spawn into group %q failed (exit %d)", group, rc)
	}
	if params.Detached || resp == nil {
		return nil
	}

	fmt.Println("\nAttaching... (Ctrl+B D to detach)")
	return session.AttachToSession(resp.Label, resp.TmuxSession, false)
}

func automaticDaemonFallback(in io.Reader, out io.Writer, interactive bool) error {
	if !interactive {
		return fmt.Errorf("agentd is unavailable; pass --no-daemon to start a solo session non-interactively, or see `tclaude agentd serve --help`")
	}
	if !confirmSoloWithoutDaemon(in, out) {
		return fmt.Errorf("agentd is unavailable; session startup canceled (pass --no-daemon to start solo, or see `tclaude agentd serve --help`)")
	}
	return session.ErrNoAutomaticGroupMatch
}

func confirmSoloWithoutDaemon(in io.Reader, out io.Writer) bool {
	fmt.Fprintln(out, "agentd is unavailable, so this session cannot join its matching group or use agent features.")
	fmt.Fprintln(out, "See `tclaude agentd serve --help` for daemon setup, or pass --no-daemon to skip this prompt.")
	fmt.Fprint(out, "Start a solo session anyway? [y/N]: ")
	answer, _ := bufio.NewReader(in).ReadString('\n')
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes"
}

func spawnParamsForJoinedSession(params *session.NewParams, group string) *SpawnParams {
	spawn := &SpawnParams{
		Group:                  group,
		Name:                   params.Name,
		Role:                   params.Role,
		Descr:                  params.Descr,
		InitialMessage:         params.InitialMessage,
		File:                   params.File,
		ReplyTo:                params.ReplyTo,
		Cwd:                    launchDir(params),
		Timeout:                params.SpawnTimeout,
		Profile:                params.Profile,
		SandboxProfile:         params.SandboxProfile,
		OmitSandboxProfiles:    params.OmitSandboxProfiles,
		Worktree:               params.Worktree,
		WorktreeBase:           params.WorktreeBase,
		WorktreeRepo:           params.WorktreeRepo,
		AskHuman:               params.AskHuman,
		AutoFocus:              params.AutoFocus,
		NoGroupContext:         params.NoGroupContext,
		Task:                   params.Task,
		TaskLabel:              params.TaskLabel,
		Effort:                 params.Effort,
		Model:                  params.Model,
		Harness:                params.Harness,
		Sandbox:                params.Sandbox,
		AskUserQuestionTimeout: params.AskUserQuestionTimeout,
		Approval:               params.Approval,
		ToolGovernance:         params.ToolGovernance,
		AutoReview:             params.AutoReview,
		TrustDir:               params.TrustDir,
		RemoteControl:          params.RemoteControl,
		AutoMemory:             params.AutoMemory,
		ContextFeatures:        params.ContextFeatures,
		AutoCompactWindow:      params.AutoCompactWindow,
		ContextWindowMax:       params.ContextWindowMax,
		CopilotAPI:             params.CopilotAPI,
		CodexAppServer:         params.CodexAppServer,
		FastMode:               params.FastMode,
		SandboxImpl:            params.SandboxImpl,
		NoOwner:                params.NoOwner,
	}
	spawn.codexAppServerSpecified = params.CodexAppServerSpecified
	return spawn
}

// automaticGroupForDir finds the single active group whose canonical default
// cwd exactly equals this launch's canonical cwd. Parent/child containment is
// intentionally not a match: a nested repository can have its own group and a
// directory must never ambiguously inherit the nearest-looking parent.
func automaticGroupForDir(params *session.NewParams) (string, error) {
	cwd, err := canonicalGroupDir(launchDir(params))
	if err != nil {
		return "", fmt.Errorf("normalize launch directory: %w", err)
	}
	groups, err := db.ListAgentGroups()
	if err != nil {
		return "", fmt.Errorf("list groups for directory auto-join: %w", err)
	}

	var matches []string
	for _, group := range groups {
		if group.IsArchived() || strings.TrimSpace(group.DefaultCwd) == "" {
			continue
		}
		groupDir, normalizeErr := canonicalGroupDir(group.DefaultCwd)
		if normalizeErr == nil && groupDir == cwd {
			matches = append(matches, group.Name)
		}
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("directory %q matches multiple groups (%s); choose one with --join-group or disable auto-join", cwd, strings.Join(matches, ", "))
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if !params.AutoJoinOrCreateGroup {
		return "", nil
	}

	name := availableDirectoryGroupName(cwd, groups)
	ask, err := ParseAskHuman(params.AskHuman)
	if err != nil {
		return "", err
	}
	if rc := RequireDaemonOrExit(os.Stderr); rc != rcOK {
		return "", fmt.Errorf("daemon required to create directory group %q", name)
	}
	var created struct {
		Name string `json:"name"`
	}
	if err := DaemonRequest(http.MethodPost, "/v1/groups", map[string]any{
		"name":        name,
		"default_cwd": cwd,
	}, &created, DaemonOpts{AskHuman: ask}); err != nil {
		return "", fmt.Errorf("create group %q for %q: %w", name, cwd, err)
	}
	fmt.Printf("Created group %q for %s\n", created.Name, cwd)
	params.Dir = cwd
	return created.Name, nil
}

func launchDir(params *session.NewParams) string {
	if dir := strings.TrimSpace(params.Dir); dir != "" {
		return dir
	}
	wd, _ := os.Getwd()
	return wd
}

// canonicalGroupDir follows the same durable identity convention used by the
// worktree cleanup guards: absolute + clean, with symlinks resolved when the
// directory exists. Both the stored group path and launch path take this route.
func canonicalGroupDir(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		raw = filepath.Join(home, strings.TrimPrefix(raw, "~/"))
	}
	abs, err := filepath.Abs(raw)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if real, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
		abs = filepath.Clean(real)
	}
	return abs, nil
}

func availableDirectoryGroupName(cwd string, groups []*db.AgentGroup) string {
	base := NormalizeSpawnName(filepath.Base(cwd))
	if base == "" {
		base = "group"
	}
	taken := make(map[string]bool, len(groups))
	for _, group := range groups {
		taken[group.Name] = true
	}
	if !taken[base] {
		return base
	}
	for suffix := 2; ; suffix++ {
		tail := "-" + strconv.Itoa(suffix)
		stem := base
		if len(stem)+len(tail) > MaxSpawnNameLen {
			stem = strings.TrimRight(stem[:MaxSpawnNameLen-len(tail)], "-")
		}
		candidate := stem + tail
		if !taken[candidate] {
			return candidate
		}
	}
}
