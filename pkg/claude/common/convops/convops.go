// Package convops provides conversation file operations shared between packages.
package convops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// SessionEntry represents a single session/conversation in the index
type SessionEntry struct {
	SessionID    string `json:"sessionId"`
	FullPath     string `json:"fullPath"`
	FileMtime    int64  `json:"fileMtime"`
	FirstPrompt  string `json:"firstPrompt"`
	Summary      string `json:"summary,omitempty"`
	CustomTitle  string `json:"customTitle,omitempty"`
	MessageCount int    `json:"messageCount"`
	Created      string `json:"created"`
	Modified     string `json:"modified"`
	GitBranch    string `json:"gitBranch"`
	ProjectPath  string `json:"projectPath"`
	IsSidechain  bool   `json:"isSidechain"`
}

// DisplayTitle returns the best available title for display
func (e *SessionEntry) DisplayTitle() string {
	if e.CustomTitle != "" {
		return e.CustomTitle
	}
	if e.Summary != "" {
		return e.Summary
	}
	return e.FirstPrompt
}

// HasTitle returns true if the entry has a custom title or summary
func (e *SessionEntry) HasTitle() bool {
	return e.CustomTitle != "" || e.Summary != ""
}

// SessionsIndex represents the sessions-index.json file
type SessionsIndex struct {
	Version int            `json:"version"`
	Entries []SessionEntry `json:"entries"`
}

// ClaudeProjectsDir returns the Claude projects directory path
func ClaudeProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// PathToProjectDir converts a real path to the Claude project directory name
func PathToProjectDir(realPath string) string {
	absPath, err := filepath.Abs(realPath)
	if err != nil {
		absPath = realPath
	}
	projectDir := strings.ReplaceAll(absPath, string(filepath.Separator), "-")
	projectDir = strings.ReplaceAll(projectDir, ":", "")
	return projectDir
}

// GetClaudeProjectPath returns the full path to a Claude project directory
func GetClaudeProjectPath(realPath string) string {
	return filepath.Join(ClaudeProjectsDir(), PathToProjectDir(realPath))
}

// LoadSessionsIndex loads the sessions index from a Claude project directory
func LoadSessionsIndex(projectPath string) (*SessionsIndex, error) {
	indexPath := filepath.Join(projectPath, "sessions-index.json")
	data, err := os.ReadFile(indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &SessionsIndex{Version: 1, Entries: []SessionEntry{}}, nil
		}
		return nil, err
	}

	var index SessionsIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return nil, err
	}
	return &index, nil
}

// SaveSessionsIndex saves the sessions index to a Claude project directory
func SaveSessionsIndex(projectPath string, index *SessionsIndex) error {
	indexPath := filepath.Join(projectPath, "sessions-index.json")
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(indexPath, data, 0600)
}

// FindSessionByID finds a session entry by its ID (full or prefix)
func FindSessionByID(index *SessionsIndex, sessionID string) (*SessionEntry, int) {
	// First try exact match
	for i, entry := range index.Entries {
		if entry.SessionID == sessionID {
			return &index.Entries[i], i
		}
	}
	// Then try prefix match
	var matches []int
	for i, entry := range index.Entries {
		if strings.HasPrefix(entry.SessionID, sessionID) {
			matches = append(matches, i)
		}
	}
	if len(matches) == 1 {
		return &index.Entries[matches[0]], matches[0]
	}
	return nil, -1
}

// RemoveSessionByID removes a session from the index by its ID
func RemoveSessionByID(index *SessionsIndex, sessionID string) bool {
	for i, entry := range index.Entries {
		if entry.SessionID == sessionID {
			index.Entries = append(index.Entries[:i], index.Entries[i+1:]...)
			return true
		}
	}
	return false
}

// CopyConversationFile copies a conversation file and updates sessionId references
func CopyConversationFile(src, dst, oldID, newID string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	content := strings.ReplaceAll(string(data), oldID, newID)
	return os.WriteFile(dst, []byte(content), 0600)
}

// CopyDir recursively copies a directory
func CopyDir(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dst, srcInfo.Mode()); err != nil {
		return err
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		srcPath := filepath.Join(src, entry.Name())
		dstPath := filepath.Join(dst, entry.Name())

		if entry.IsDir() {
			if err := CopyDir(srcPath, dstPath); err != nil {
				return err
			}
		} else {
			if err := CopyFile(srcPath, dstPath); err != nil {
				return err
			}
		}
	}

	return nil
}

// CopyFile copies a single file
func CopyFile(src, dst string) error {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return err
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}

	return os.WriteFile(dst, data, srcInfo.Mode())
}

// GenerateUUID generates a new UUID v4
func GenerateUUID() string {
	return uuid.New().String()
}

// FormatTime returns current time in RFC3339 format (local time)
func FormatTime() string {
	return time.Now().Format(time.RFC3339)
}

// CopyConversationResult contains the result of copying a conversation
type CopyConversationResult struct {
	NewConvID      string
	DstProjectPath string
}

// CopyConversationToPath copies a conversation to a new project path
func CopyConversationToPath(convID, destPath string, global bool) (*CopyConversationResult, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	var srcEntry *SessionEntry
	var srcProjectPath string
	dstProjectPath := GetClaudeProjectPath(destPath)

	if global {
		projectsDir := ClaudeProjectsDir()
		entries, err := os.ReadDir(projectsDir)
		if err != nil {
			return nil, err
		}

		for _, dirEntry := range entries {
			if !dirEntry.IsDir() {
				continue
			}
			projPath := projectsDir + "/" + dirEntry.Name()
			index, err := LoadSessionsIndex(projPath)
			if err != nil {
				continue
			}
			if found, _ := FindSessionByID(index, convID); found != nil {
				srcEntry = found
				srcProjectPath = projPath
				break
			}
		}
	} else {
		srcProjectPath = GetClaudeProjectPath(cwd)
		srcIndex, err := LoadSessionsIndex(srcProjectPath)
		if err != nil {
			return nil, err
		}
		srcEntry, _ = FindSessionByID(srcIndex, convID)
	}

	if srcEntry == nil {
		return nil, os.ErrNotExist
	}

	// Create destination directory if needed
	if err := os.MkdirAll(dstProjectPath, 0700); err != nil {
		return nil, err
	}

	// Load or create destination index
	dstIndex, err := LoadSessionsIndex(dstProjectPath)
	if err != nil {
		dstIndex = &SessionsIndex{Version: 1, Entries: []SessionEntry{}}
	}

	// Generate new UUID
	newConvID := GenerateUUID()
	oldConvID := srcEntry.SessionID

	// Copy conversation file
	srcConvFile := filepath.Join(srcProjectPath, oldConvID+".jsonl")
	dstConvFile := filepath.Join(dstProjectPath, newConvID+".jsonl")

	if err := CopyConversationFile(srcConvFile, dstConvFile, oldConvID, newConvID); err != nil {
		return nil, err
	}

	// Copy conversation directory if exists
	srcConvDir := filepath.Join(srcProjectPath, oldConvID)
	dstConvDir := filepath.Join(dstProjectPath, newConvID)
	if info, err := os.Stat(srcConvDir); err == nil && info.IsDir() {
		if err := CopyDir(srcConvDir, dstConvDir); err != nil {
			return nil, err
		}
	}

	// Get file info for new entry
	dstInfo, err := os.Stat(dstConvFile)
	if err != nil {
		return nil, err
	}

	// Create new entry
	now := FormatTime()
	newEntry := SessionEntry{
		SessionID:    newConvID,
		FullPath:     dstConvFile,
		FileMtime:    dstInfo.ModTime().UnixMilli(),
		FirstPrompt:  srcEntry.FirstPrompt,
		Summary:      srcEntry.Summary,
		CustomTitle:  srcEntry.CustomTitle,
		MessageCount: srcEntry.MessageCount,
		Created:      now,
		Modified:     now,
		GitBranch:    srcEntry.GitBranch,
		ProjectPath:  destPath,
		IsSidechain:  srcEntry.IsSidechain,
	}

	dstIndex.Entries = append(dstIndex.Entries, newEntry)

	if err := SaveSessionsIndex(dstProjectPath, dstIndex); err != nil {
		return nil, err
	}

	return &CopyConversationResult{
		NewConvID:      newConvID,
		DstProjectPath: dstProjectPath,
	}, nil
}
