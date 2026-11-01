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
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/opencodeapi"
	"golang.org/x/sync/errgroup"
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

// OpenCodePrivateEnvironment is the environment that selects an allocation's
// private state. agentd owns that layout and sets this.
var OpenCodePrivateEnvironment func(*db.OpenCodeAgentStateAllocation) ([]string, error)

type openCodeConvStore struct {
	// listSessions lists the project of dir; "" is this process's own.
	listSessions func(dir string) ([]openCodeSession, error)
	// listPrivate lists the project of dir in one agent's private state.
	listPrivate func(dir string, environment []string) ([]openCodeSession, error)
	writeTitle  func(db.OpenCodeRuntime, string, string) error
}

var _ ConvStore = openCodeConvStore{}

// openCodeListConcurrency bounds the CLI processes one listing starts.
const openCodeListConcurrency = 4

// sessions lists dirs. `opencode session list` shows only the project of its
// working directory, so a store-wide view has to ask from each project, but one
// answer covers every directory of that project (worktrees, subdirectories), so
// a directory an answer already named is not asked again. A dir that fails
// leaves its rows unseen, and the cache sync does not read unseen as gone: the
// listing fails only when nothing answered.
func (s openCodeConvStore) sessions(dirs ...string) ([]openCodeSession, error) {
	list := s.listSessions
	if list == nil {
		executable, err := OpenCodeExecutable()
		if err != nil {
			return nil, fmt.Errorf("find OpenCode executable: %w", err)
		}
		list = func(dir string) ([]openCodeSession, error) { return listOpenCodeSessions(executable, dir, nil) }
	}
	seen := map[string]bool{}
	covered := map[string]bool{}
	var sessions []openCodeSession
	var failure error
	answered := false
	for len(dirs) > 0 {
		batch := dirs[:min(len(dirs), openCodeListConcurrency)]
		dirs = dirs[len(batch):]
		listed := make([][]openCodeSession, len(batch))
		errs := make([]error, len(batch))
		var group errgroup.Group
		for i, dir := range batch {
			group.Go(func() error {
				listed[i], errs[i] = list(dir)
				return nil
			})
		}
		_ = group.Wait()
		for i := range batch {
			if errs[i] != nil {
				slog.Warn("opencode convstore: listing a project failed; its rows stay unseen", "dir", batch[i], "error", errs[i])
				failure = errs[i]
				continue
			}
			answered = true
			for _, session := range listed[i] {
				covered[session.Directory] = true
				if !seen[session.ID] {
					seen[session.ID] = true
					sessions = append(sessions, session)
				}
			}
		}
		dirs = slices.DeleteFunc(dirs, func(dir string) bool { return covered[dir] })
	}
	if !answered {
		return nil, failure
	}
	return sessions, nil
}

// openCodeKnownDirs is every directory tclaude knows OpenCode conversations
// in: what it has indexed, and what it recorded at a landing nothing has listed
// yet. A directory that has since gone is left out, because the CLI cannot run
// there.
func openCodeKnownDirs() []string {
	var dirs []string
	if rows, err := db.ListAllConvIndex(); err != nil {
		slog.Warn("opencode convstore: conv_index unreadable; listing fewer projects", "error", err)
	} else {
		for _, row := range rows {
			if row.Harness == OpenCodeName {
				dirs = append(dirs, row.ProjectPath)
			}
		}
	}
	if recorded, err := db.ConversationResumeCwds(OpenCodeName); err != nil {
		slog.Warn("opencode convstore: resume profiles unreadable; listing fewer projects", "error", err)
	} else {
		dirs = append(dirs, recorded...)
	}
	return existingDirs(dirs)
}

// openCodeConvDirs is where tclaude last knew convID to be.
func openCodeConvDirs(convID string) []string {
	var dirs []string
	if row, err := db.GetConvIndex(convID); err == nil && row != nil {
		dirs = append(dirs, row.ProjectPath)
	}
	if profile, err := db.ConversationResumeProfileForConv(convID); err == nil && profile != nil && profile.Harness == OpenCodeName {
		dirs = append(dirs, profile.Cwd)
	}
	return existingDirs(dirs)
}

func existingDirs(dirs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, dir := range dirs {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			out = append(out, dir)
		}
	}
	return out
}

// openCodeIsolation shuts a CLI run off from every config source, on top of
// --pure. It matches what the filtered server gets in agentd. The auth content
// matters as much as the rest: a data directory holds auth.json, and a wellknown
// entry there is remote config OpenCode fetches and acts on.
var openCodeIsolation = []string{
	"OPENCODE_PURE=1",
	"OPENCODE_DISABLE_PROJECT_CONFIG=1",
	"OPENCODE_CONFIG=",
	"OPENCODE_CONFIG_DIR=",
	"OPENCODE_DISABLE_AUTOUPDATE=1",
	"OPENCODE_DISABLE_MODELS_FETCH=1",
	"OPENCODE_AUTH_CONTENT={}",
}

// openCodeCLIEnvironment is the environment of a CLI run tclaude starts itself.
// Such a run is outside any sandbox, and the project directory and an agent's
// private state are both writable by that agent, so nothing there may be loaded
// as config. A run against private state takes only its data directory; the
// agent's config, cache and state are replaced by fresh directories, removed by
// the returned cleanup.
//
// OpenCode still opens the database in that data directory, which the agent can
// write. This closes code loading, not a hostile database.
func openCodeCLIEnvironment(environment []string) ([]string, func(), error) {
	env := append(os.Environ(), openCodeIsolation...)
	var data string
	for _, entry := range environment {
		if value, ok := strings.CutPrefix(entry, "XDG_DATA_HOME="); ok {
			data = value
		}
	}
	if data == "" {
		return append(env, environment...), func() {}, nil
	}
	fresh, err := os.MkdirTemp("", "tclaude-opencode-cli-*")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(fresh) }
	env = append(env, "XDG_DATA_HOME="+data)
	for name, dir := range map[string]string{"XDG_CONFIG_HOME": "config", "XDG_CACHE_HOME": "cache", "XDG_STATE_HOME": "state"} {
		path := filepath.Join(fresh, dir)
		if err = os.Mkdir(path, 0o700); err != nil {
			cleanup()
			return nil, nil, err
		}
		env = append(env, name+"="+path)
	}
	return env, cleanup, nil
}

// listOpenCodeSessions deliberately uses the supported CLI instead of reading
// opencode.db. OpenCode already migrated its private store from JSON files to
// SQLite once; pinning tclaude to that schema would turn a future migration
// into a silent empty conversation list.
func listOpenCodeSessions(executable, dir string, environment []string) ([]openCodeSession, error) {
	env, cleanup, err := openCodeCLIEnvironment(environment)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), openCodeConvStoreTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable,
		"session", "list", "--format", "json", "--pure", "--log-level", "ERROR")
	cmd.Dir = dir
	cmd.Env = env
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
	dirs := []string{cwd}
	if cwd == "" {
		// Spelled out, so that a known directory is not listed twice.
		dirs = openCodeKnownDirs()
		if own, err := os.Getwd(); err != nil {
			dirs = append(dirs, "")
		} else if !slices.Contains(dirs, own) {
			dirs = append(dirs, own)
		}
	} else if len(existingDirs(dirs)) == 0 {
		// The CLI cannot run from a directory that has gone.
		dirs = []string{""}
	}
	listedAt := time.Now()
	sessions, err := s.sessions(dirs...)
	if err != nil {
		return nil, err
	}
	entries, err := syncOpenCodeConvIndex(sessions, listedAt)
	if err != nil {
		return nil, err
	}
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
// snapshot, but only in directories the snapshot covers: `opencode session list`
// shows just the project of its working directory, so another project's rows
// are unseen, not gone. So is a conversation in an agent's private state, which
// the CLI never shows, and a row written since listedAt, which the snapshot was
// too early for. A row that has not changed is not rewritten, so that a poll of
// the cache's newest indexed_at does not see every listing as news. Cache failures degrade enrichment rather than hiding source CLI
// sessions. Timestamp representation failures are logged at error level with
// the source values and skip the cache row, so the invalid instant stays loud
// without letting one bad third-party value blank the source listing.
func syncOpenCodeConvIndex(sessions []openCodeSession, listedAt time.Time) ([]convops.SessionEntry, error) {
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
	covered := make(map[string]bool, len(sessions))
	entries := make([]convops.SessionEntry, 0, len(sessions))
	for _, session := range sessions {
		seen[session.ID] = true
		covered[session.Directory] = true
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
			// OpenCode's supported session-list API has no branch field. The
			// managed event projector records the live branch out of band from
			// the runtime cwd, so carry that enrichment through this cold-store
			// refresh instead of replacing it with an empty value.
			entry.GitBranch = row.GitBranch
			entry.GitBranchStartup = row.GitBranchStartup
		}
		if row := cached[session.ID]; row != nil && openCodeRowCurrent(row, entry) {
			entries = append(entries, entry)
			continue
		}
		if err := db.UpsertConvIndex(openCodeEntryDBRow(entry)); err != nil {
			if db.IsTimestampRepresentationError(err) {
				slog.Error("opencode convstore: timestamp cannot be represented; cache row skipped",
					"conv", session.ID, "created_millis", session.Created,
					"updated_millis", session.Updated, "error", err)
			} else {
				slog.Warn("opencode convstore: conv_index upsert failed; continuing from CLI",
					"conv", session.ID, "error", err)
			}
		}
		entries = append(entries, entry)
	}
	for id, row := range cached {
		if !seen[id] && covered[filepath.Clean(row.ProjectPath)] && row.IndexedAt.Before(listedAt) && !openCodeConvInPrivateState(id) {
			if err := db.DeleteConvIndex(id); err != nil {
				slog.Warn("opencode convstore: stale conv_index eviction failed",
					"conv", id, "error", err)
			}
		}
	}
	return entries, nil
}

func openCodeRowCurrent(row *db.ConvIndexRow, entry convops.SessionEntry) bool {
	return row.Summary == entry.Summary && row.CustomTitle == entry.CustomTitle &&
		row.Created == entry.Created && row.Modified == entry.Modified &&
		row.ProjectPath == entry.ProjectPath
}

// openCodePrivateAllocation is the private state of a conversation's agent, nil
// when it keeps none.
func openCodePrivateAllocation(convID string) *db.OpenCodeAgentStateAllocation {
	agentID, err := db.AgentIDForConv(convID)
	if err != nil || agentID == "" {
		return nil
	}
	allocation, err := db.GetOpenCodeAgentStateAllocation(agentID)
	if err != nil || allocation == nil || allocation.Mode != db.OpenCodeStatePrivate {
		return nil
	}
	return allocation
}

func openCodeConvInPrivateState(convID string) bool {
	return openCodePrivateAllocation(convID) != nil
}

// findPrivate looks a conversation up in its agent's private state, from the
// directories tclaude knows it in. Its listing stays out of the cache sync: it
// covers a directory without showing what the shared store holds there. A lookup
// that could not run is an error, never a conversation that is gone.
func (s openCodeConvStore) findPrivate(convID string, allocation *db.OpenCodeAgentStateAllocation) (*convops.SessionEntry, error) {
	if OpenCodePrivateEnvironment == nil {
		return nil, fmt.Errorf("opencode private state: no environment for conversation %s", convID)
	}
	environment, err := OpenCodePrivateEnvironment(allocation)
	if err != nil {
		return nil, fmt.Errorf("opencode private state: %w", err)
	}
	list := s.listPrivate
	if list == nil {
		executable, err := OpenCodeExecutable()
		if err != nil {
			return nil, fmt.Errorf("find OpenCode executable: %w", err)
		}
		list = func(dir string, environment []string) ([]openCodeSession, error) {
			return listOpenCodeSessions(executable, dir, environment)
		}
	}
	dirs := openCodeConvDirs(convID)
	if rows, err := db.FindSessionsByConvID(convID); err == nil {
		var known []string
		for _, row := range rows {
			known = append(known, row.Cwd)
		}
		dirs = existingDirs(append(dirs, known...))
	}
	if len(dirs) == 0 {
		return nil, fmt.Errorf("opencode private state: no directory to look for conversation %s in", convID)
	}
	var failure error
	for _, dir := range dirs {
		sessions, err := list(dir, environment)
		if err != nil {
			failure = err
			continue
		}
		for _, session := range sessions {
			if session.ID != convID {
				continue
			}
			entry := openCodeSessionEntry(session)
			if row, err := db.GetConvIndex(convID); err == nil && row != nil && row.CustomTitle != session.Title {
				entry.CustomTitle = row.CustomTitle
			}
			return &entry, nil
		}
	}
	return nil, failure
}

func openCodeSessionEntry(session openCodeSession) convops.SessionEntry {
	return convops.SessionEntry{
		SessionID:    session.ID,
		FileMtime:    openCodeMillisToTime(session.Updated),
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
		ConvID:           entry.SessionID,
		ProjectDir:       entry.ProjectPath,
		FullPath:         entry.FullPath,
		FileMtime:        entry.FileMtime,
		FileSize:         entry.FileSize,
		FirstPrompt:      entry.FirstPrompt,
		Summary:          entry.Summary,
		CustomTitle:      entry.CustomTitle,
		MessageCount:     entry.MessageCount,
		Created:          entry.Created,
		Modified:         entry.Modified,
		ProjectPath:      entry.ProjectPath,
		GitBranch:        entry.GitBranch,
		GitBranchStartup: entry.GitBranchStartup,
		IndexedAt:        time.Now(),
		Harness:          OpenCodeName,
	}
}

func openCodeMillisToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).Round(0).UTC()
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
	if allocation := openCodePrivateAllocation(idPrefix); allocation != nil {
		entry, err := s.findPrivate(idPrefix, allocation)
		if err != nil {
			return nil, fmt.Errorf("resolve OpenCode conversation %q: %w", idPrefix, err)
		}
		if entry == nil {
			return nil, nil
		}
		return openCodeConvRef(*entry), nil
	}
	if cwd == "" {
		if entry := s.findNearby(idPrefix); entry != nil {
			return openCodeConvRef(*entry), nil
		}
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
	entry, err := s.find(convID)
	if entry != nil {
		return entry.DisplayTitle(), nil
	}
	// A rename the CLI cannot see the conversation for is still a rename.
	if row, rowErr := db.GetConvIndex(convID); rowErr == nil && row != nil && row.CustomTitle != "" {
		return row.CustomTitle, nil
	}
	return "", err
}

// findNearby looks convID up where tclaude last knew it to be, which takes
// one CLI process.
func (s openCodeConvStore) findNearby(convID string) *convops.SessionEntry {
	for _, dir := range openCodeConvDirs(convID) {
		entries, err := s.ListConvs(dir)
		if err != nil {
			slog.Warn("opencode convstore: listing the conversation's project failed", "dir", dir, "error", err)
			continue
		}
		for i := range entries {
			if entries[i].SessionID == convID {
				return &entries[i]
			}
		}
	}
	return nil
}

// find is findNearby, then every project. A conversation in private state is
// in no ambient listing, so it is looked up there instead.
func (s openCodeConvStore) find(convID string) (*convops.SessionEntry, error) {
	if allocation := openCodePrivateAllocation(convID); allocation != nil {
		return s.findPrivate(convID, allocation)
	}
	if entry := s.findNearby(convID); entry != nil {
		return entry, nil
	}
	entries, err := s.ListConvs("")
	if err != nil {
		return nil, err
	}
	for i := range entries {
		if entries[i].SessionID == convID {
			return &entries[i], nil
		}
	}
	return nil, nil
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
			return db.SetConvIndexCustomTitle(convID, title, OpenCodeName)
		} else {
			slog.Warn("opencode SetTitle: live API unavailable; using local title cache",
				"conv", convID, "error", err)
		}
	} else if !openCodeConvInPrivateState(convID) {
		// tclaude recorded a private conversation when it landed, and a rename
		// of it should not depend on its store being readable right now.
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
	response, err := opencodeapi.Do(client, request, runtime)
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

func (s openCodeConvStore) Exists(convID, _ string) (bool, error) {
	if convID == "" {
		return false, nil
	}
	entry, err := s.find(convID)
	return entry != nil, err
}
