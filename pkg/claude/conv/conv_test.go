package conv

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseJSONLSession_ExtractsProjectPathAndGitBranch(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "b6075720-0f5f-46e5-b382-efb055c6d25b"
	filePath := filepath.Join(tmpDir, sessionID+".jsonl")

	content := `{"type":"user","cwd":"/home/alice/git/myproject","sessionId":"b6075720-0f5f-46e5-b382-efb055c6d25b","gitBranch":"main","message":{"role":"user","content":"please say hello"},"timestamp":"2026-02-01T00:35:46.567Z"}
{"type":"assistant","cwd":"/home/alice/git/myproject","sessionId":"b6075720-0f5f-46e5-b382-efb055c6d25b","gitBranch":"main","message":{"role":"assistant","content":"Hello!"},"timestamp":"2026-02-01T00:35:48.557Z"}
`
	require.NoError(t, os.WriteFile(filePath, []byte(content), 0644), "Failed to write test file")

	entry := ParseJSONLSessionPublic(filePath, sessionID)
	require.NotNil(t, entry, "ParseJSONLSessionPublic returned nil")

	assert.Equal(t, "/home/alice/git/myproject", entry.ProjectPath)
	assert.Equal(t, "main", entry.GitBranch)
	assert.Equal(t, "please say hello", entry.FirstPrompt)
}

func TestParseJSONLSession_ExtractsFromFirstMessageWithData(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "test-session-id-00000000000000000"
	filePath := filepath.Join(tmpDir, sessionID+".jsonl")

	content := `{"type":"file-history-snapshot","timestamp":"2026-02-01T00:35:46.000Z"}
{"type":"user","cwd":"/projects/myapp","gitBranch":"feature-branch","message":{"role":"user","content":"test prompt"},"timestamp":"2026-02-01T00:35:46.567Z"}
`
	require.NoError(t, os.WriteFile(filePath, []byte(content), 0644), "Failed to write test file")

	entry := ParseJSONLSessionPublic(filePath, sessionID)
	require.NotNil(t, entry, "ParseJSONLSessionPublic returned nil")

	assert.Equal(t, "/projects/myapp", entry.ProjectPath)
	assert.Equal(t, "feature-branch", entry.GitBranch)
}

func TestParseJSONLSession_FindsCustomTitle(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	filePath := filepath.Join(tmpDir, sessionID+".jsonl")

	content := `{"type":"user","cwd":"/myproject","message":{"role":"user","content":"hello"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":"Hi!"},"timestamp":"2026-03-01T10:00:05Z"}
{"type":"progress","timestamp":"2026-03-01T10:00:06Z"}
{"type":"system","timestamp":"2026-03-01T10:00:07Z"}
{"type":"custom-title","customTitle":"initial-greeting","sessionId":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}
`
	require.NoError(t, os.WriteFile(filePath, []byte(content), 0644), "Failed to write test file")

	entry := ParseJSONLSessionPublic(filePath, sessionID)
	require.NotNil(t, entry, "ParseJSONLSessionPublic returned nil")

	assert.Equal(t, "hello", entry.FirstPrompt)
	assert.Equal(t, "initial-greeting", entry.CustomTitle)
}

func TestParseJSONLSession_FindsSummaryAfterPrompt(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	filePath := filepath.Join(tmpDir, sessionID+".jsonl")

	content := `{"type":"user","cwd":"/myproject","gitBranch":"main","message":{"role":"user","content":"fix the bug"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":"Sure, let me look."},"timestamp":"2026-03-01T10:00:05Z"}
{"type":"summary","summary":"Fixed a critical authentication bug","timestamp":"2026-03-01T10:05:00Z"}
`
	require.NoError(t, os.WriteFile(filePath, []byte(content), 0644), "Failed to write test file")

	entry := ParseJSONLSessionPublic(filePath, sessionID)
	require.NotNil(t, entry, "ParseJSONLSessionPublic returned nil")

	assert.Equal(t, "fix the bug", entry.FirstPrompt)
	assert.Equal(t, "Fixed a critical authentication bug", entry.Summary)
	assert.Equal(t, "/myproject", entry.ProjectPath)
	assert.Equal(t, "main", entry.GitBranch)
}

func TestParseJSONLSession_StopsWhenAllFound(t *testing.T) {
	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	filePath := filepath.Join(tmpDir, sessionID+".jsonl")

	content := `{"type":"summary","summary":"Resumed session summary","timestamp":"2026-03-01T09:59:00Z"}
{"type":"user","cwd":"/myproject","message":{"role":"user","content":"continue where we left off"},"timestamp":"2026-03-01T10:00:00Z"}
{"type":"assistant","message":{"role":"assistant","content":"OK"},"timestamp":"2026-03-01T10:00:05Z"}
`
	require.NoError(t, os.WriteFile(filePath, []byte(content), 0644), "Failed to write test file")

	entry := ParseJSONLSessionPublic(filePath, sessionID)
	require.NotNil(t, entry, "ParseJSONLSessionPublic returned nil")

	assert.Equal(t, "Resumed session summary", entry.Summary)
	assert.Equal(t, "continue where we left off", entry.FirstPrompt)
}

func TestParseJSONLSession_FindsSummaryAfterManyLines(t *testing.T) {
	// Summary at end of file after many assistant messages should be found
	tmpDir := t.TempDir()
	sessionID := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	filePath := filepath.Join(tmpDir, sessionID+".jsonl")

	lines := `{"type":"user","cwd":"/proj","message":{"role":"user","content":"refactor the auth module"},"timestamp":"2026-03-01T10:00:00Z"}
`
	for i := 0; i < 100; i++ {
		lines += `{"type":"assistant","message":{"role":"assistant","content":"working on it..."},"timestamp":"2026-03-01T10:00:05Z"}
`
	}
	lines += `{"type":"summary","summary":"Refactored auth module for better separation of concerns","timestamp":"2026-03-01T11:00:00Z"}
`
	require.NoError(t, os.WriteFile(filePath, []byte(lines), 0644), "Failed to write test file")

	entry := ParseJSONLSessionPublic(filePath, sessionID)
	require.NotNil(t, entry, "ParseJSONLSessionPublic returned nil")

	assert.Equal(t, "refactor the auth module", entry.FirstPrompt)
	assert.Equal(t, "Refactored auth module for better separation of concerns", entry.Summary)
}
