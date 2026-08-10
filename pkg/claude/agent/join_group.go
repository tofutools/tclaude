package agent

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
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
	if group == "" {
		var err error
		group, err = automaticGroupForDir(params)
		if err != nil {
			return err
		}
		if group == "" {
			return session.ErrNoAutomaticGroupMatch
		}
		params.JoinGroup = group
	}

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

// automaticGroupForDir finds the single active group whose canonical default
// cwd exactly equals this launch's canonical cwd. Parent/child containment is
// intentionally not a match: a nested repository can have its own group and a
// directory must never ambiguously inherit the nearest-looking parent.
func automaticGroupForDir(params *session.NewParams) (string, error) {
	cwd, err := canonicalGroupDir(launchDir(params))
	if err != nil {
		return "", fmt.Errorf("normalize launch directory: %w", err)
	}
	// A relative spelling was resolved against THIS terminal process. Carry the
	// canonical result into the daemon request so agentd never reinterprets it
	// against its own (possibly different) working directory.
	params.Dir = cwd
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
