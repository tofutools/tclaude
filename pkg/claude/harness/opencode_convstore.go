package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
)

const openCodeConvStoreTimeout = 15 * time.Second

// openCodeSession is the stable, intentionally small JSON shape exposed by
// `opencode session list --format json` in OpenCode 1.18.4. The CLI is the
// cold-store contract: unlike a managed server, it works when no pane or
// `opencode serve` process is alive.
type openCodeSession struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Updated   int64  `json:"updated"` // Unix milliseconds
	Created   int64  `json:"created"` // Unix milliseconds
	ProjectID string `json:"projectId"`
	Directory string `json:"directory"`
}

type openCodeConvStore struct {
	listSessions func() ([]openCodeSession, error)
	writeTitle   func(db.OpenCodeRuntime, string, string) error
}

var _ ConvStore = openCodeConvStore{}

func (s openCodeConvStore) sessions() ([]openCodeSession, error) {
	if s.listSessions != nil {
		return s.listSessions()
	}
	executable, err := OpenCodeExecutable()
	if err != nil {
		return nil, fmt.Errorf("find OpenCode executable: %w", err)
	}
	return listOpenCodeSessions(executable)
}

// listOpenCodeSessions deliberately uses the supported CLI instead of reading
// opencode.db. OpenCode already migrated its private store from JSON files to
// SQLite once; pinning tclaude to that schema would turn a future migration
// into a silent empty conversation list.
func listOpenCodeSessions(executable string) ([]openCodeSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), openCodeConvStoreTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable,
		"session", "list", "--format", "json", "--pure", "--log-level", "ERROR")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("list OpenCode sessions: %w", ctx.Err())
		}
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("list OpenCode sessions: %w: %s", err, detail)
		}
		return nil, fmt.Errorf("list OpenCode sessions: %w", err)
	}
	sessions, err := parseOpenCodeSessions(output)
	if err != nil {
		return nil, err
	}
	return sessions, nil
}

func parseOpenCodeSessions(output []byte) ([]openCodeSession, error) {
	if len(bytes.TrimSpace(output)) == 0 {
		// On a fresh data root OpenCode 1.18.4 exits successfully without
		// printing the JSON `[]` value. That is its cold "no DB/sessions yet"
		// result, not an unrecognised non-empty schema.
		return []openCodeSession{}, nil
	}
	var sessions []openCodeSession
	if err := json.Unmarshal(output, &sessions); err != nil {
		return nil, fmt.Errorf("decode `opencode session list --format json`: %w", err)
	}
	for i := range sessions {
		if sessions[i].ID == "" || sessions[i].Directory == "" {
			return nil, fmt.Errorf(
				"decode `opencode session list --format json`: session %d lacks id or directory",
				i)
		}
		sessions[i].Directory = filepath.Clean(sessions[i].Directory)
	}
	return sessions, nil
}

// ListConvs maps OpenCode's per-session directory field directly onto
// tclaude's cwd identity. OpenCode's projectId groups repositories internally,
// but directory is the supported resume target and remains distinct for
// sessions created in different subdirectories of the same project.
func (s openCodeConvStore) ListConvs(cwd string) ([]convops.SessionEntry, error) {
	sessions, err := s.sessions()
	if err != nil {
		return nil, err
	}
	entries := syncOpenCodeConvIndex(sessions)
	if cwd == "" {
		return entries, nil
	}
	cwd = filepath.Clean(cwd)
	filtered := make([]convops.SessionEntry, 0, len(entries))
	for _, entry := range entries {
		if filepath.Clean(entry.ProjectPath) == cwd {
			filtered = append(filtered, entry)
		}
	}
	return filtered, nil
}

// syncOpenCodeConvIndex keeps the common cache useful for dashboard and title
// readers without making it the source of truth. A successful CLI snapshot
// inserts/refreshes every OpenCode row and evicts OpenCode rows absent from the
// snapshot. Cache failures only degrade enrichment; they never hide CLI data.
func syncOpenCodeConvIndex(sessions []openCodeSession) []convops.SessionEntry {
	cached := map[string]*db.ConvIndexRow{}
	if rows, err := db.ListAllConvIndex(); err != nil {
		slog.Warn("opencode convstore: conv_index unreadable; continuing from CLI", "error", err)
	} else {
		for _, row := range rows {
			if row.Harness == OpenCodeName {
				cached[row.ConvID] = row
			}
		}
	}

	seen := make(map[string]bool, len(sessions))
	entries := make([]convops.SessionEntry, 0, len(sessions))
	for _, session := range sessions {
		seen[session.ID] = true
		entry := openCodeSessionEntry(session)
		if row := cached[session.ID]; row != nil {
			// CustomTitle is a tclaude-local fallback used only when a cold
			// session cannot be reached over HTTP. Once the native title
			// catches up to the same value, stop treating it as an override.
			if row.CustomTitle != "" && row.CustomTitle != session.Title {
				entry.CustomTitle = row.CustomTitle
			}
			if !row.ArchivedAt.IsZero() {
				entry.ArchivedAt = row.ArchivedAt.UTC().Format(time.RFC3339)
			}
		}
		if err := db.UpsertConvIndex(openCodeEntryDBRow(entry)); err != nil {
			slog.Warn("opencode convstore: conv_index upsert failed",
				"conv", session.ID, "error", err)
		}
		entries = append(entries, entry)
	}
	for id := range cached {
		if !seen[id] {
			if err := db.DeleteConvIndex(id); err != nil {
				slog.Warn("opencode convstore: stale conv_index eviction failed",
					"conv", id, "error", err)
			}
		}
	}
	return entries
}

func openCodeSessionEntry(session openCodeSession) convops.SessionEntry {
	return convops.SessionEntry{
		SessionID:    session.ID,
		FileMtime:    openCodeMillisToUnix(session.Updated),
		Summary:      session.Title,
		MessageCount: 0,
		Created:      openCodeMillisToRFC3339(session.Created),
		Modified:     openCodeMillisToRFC3339(session.Updated),
		ProjectPath:  session.Directory,
		Harness:      OpenCodeName,
	}
}

func openCodeEntryDBRow(entry convops.SessionEntry) *db.ConvIndexRow {
	return &db.ConvIndexRow{
		ConvID:       entry.SessionID,
		ProjectDir:   entry.ProjectPath,
		FullPath:     entry.FullPath,
		FileMtime:    entry.FileMtime,
		FileSize:     entry.FileSize,
		FirstPrompt:  entry.FirstPrompt,
		Summary:      entry.Summary,
		CustomTitle:  entry.CustomTitle,
		MessageCount: entry.MessageCount,
		Created:      entry.Created,
		Modified:     entry.Modified,
		ProjectPath:  entry.ProjectPath,
		IndexedAt:    time.Now(),
		Harness:      OpenCodeName,
	}
}

func openCodeMillisToUnix(ms int64) int64 {
	if ms <= 0 {
		return 0
	}
	return ms / int64(time.Second/time.Millisecond)
}

func openCodeMillisToRFC3339(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

func (s openCodeConvStore) Resolve(idPrefix, cwd string, global bool) (*ConvRef, error) {
	if !couldMatchOpenCodeID(idPrefix) {
		return nil, nil
	}
	if global {
		cwd = ""
	}
	entries, err := s.ListConvs(cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve OpenCode conversation %q: %w", idPrefix, err)
	}
	for _, entry := range entries {
		if entry.SessionID == idPrefix {
			return openCodeConvRef(entry), nil
		}
	}
	var match *convops.SessionEntry
	for i := range entries {
		if !strings.HasPrefix(entries[i].SessionID, idPrefix) {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf(
				"ambiguous conversation id %q: matches multiple OpenCode conversations",
				idPrefix)
		}
		match = &entries[i]
	}
	if match == nil {
		return nil, nil
	}
	return openCodeConvRef(*match), nil
}

func couldMatchOpenCodeID(prefix string) bool {
	return prefix != "" &&
		(strings.HasPrefix("ses_", prefix) || strings.HasPrefix(prefix, "ses_"))
}

func openCodeConvRef(entry convops.SessionEntry) *ConvRef {
	return &ConvRef{
		ConvID:      entry.SessionID,
		ProjectPath: entry.ProjectPath,
		Harness:     OpenCodeName,
	}
}

func (s openCodeConvStore) Title(convID string) (string, error) {
	if convID == "" {
		return "", nil
	}
	entries, err := s.ListConvs("")
	if err != nil {
		return "", err
	}
	for i := range entries {
		if entries[i].SessionID == convID {
			return entries[i].DisplayTitle(), nil
		}
	}
	return "", nil
}

// SetTitle prefers the authenticated API of the managed server. A cold
// conversation has no server to PATCH, so its title is retained in tclaude's
// conv_index overlay and surfaced by ListConvs/Title without modifying
// OpenCode's private SQLite schema.
func (s openCodeConvStore) SetTitle(convID, title string) error {
	if convID == "" {
		return fmt.Errorf("opencode SetTitle: empty conversation id")
	}
	if title == "" {
		return fmt.Errorf("opencode SetTitle: refusing to write an empty title for %s", convID)
	}

	runtime, err := db.GetOpenCodeRuntimeByConvID(convID)
	if err != nil {
		return fmt.Errorf("opencode SetTitle: look up managed runtime: %w", err)
	}
	if runtime != nil {
		writer := s.writeTitle
		if writer == nil {
			writer = writeOpenCodeTitle
		}
		if err := writer(*runtime, convID, title); err == nil {
			return nil
		} else {
			slog.Warn("opencode SetTitle: live API unavailable; using local title cache",
				"conv", convID, "error", err)
		}
	} else {
		exists, err := s.Exists(convID, "")
		if err != nil {
			return fmt.Errorf("opencode SetTitle: verify conversation %s: %w", convID, err)
		}
		if !exists {
			return fmt.Errorf("opencode SetTitle: conversation %s not found", convID)
		}
	}
	return db.SetConvIndexCustomTitle(convID, title, OpenCodeName)
}

func writeOpenCodeTitle(runtime db.OpenCodeRuntime, convID, title string) error {
	endpoint := runtime.ServerURL + "/session/" + url.PathEscape(convID) +
		"?directory=" + url.QueryEscape(runtime.Cwd)
	request, err := opencodeapi.NewRequest(http.MethodPatch, endpoint, runtime,
		map[string]string{"title": title})
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("rename OpenCode session: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return fmt.Errorf("rename OpenCode session: HTTP %d: %s",
			response.StatusCode, strings.TrimSpace(string(detail)))
	}
	return nil
}

func (s openCodeConvStore) Exists(convID, cwd string) (bool, error) {
	if convID == "" {
		return false, nil
	}
	entries, err := s.ListConvs(cwd)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.SessionID == convID {
			return true, nil
		}
	}
	return false, nil
}
