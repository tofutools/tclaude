// Package convindex provides minimal conversation index lookup functionality.
// This package exists to avoid import cycles between session and conv packages.
package convindex

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
)

// SessionsIndex represents the sessions-index.json file (minimal version for fast loading)
type SessionsIndex struct {
	Version int            `json:"version"`
	Entries []SessionEntry `json:"entries"`
}

// SessionEntry represents a minimal session/conversation entry for fast lookups.
// This is a subset of convops.SessionEntry - only fields needed for title display.
type SessionEntry struct {
	SessionID   string `json:"sessionId"`
	FirstPrompt string `json:"firstPrompt"`
	Summary     string `json:"summary,omitempty"`
	CustomTitle string `json:"customTitle,omitempty"`
}

// DisplayTitle returns the best available title for display
// Priority: CustomTitle -> Summary -> FirstPrompt
func (e *SessionEntry) DisplayTitle() string {
	if e.CustomTitle != "" {
		return e.CustomTitle
	}
	if e.Summary != "" {
		return e.Summary
	}
	return e.FirstPrompt
}

// Re-export path functions from convops for backward compatibility
var (
	ClaudeProjectsDir    = convops.ClaudeProjectsDir
	PathToProjectDir     = convops.PathToProjectDir
	GetClaudeProjectPath = convops.GetClaudeProjectPath
)

// LoadSessionsIndexFast loads just the sessions index JSON without scanning.
func LoadSessionsIndexFast(projectPath string) (*SessionsIndex, error) {
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

// FindSessionByID finds a session entry by its ID (full or prefix)
func FindSessionByID(index *SessionsIndex, sessionID string) *SessionEntry {
	if index == nil {
		return nil
	}
	// First try exact match
	for i, entry := range index.Entries {
		if entry.SessionID == sessionID {
			return &index.Entries[i]
		}
	}
	// Then try prefix match
	var matches []*SessionEntry
	for i, entry := range index.Entries {
		if strings.HasPrefix(entry.SessionID, sessionID) {
			matches = append(matches, &index.Entries[i])
		}
	}
	if len(matches) == 1 {
		return matches[0]
	}
	return nil
}

// GetConvTitle is a convenience function to look up a conversation title.
// It checks the index first, then falls back to parsing the .jsonl file directly.
func GetConvTitle(convID, cwd string) string {
	if convID == "" || cwd == "" {
		return ""
	}

	projectPath := GetClaudeProjectPath(cwd)

	// Try index first
	index, _ := LoadSessionsIndexFast(projectPath)
	if index != nil {
		if entry := FindSessionByID(index, convID); entry != nil {
			if title := entry.DisplayTitle(); title != "" {
				return cleanTitle(title)
			}
		}
	}

	// Fallback: parse .jsonl file directly for unindexed conversations
	return cleanTitle(parseFirstPromptFromJSONL(projectPath, convID))
}

// FormatTitleAndPrompt formats a title and prompt into "[title]: prompt" format.
// If title is empty, returns just the cleaned prompt.
// If prompt is empty, returns just the cleaned title.
// Both title and prompt are cleaned (XML tags removed, truncated).
func FormatTitleAndPrompt(title, prompt string) string {
	cleanedTitle := cleanTitle(title)
	cleanedPrompt := cleanTitle(prompt)

	if cleanedTitle != "" && cleanedPrompt != "" {
		return "[" + cleanedTitle + "]: " + cleanedPrompt
	} else if cleanedTitle != "" {
		return cleanedTitle
	}
	return cleanedPrompt
}

// GetConvTitleAndPrompt returns both the title (CustomTitle or Summary) and the first prompt.
// Returns formatted string like "[title]: prompt" or just "prompt" if no title.
func GetConvTitleAndPrompt(convID, cwd string) string {
	if convID == "" || cwd == "" {
		return ""
	}

	projectPath := GetClaudeProjectPath(cwd)

	// Try index first
	index, _ := LoadSessionsIndexFast(projectPath)
	if index != nil {
		if entry := FindSessionByID(index, convID); entry != nil {
			title := ""
			if entry.CustomTitle != "" {
				title = entry.CustomTitle
			} else if entry.Summary != "" {
				title = entry.Summary
			}
			return FormatTitleAndPrompt(title, entry.FirstPrompt)
		}
	}

	// Fallback: parse .jsonl file directly for unindexed conversations
	return cleanTitle(parseFirstPromptFromJSONL(projectPath, convID))
}

// cleanTitle removes XML-like tags and normalizes whitespace for display.
// Does NOT truncate - callers (table rendering, notifications) handle truncation.
func cleanTitle(title string) string {
	if title == "" {
		return ""
	}

	// Remove XML-like tags (e.g., <local-command-caveat>...</local-command-caveat>)
	result := stripXMLTags(title)

	// Replace newlines and carriage returns with visible marker
	result = strings.ReplaceAll(result, "\r\n", " ↵ ")
	result = strings.ReplaceAll(result, "\n", " ↵ ")
	result = strings.ReplaceAll(result, "\r", " ↵ ")

	// Collapse multiple spaces into one
	for strings.Contains(result, "  ") {
		result = strings.ReplaceAll(result, "  ", " ")
	}

	// Trim whitespace
	return strings.TrimSpace(result)
}

// stripXMLTags removes XML-like tags from a string.
func stripXMLTags(s string) string {
	var result strings.Builder
	inTag := false

	for _, r := range s {
		if r == '<' {
			inTag = true
			continue
		}
		if r == '>' {
			inTag = false
			continue
		}
		if !inTag {
			result.WriteRune(r)
		}
	}

	return result.String()
}

// jsonlMessage represents a message in the JSONL transcript
type jsonlMessage struct {
	Type    string `json:"type"`
	Message struct {
		Role    string `json:"role"`
		Content any    `json:"content"` // Can be string or array
	} `json:"message"`
	Summary string `json:"summary,omitempty"` // Some entries have summary
}

// parseFirstPromptFromJSONL extracts the first user prompt from a .jsonl file
func parseFirstPromptFromJSONL(projectPath, sessionID string) string {
	filePath := filepath.Join(projectPath, sessionID+".jsonl")
	file, err := os.Open(filePath)
	if err != nil {
		return ""
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	// Large buffer for lines with embedded images/screenshots (can exceed 2MB)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		var msg jsonlMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}

		// Check for summary field first
		if msg.Summary != "" {
			return msg.Summary
		}

		// Look for first user message
		if msg.Type == "user" && msg.Message.Role == "user" {
			return extractTextContent(msg.Message.Content)
		}
	}

	return ""
}

// extractTextContent extracts text from message content (can be string or array)
func extractTextContent(content any) string {
	// Direct string
	if s, ok := content.(string); ok {
		return s
	}

	// Array of content blocks
	if arr, ok := content.([]any); ok {
		for _, item := range arr {
			if m, ok := item.(map[string]any); ok {
				if m["type"] == "text" {
					if text, ok := m["text"].(string); ok {
						return text
					}
				}
			}
		}
	}

	return ""
}
