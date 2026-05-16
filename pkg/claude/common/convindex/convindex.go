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
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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

// GetConvTitle is a convenience function to look up a conversation title.
// It checks the DB first, then falls back to parsing the .jsonl file directly.
func GetConvTitle(convID, cwd string) string {
	if convID == "" || cwd == "" {
		return ""
	}

	// Try DB first (fast PK lookup)
	if row, err := db.GetConvIndex(convID); err == nil && row != nil {
		entry := &SessionEntry{
			SessionID:   row.ConvID,
			FirstPrompt: row.FirstPrompt,
			Summary:     row.Summary,
			CustomTitle: row.CustomTitle,
		}
		if title := entry.DisplayTitle(); title != "" {
			return cleanTitle(title)
		}
	}

	// Try prefix match in DB
	if row, err := db.FindConvIndexByPrefix(convID); err == nil && row != nil {
		entry := &SessionEntry{
			SessionID:   row.ConvID,
			FirstPrompt: row.FirstPrompt,
			Summary:     row.Summary,
			CustomTitle: row.CustomTitle,
		}
		if title := entry.DisplayTitle(); title != "" {
			return cleanTitle(title)
		}
	}

	// Fallback: parse .jsonl file directly for unindexed conversations
	projectPath := GetClaudeProjectPath(cwd)
	return cleanTitle(parseFirstPromptFromJSONL(projectPath, convID))
}

// FormatTitleAndPrompt formats a title and prompt into "[title]: prompt"
// when both are set, or "[title]" when only the title is set (e.g. a
// freshly /rename'd conv that hasn't seen a user prompt yet). When only
// the prompt is set, returns it raw.
func FormatTitleAndPrompt(title, prompt string) string {
	cleanedTitle := cleanTitle(title)
	cleanedPrompt := cleanTitle(prompt)

	if cleanedTitle != "" && cleanedPrompt != "" {
		return "[" + cleanedTitle + "]: " + cleanedPrompt
	} else if cleanedTitle != "" {
		return "[" + cleanedTitle + "]"
	}
	return cleanedPrompt
}

// FormatConvTitle is THE one canonical "Prompt/Title" display string for
// a conversation. Every surface that shows a conversation title to a
// human — `conv ls`, `conv ls -w`, the web dashboard's Groups/Agents
// tabs — must go through here so the rendering never diverges between
// them again (see issue #91).
//
// The title part is the custom title, else the summary; the first
// prompt is always the prompt part. DisplayTitle's FirstPrompt fallback
// is deliberately NOT folded into the title part — the first prompt is
// already rendered as the prompt part, so reusing it as the title too
// would print it twice. The whole thing is cleaned (system tags and
// newlines stripped) and formatted as "[title]: prompt" / "[title]" /
// "prompt" by FormatTitleAndPrompt.
//
// Note this is distinct from an agent's bare *name* (custom title, else
// summary, else first prompt — used for identity, name derivation and
// reincarnate/clone name prefixes). That stays bare; this is the
// browse-a-conversation rendering.
func FormatConvTitle(customTitle, summary, firstPrompt string) string {
	title := customTitle
	if title == "" {
		title = summary
	}
	return FormatTitleAndPrompt(title, firstPrompt)
}

// GetConvTitleAndPrompt returns both the title (CustomTitle or Summary) and the first prompt.
// Returns formatted string like "[title]: prompt" or just "prompt" if no title.
func GetConvTitleAndPrompt(convID, cwd string) string {
	if convID == "" || cwd == "" {
		return ""
	}

	// Try DB first (exact match, then prefix)
	row, err := db.GetConvIndex(convID)
	if err != nil || row == nil {
		row, _ = db.FindConvIndexByPrefix(convID)
	}
	if row != nil {
		title := ""
		if row.CustomTitle != "" {
			title = row.CustomTitle
		} else if row.Summary != "" {
			title = row.Summary
		}
		return FormatTitleAndPrompt(title, row.FirstPrompt)
	}

	// Fallback: parse .jsonl file directly for unindexed conversations
	projectPath := GetClaudeProjectPath(cwd)
	return cleanTitle(parseFirstPromptFromJSONL(projectPath, convID))
}

// cleanTitle removes XML-like tags and normalizes whitespace for display.
// Does NOT truncate - callers (table rendering, notifications) handle truncation.
func cleanTitle(title string) string {
	if title == "" {
		return ""
	}

	// Remove XML-like tags and their content (system-injected metadata)
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

// SystemTags is a backward-compat re-export of convops.SystemTags.
// The canonical definition now lives in convops so the .jsonl parser
// there doesn't need to back-import convindex.
var SystemTags = convops.SystemTags

// stripXMLTags removes known system XML tags and their content from a string.
// Only strips tags listed in SystemTags; other XML-like content is left intact.
func stripXMLTags(s string) string {
	for _, tag := range SystemTags {
		open := "<" + tag + ">"
		close := "</" + tag + ">"
		for {
			start := strings.Index(s, open)
			if start == -1 {
				break
			}
			end := strings.Index(s[start:], close)
			if end == -1 {
				// No closing tag — remove from open tag to end of string
				s = s[:start]
				break
			}
			s = s[:start] + s[start+end+len(close):]
		}
	}
	return s
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
	defer func() { _ = file.Close() }()

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
