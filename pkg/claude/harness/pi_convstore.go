package harness

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// pi's cold conversation store: its session files (see pi_paths.go for where
// they live and pi_session.go for their format).
//
// A conversation is listed once it holds a user message, so a pane that was
// launched but never prompted does not show up as an empty conversation; pi's
// own /resume picker lists those too. The title is pi's own: the newest
// session_info name (`/name`, `--name`), which is a user-set title and maps
// onto SessionEntry.CustomTitle, so DisplayTitle falls back to the first
// prompt.

type piConvStore struct {
	// roots overrides the session-directory resolution; tests set it.
	roots []string
}

var _ ConvStore = piConvStore{}

// piSessionRoots lists the directories pi may keep sessions in: a session
// directory configured by environment or settings when it is absolute, and
// always the agent directory's default, which keeps the sessions written
// before a custom directory was configured. A relative settings sessionDir is
// per working directory and cannot be enumerated; its sessions are found
// only when tclaude knows the cwd (piFindSessionFile at launch).
func piSessionRoots() []string {
	var roots []string
	add := func(dir string) {
		if dir == "" || !filepath.IsAbs(dir) {
			return
		}
		dir = filepath.Clean(dir)
		for _, r := range roots {
			if r == dir {
				return
			}
		}
		roots = append(roots, dir)
	}
	if dir, err := piSessionsDir(nil, ""); err == nil {
		add(dir)
	}
	if agentDir, err := piAgentDir(nil); err == nil {
		add(filepath.Join(agentDir, "sessions"))
	}
	return roots
}

func (s piConvStore) sessionRoots() []string {
	if s.roots != nil {
		return s.roots
	}
	return piSessionRoots()
}

// piSessionFiles lists the session files under one root: per-cwd
// subdirectories by default, flat under a custom session directory.
func piSessionFiles(root string) []string {
	var files []string
	for _, pattern := range []string{
		filepath.Join(root, "*", "*.jsonl"),
		filepath.Join(root, "*.jsonl"),
	} {
		found, _ := filepath.Glob(pattern)
		files = append(files, found...)
	}
	return files
}

// ListConvs returns one entry per pi conversation. An empty cwd lists all of
// them; otherwise the session header's cwd must match through symlinks. One
// unreadable or malformed file is skipped, since pi writes these files while
// tclaude reads them.
func (s piConvStore) ListConvs(cwd string) ([]convops.SessionEntry, error) {
	entries := s.scan()
	syncPiConvIndex(entries)
	if cwd != "" {
		filter := newCopilotCwdFilter(cwd)
		filtered := entries[:0]
		for _, entry := range entries {
			if filter.matches(entry.ProjectPath) {
				filtered = append(filtered, entry)
			}
		}
		entries = filtered
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Modified != entries[j].Modified {
			return entries[i].Modified > entries[j].Modified
		}
		return entries[i].SessionID < entries[j].SessionID
	})
	return entries, nil
}

// scan reads every session file, keeping the newest file when an id appears
// twice.
func (s piConvStore) scan() []convops.SessionEntry {
	best := map[string]convops.SessionEntry{}
	for _, root := range s.sessionRoots() {
		for _, path := range piSessionFiles(root) {
			entry, ok := readPiSessionEntry(path)
			if !ok {
				continue
			}
			if prior, seen := best[entry.SessionID]; seen && !entry.FileMtime.After(prior.FileMtime) {
				continue
			}
			best[entry.SessionID] = entry
		}
	}
	entries := make([]convops.SessionEntry, 0, len(best))
	for _, entry := range best {
		entries = append(entries, entry)
	}
	return entries
}

// readPiSessionEntry builds the listing entry for one session file, or
// reports false when it is not a listable conversation.
func readPiSessionEntry(path string) (convops.SessionEntry, bool) {
	file, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("pi convstore: session file unreadable; skipped", "path", path, "error", err)
		}
		return convops.SessionEntry{}, false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return convops.SessionEntry{}, false
	}
	session, err := readPiSession(file)
	if err != nil {
		return convops.SessionEntry{}, false
	}
	var (
		firstPrompt  string
		userMessages int
		model        string
		lastActivity int64
	)
	for _, e := range session.Entries {
		if e.Type == "model_change" && e.ModelID != "" {
			model = piModelRef(e.Provider, e.ModelID)
		}
		if e.Type != "message" || e.Message == nil {
			continue
		}
		lastActivity = max(lastActivity, e.Message.Timestamp)
		switch e.Message.Role {
		case "user":
			userMessages++
			if firstPrompt == "" {
				firstPrompt = strings.TrimSpace(piContentText(e.Message.Content))
			}
		case "assistant":
			if e.Message.Model != "" {
				model = piModelRef(e.Message.Provider, e.Message.Model)
			}
		}
	}
	if userMessages == 0 {
		return convops.SessionEntry{}, false
	}
	mtime := info.ModTime().UTC()
	created := geminiTimestamp(session.Header.Timestamp)
	modified := mtime.Format(time.RFC3339)
	if lastActivity > 0 {
		modified = time.UnixMilli(lastActivity).UTC().Format(time.RFC3339)
	}
	if created == "" {
		created = modified
	}
	return convops.SessionEntry{
		SessionID:    session.Header.ID,
		FullPath:     path,
		FileMtime:    mtime,
		FileSize:     info.Size(),
		FirstPrompt:  firstPrompt,
		CustomTitle:  session.Name(),
		MessageCount: userMessages,
		Created:      created,
		Modified:     modified,
		ProjectPath:  filepath.Clean(session.Header.Cwd),
		Harness:      PiName,
		Model:        model,
	}, true
}

// piModelRef renders pi's provider and model id as the `provider/model`
// reference its --model flag takes.
func piModelRef(provider, model string) string {
	if provider == "" {
		return model
	}
	return provider + "/" + model
}

// syncPiConvIndex mirrors the listing into tclaude's conv_index cache and
// overlays the archived flag onto the entries. Upsert-only, for the reason
// Copilot's sync gives: PI_CODING_AGENT_DIR can be repointed, and evicting
// rows the current directory does not mention would destroy tclaude state for
// conversations under another.
//
// The git branch columns are the hooks' and are carried over. A cached title
// is kept while pi's file names none yet: a rename tclaude just delivered is
// cached at once, before pi has processed the `/name`.
func syncPiConvIndex(entries []convops.SessionEntry) {
	if len(entries) == 0 {
		return
	}
	cached := map[string]*db.ConvIndexRow{}
	if rows, err := db.ListAllConvIndex(); err != nil {
		slog.Warn("pi convstore: conv_index unreadable; archived state unavailable", "error", err)
	} else {
		for _, row := range rows {
			if row.Harness == PiName {
				cached[row.ConvID] = row
			}
		}
	}
	for i := range entries {
		row := piEntryDBRow(entries[i])
		if prior := cached[entries[i].SessionID]; prior != nil {
			if !prior.ArchivedAt.IsZero() {
				entries[i].ArchivedAt = prior.ArchivedAt.UTC().Format(time.RFC3339)
			}
			if entries[i].CustomTitle == "" {
				entries[i].CustomTitle = prior.CustomTitle
				row.CustomTitle = prior.CustomTitle
			}
			row.GitBranch, row.GitBranchStartup = prior.GitBranch, prior.GitBranchStartup
		}
		if err := db.UpsertConvIndex(row); err != nil {
			slog.Warn("pi convstore: conv_index upsert failed; continuing from pi",
				"conv", entries[i].SessionID, "error", err)
		}
	}
}

// piEntryDBRow projects a listing entry onto the cache row. ArchivedAt is
// absent on purpose: SetConvIndexArchived owns that column.
func piEntryDBRow(entry convops.SessionEntry) *db.ConvIndexRow {
	return &db.ConvIndexRow{
		ConvID:       entry.SessionID,
		ProjectDir:   filepath.Dir(entry.FullPath),
		FullPath:     entry.FullPath,
		FileMtime:    entry.FileMtime,
		FileSize:     entry.FileSize,
		FirstPrompt:  entry.FirstPrompt,
		CustomTitle:  entry.CustomTitle,
		MessageCount: entry.MessageCount,
		Created:      entry.Created,
		Modified:     entry.Modified,
		ProjectPath:  entry.ProjectPath,
		IndexedAt:    time.Now(),
		Harness:      PiName,
	}
}

// Resolve maps a full id or an id prefix onto a conversation. pi session ids
// are unique across working directories, so `global` only widens which
// conversations are considered.
func (s piConvStore) Resolve(idPrefix, cwd string, global bool) (*ConvRef, error) {
	if idPrefix == "" {
		return nil, nil
	}
	if global {
		cwd = ""
	}
	entries, err := s.ListConvs(cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve pi conversation %q: %w", idPrefix, err)
	}
	var match *convops.SessionEntry
	for i := range entries {
		if entries[i].SessionID == idPrefix {
			return piConvRef(entries[i]), nil
		}
		if !strings.HasPrefix(entries[i].SessionID, idPrefix) {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("ambiguous conversation id %q: matches multiple pi conversations", idPrefix)
		}
		match = &entries[i]
	}
	if match == nil {
		return nil, nil
	}
	return piConvRef(*match), nil
}

func piConvRef(entry convops.SessionEntry) *ConvRef {
	return &ConvRef{ConvID: entry.SessionID, ProjectPath: entry.ProjectPath, Harness: PiName}
}

// find returns the listing entry for one conversation id.
func (s piConvStore) find(convID string) (convops.SessionEntry, bool) {
	for _, entry := range s.scan() {
		if entry.SessionID == convID {
			return entry, true
		}
	}
	return convops.SessionEntry{}, false
}

// Title returns pi's session name, or the first prompt without one. An
// unknown conversation is ("", nil).
func (s piConvStore) Title(convID string) (string, error) {
	if convID == "" {
		return "", nil
	}
	entry, ok := s.find(convID)
	if !ok {
		return "", nil
	}
	return entry.DisplayTitle(), nil
}

// SetTitle names a conversation that no pane is running, the way `/name`
// does in a live one: it appends a session_info entry to the session file,
// parented on the last entry so the current branch stays the current branch.
// A live pi is renamed in-pane instead (Lifecycle.RenameCommand), so this
// never races pi's own appends.
func (s piConvStore) SetTitle(convID, title string) error {
	title = strings.TrimSpace(title)
	if convID == "" || title == "" {
		return fmt.Errorf("pi SetTitle: empty conversation id or title")
	}
	var path string
	for _, root := range s.sessionRoots() {
		if path = piFindSessionFile(root, convID); path != "" {
			break
		}
	}
	if path == "" {
		return fmt.Errorf("pi SetTitle: no session file for %s", convID)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	session, err := decodePiSession(strings.NewReader(string(raw)))
	if err != nil {
		return fmt.Errorf("pi SetTitle: %w", err)
	}
	var parent any
	if n := len(session.Entries); n > 0 {
		parent = session.Entries[n-1].ID
	}
	line, err := json.Marshal(map[string]any{
		"type": "session_info", "id": piNewEntryID(session.byID), "parentId": parent,
		"timestamp": rfc3339ms(time.Now()), "name": title,
	})
	if err != nil {
		return err
	}
	if len(raw) > 0 && raw[len(raw)-1] != '\n' {
		line = append([]byte("\n"), line...)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// piNewEntryID mints an entry id the way pi does: 8 hex characters, unique
// within the session.
func piNewEntryID(taken map[string]*piEntry) string {
	for {
		var b [4]byte
		_, _ = rand.Read(b[:])
		id := hex.EncodeToString(b[:])
		if _, ok := taken[id]; !ok {
			return id
		}
	}
}

// Exists reports whether the conversation is listable. cwd is ignored: ids
// are unique across working directories.
func (s piConvStore) Exists(convID, _ string) (bool, error) {
	if convID == "" {
		return false, nil
	}
	_, ok := s.find(convID)
	return ok, nil
}
