package common

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ConvEntry represents a conversation entry for completions
type ConvEntry struct {
	SessionID   string `json:"sessionId"`
	FirstPrompt string `json:"firstPrompt"`
	Summary     string `json:"summary,omitempty"`
	CustomTitle string `json:"customTitle,omitempty"`
	ProjectPath string `json:"projectPath"`
	Modified    string `json:"modified"`
}

// DisplayTitle returns the best available title
func (e *ConvEntry) DisplayTitle() string {
	if e.CustomTitle != "" {
		return e.CustomTitle
	}
	if e.Summary != "" {
		return e.Summary
	}
	return e.FirstPrompt
}

// HasTitle returns true if the entry has a title or summary
func (e *ConvEntry) HasTitle() bool {
	return e.CustomTitle != "" || e.Summary != ""
}

// ClaudeProjectsDir returns the Claude projects directory path
func ClaudeProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// GetConversationCompletions returns completions for conversation IDs
func GetConversationCompletions(global bool) []string {
	var entries []ConvEntry

	if global {
		projectsDir := ClaudeProjectsDir()
		dirEntries, err := os.ReadDir(projectsDir)
		if err != nil {
			return nil
		}

		for _, dirEntry := range dirEntries {
			if !dirEntry.IsDir() {
				continue
			}
			projPath := filepath.Join(projectsDir, dirEntry.Name())
			loaded := loadConvEntries(projPath)
			entries = append(entries, loaded...)
		}
	} else {
		cwd, err := os.Getwd()
		if err != nil {
			return nil
		}

		projectPath := getClaudeProjectPath(cwd)
		entries = loadConvEntries(projectPath)
	}

	// Sort by modified date descending (most recent first)
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Modified > entries[j].Modified
	})

	// Format completions
	results := make([]string, len(entries))
	for i, e := range entries {
		results[i] = FormatConvCompletion(e)
	}

	return results
}

func loadConvEntries(projectPath string) []ConvEntry {
	indexPath := filepath.Join(projectPath, "sessions-index.json")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return nil
	}

	var index struct {
		Entries []ConvEntry `json:"entries"`
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return nil
	}

	return index.Entries
}

func getClaudeProjectPath(realPath string) string {
	absPath, err := filepath.Abs(realPath)
	if err != nil {
		absPath = realPath
	}
	projectDir := strings.ReplaceAll(absPath, string(filepath.Separator), "-")
	projectDir = strings.ReplaceAll(projectDir, ":", "")
	return filepath.Join(ClaudeProjectsDir(), projectDir)
}

// ExtractIDFromCompletion extracts just the ID from autocomplete format
// e.g., "0459cd73_[title]_prompt..." -> "0459cd73"
func ExtractIDFromCompletion(s string) string {
	if idx := strings.Index(s, "_"); idx > 0 {
		return s[:idx]
	}
	return s
}

// ConvInfo contains resolved conversation information
type ConvInfo struct {
	SessionID    string // Full UUID
	ProjectPath  string // Original project directory
	FirstPrompt  string // First user prompt
	DisplayTitle string // Title or summary for display
}

// ResolveConvID resolves a short conversation ID prefix to full info
// If global is true, searches all projects; otherwise only searches cwd's project
func ResolveConvID(shortID string, global bool, cwd string) *ConvInfo {
	if shortID == "" {
		return nil
	}

	if global {
		return resolveConvIDGlobal(shortID)
	}
	return resolveConvIDLocal(shortID, cwd)
}

func resolveConvIDGlobal(shortID string) *ConvInfo {
	projectsDir := ClaudeProjectsDir()
	dirEntries, err := os.ReadDir(projectsDir)
	if err != nil {
		return nil
	}

	for _, dirEntry := range dirEntries {
		if !dirEntry.IsDir() {
			continue
		}
		projPath := filepath.Join(projectsDir, dirEntry.Name())
		if info := findConvInProject(shortID, projPath); info != nil {
			return info
		}
	}

	return nil
}

func resolveConvIDLocal(shortID string, cwd string) *ConvInfo {
	projPath := getClaudeProjectPath(cwd)
	return findConvInProject(shortID, projPath)
}

func findConvInProject(shortID, projPath string) *ConvInfo {
	entries := loadConvEntries(projPath)

	for _, e := range entries {
		// Exact match or prefix match
		if e.SessionID == shortID || strings.HasPrefix(e.SessionID, shortID) {
			return &ConvInfo{
				SessionID:    e.SessionID,
				ProjectPath:  e.ProjectPath,
				FirstPrompt:  e.FirstPrompt,
				DisplayTitle: e.DisplayTitle(),
			}
		}
	}
	return nil
}

// ExtractClaudeExtraArgs finds the first '--' in os.Args and returns everything after it.
// These args are forwarded directly to the claude binary.
func ExtractClaudeExtraArgs() []string {
	for i, arg := range os.Args {
		if arg == "--" {
			return os.Args[i+1:]
		}
	}
	return nil
}

// ShouldRunClaudeDirect returns true when the extra args indicate claude should be
// run directly in the foreground, bypassing tmux/session management (e.g. --help, --version).
func ShouldRunClaudeDirect(extraArgs []string) bool {
	for _, arg := range extraArgs {
		switch arg {
		case "--help", "-h", "--version":
			return true
		}
	}
	return false
}

// ShellQuoteArg quotes a single argument for safe inclusion in a sh -c command string.
func ShellQuoteArg(s string) string {
	if !strings.ContainsAny(s, " \t\n\"'\\$`|&;<>(){}*?[]#~!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// FormatConvCompletion formats a conversation entry for shell completion
func FormatConvCompletion(e ConvEntry) string {
	sanitize := func(s string) string {
		s = strings.ReplaceAll(s, "\t", "__")
		s = strings.ReplaceAll(s, " ", "_")
		s = strings.ReplaceAll(s, "\n", "_")
		s = strings.ReplaceAll(s, "\r", "")
		return s
	}

	id := e.SessionID
	if len(id) > 8 {
		id = id[:8]
	}

	var namePart string
	if e.HasTitle() {
		namePart = "[" + sanitize(e.DisplayTitle()) + "]_"
	}

	prompt := sanitize(e.FirstPrompt)
	if len(prompt) > 40 {
		prompt = prompt[:37] + "..."
	}

	modified := e.Modified
	if len(modified) >= 16 {
		modified = strings.ReplaceAll(modified[:16], "T", "_")
	}

	return id + "_" + namePart + prompt + "__" + modified
}
