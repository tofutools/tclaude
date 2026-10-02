package harness

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// An ephemeral Gemini resume, for a séance (geminiAsker documents the argv).
//
// `--session-file <path>` imports the conversation at path into a NEW session:
// Gemini keeps only its user and model messages, puts one info message in
// front of them reading "Imported session from <path>", writes the result to
// the project's chats directory under a fresh id, and continues from there.
// The original file is only read. tclaude hands Gemini a private copy, so that
// info message names a path no other session can carry, which is how cleanup
// finds the imported session again without guessing at ids or timing.

var (
	_ EphemeralResumer        = geminiAsker{}
	_ EphemeralSessionRemover = geminiAsker{}
)

// RemoveEphemeralSession deletes every session file holding convID, a fresh
// headless turn's pinned id: Gemini persists each turn as a conversation.
func (geminiAsker) RemoveEphemeralSession(convID string) {
	for {
		path, found, err := LocateGeminiSessionFile(convID)
		if err != nil || !found || os.Remove(path) != nil {
			return
		}
	}
}

// geminiImportMarker is the content prefix of the info message an imported
// session starts with (gemini.tsx resolveSessionId).
const geminiImportMarker = "Imported session from "

// geminiSeanceDirName holds the copies, under Gemini's own state directory so
// they are readable wherever the sandboxed CLI can read its state: inside
// tclaude's sandbox and under Gemini's Seatbelt profile alike.
const geminiSeanceDirName = "tclaude-seance"

func (geminiAsker) PrepareEphemeralResume(convID string) (string, func(), error) {
	noop := func() {}
	source, found, err := LocateGeminiSessionFile(convID)
	if err != nil {
		return "", noop, err
	}
	if !found {
		return "", noop, fmt.Errorf("gemini: no session file holds conversation %s", convID)
	}
	root := geminiDir()
	if root == "" {
		return "", noop, errors.New("gemini: cannot determine the Gemini CLI home directory")
	}
	parent := filepath.Join(root, geminiSeanceDirName)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", noop, err
	}
	// ~/.gemini is writable to a confined Gemini agent, which could plant
	// this directory as a symlink to steer where the daemon writes.
	if info, err := os.Lstat(parent); err != nil || !info.IsDir() {
		return "", noop, fmt.Errorf("gemini: %s is not a plain directory", parent)
	}
	dir, err := os.MkdirTemp(parent, "resume-")
	if err != nil {
		return "", noop, err
	}
	copyPath := filepath.Join(dir, filepath.Base(source))
	if err := copyGeminiSessionFile(source, copyPath); err != nil {
		_ = os.RemoveAll(dir)
		return "", noop, err
	}
	started := time.Now().Add(-time.Second)
	cleanup := func() {
		removeGeminiImportedSessions(copyPath, started)
		_ = os.RemoveAll(dir)
	}
	return copyPath, cleanup, nil
}

func copyGeminiSessionFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// removeGeminiImportedSessions deletes every session file written since
// `since` whose first message is the import marker for importedFrom.
func removeGeminiImportedSessions(importedFrom string, since time.Time) {
	for _, dir := range geminiRuntimeDirs() {
		matches, _ := filepath.Glob(filepath.Join(dir, geminiTmpDirName, "*", geminiChatsDirName, geminiSessionPrefix+"*.json*"))
		for _, path := range matches {
			info, err := os.Stat(path)
			if err != nil || info.ModTime().Before(since) || !geminiIsSessionFile(filepath.Base(path)) {
				continue
			}
			if geminiSessionImportedFrom(path) == importedFrom {
				_ = os.Remove(path)
			}
		}
	}
}

// geminiSessionImportedFrom returns the path an imported session names in its
// first message, "" for any other session.
func geminiSessionImportedFrom(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = file.Close() }()
	reader := bufio.NewReaderSize(io.LimitReader(file, 1<<20), 64<<10)
	// Line one is the session metadata; line two the first message.
	for range 2 {
		line, err := reader.ReadBytes('\n')
		var record struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(line, &record) == nil && record.Type == "info" {
			var content string
			if json.Unmarshal(record.Content, &content) == nil && strings.HasPrefix(content, geminiImportMarker) {
				return strings.TrimPrefix(content, geminiImportMarker)
			}
			return ""
		}
		if err != nil {
			return ""
		}
	}
	return ""
}
