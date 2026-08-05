package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Copilot's cold conversation store.
//
// The layout below is runtime evidence from the pinned 1.0.77 binary running
// credential-free in pkg/claude/harness/copilotfixture, not documentation.
// GitHub documents neither of these files.
//
//	<COPILOT_HOME>/session-state/<session-id>/
//	    workspace.yaml   flat YAML: id, cwd, git_root, repository, host_type,
//	                     branch, client_name, name, user_named, summary_count,
//	                     created_at, updated_at
//	    events.jsonl     append-only event log; a resume APPENDS to the same
//	                     file rather than starting a new one
//
// Two consequences shape this store.
//
// First, it needs no SQLite at all. `session-store.db` mirrors the same
// identity/cwd/repository/branch/summary columns, but every one of them is
// already in `workspace.yaml` — a small, per-session, append-safe file with no
// WAL, no lock, no schema version and no risk of tclaude reading a database
// Copilot is mid-write on. TCL-976 admits read-only SQLite only for fields
// whose authoritative source is the database; for a ConvStore there are none.
//
// Second, the title lives in `workspace.yaml`, not in the event log.
// `session.title_changed` is declared `ephemeral: true` in the CLI's shipped
// session-events.schema.json — "not persisted to the session event log on
// disk" — so an events-only reader would never see a title. `workspace.yaml`
// carries it as `name`, with `user_named` distinguishing an operator title
// (`--name`, `/rename`) from Copilot's own generated summary. That maps
// exactly onto SessionEntry.CustomTitle vs SessionEntry.Summary, so
// DisplayTitle's existing precedence does the right thing without a
// Copilot-specific rule.
//
// The event log is still read, for the three fields workspace.yaml does not
// carry: the first user prompt, the user-turn count, and the model.
const copilotSessionStateDirName = "session-state"

const (
	copilotWorkspaceFileName = "workspace.yaml"
	copilotEventsFileName    = "events.jsonl"
)

type copilotConvStore struct {
	// home overrides COPILOT_HOME resolution. Empty means "resolve normally";
	// tests set it to a fixture root.
	home string
}

var _ ConvStore = copilotConvStore{}

func (s copilotConvStore) sessionStateDir() (string, error) {
	home := s.home
	if home == "" {
		home = copilotHome()
	}
	if home == "" {
		return "", errors.New("copilot: cannot determine COPILOT_HOME")
	}
	return filepath.Join(home, copilotSessionStateDirName), nil
}

// copilotWorkspace is the committed subset of workspace.yaml. Unknown keys are
// ignored rather than rejected: the file is Copilot's, and a future CLI adding
// a key must not blank tclaude's conversation list.
type copilotWorkspace struct {
	ID         string `yaml:"id"`
	Cwd        string `yaml:"cwd"`
	GitRoot    string `yaml:"git_root"`
	Repository string `yaml:"repository"`
	Branch     string `yaml:"branch"`
	Name       string `yaml:"name"`
	UserNamed  bool   `yaml:"user_named"`
	CreatedAt  string `yaml:"created_at"`
	UpdatedAt  string `yaml:"updated_at"`
}

// ListConvs assembles one SessionEntry per session-state directory. An empty
// cwd is the documented "everything, everywhere" sentinel; a non-empty one is
// matched through symlinks rather than by spelling — see copilotCwdFilter for
// why the two sides disagree on macOS, and for what that matching does not
// cover.
//
// A session whose own files are unreadable or malformed is SKIPPED with a
// warning rather than failing the listing: Copilot writes these files while
// tclaude reads them, and one half-written workspace.yaml must not hide every
// other conversation. Only a failure to enumerate the directory itself — the
// store as a whole being unreadable — is an error, since that is the case a
// caller genuinely cannot distinguish from "no conversations".
func (s copilotConvStore) ListConvs(cwd string) ([]convops.SessionEntry, error) {
	return s.listConvs(cwd, true)
}

// listConvs is ListConvs with the event scan made optional.
//
// Only three SessionEntry fields come from events.jsonl — FirstPrompt,
// MessageCount and Model — and a caller that needs none of them should not pay
// to read every session's log. Resolve needs an id and a cwd; Title needs a
// name. Those are workspace.yaml alone, and workspace.yaml is a few hundred
// bytes next to an event log that carries a ~26 kB system prompt per turn.
func (s copilotConvStore) listConvs(cwd string, withEvents bool) ([]convops.SessionEntry, error) {
	stateDir, err := s.sessionStateDir()
	if err != nil {
		return nil, err
	}
	dirEntries, err := os.ReadDir(stateDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Copilot has never run under this home. Not an error: an empty
			// listing is the truthful answer, exactly as for a fresh install.
			return []convops.SessionEntry{}, nil
		}
		return nil, fmt.Errorf("copilot: read %s: %w", stateDir, err)
	}

	// Built before the scan so the caller's cwd is resolved once per listing
	// rather than once per session. Nil for the "everything, everywhere"
	// sentinel, which never compares a directory at all.
	var cwdFilter *copilotCwdFilter
	if cwd != "" {
		cwdFilter = newCopilotCwdFilter(cwd)
	}
	entries := make([]convops.SessionEntry, 0, len(dirEntries))
	for _, dirEntry := range dirEntries {
		if !dirEntry.IsDir() {
			continue
		}
		// A single unreadable session is skipped, not fatal: the warning is
		// already logged, and one bad directory must not hide the rest.
		entry, ok, _ := readCopilotSession(stateDir, dirEntry.Name(), withEvents)
		if !ok {
			continue
		}
		entries = append(entries, entry)
	}
	// The cache is synchronized over the UNFILTERED listing and filtered
	// afterwards. Syncing the cwd-scoped subset instead would make a
	// project-scoped `conv ls` evict every conversation belonging to another
	// project — the cache would then flip its contents depending on which
	// directory the last command ran in.
	if withEvents {
		// Only the full read may write the cache. The events-free read exists
		// precisely because it does not know FirstPrompt, MessageCount or
		// Model, and upserting from it would blank those columns for every
		// Copilot conversation on any `conv resolve`.
		syncCopilotConvIndex(entries)
	} else {
		applyCopilotArchivedState(entries)
	}
	if cwdFilter != nil {
		filtered := entries[:0]
		for _, entry := range entries {
			if cwdFilter.matches(entry.ProjectPath) {
				filtered = append(filtered, entry)
			}
		}
		entries = filtered
	}
	// Newest first, with the id as a stable tiebreaker so a listing does not
	// reorder itself between calls when two sessions share a timestamp.
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Modified != entries[j].Modified {
			return entries[i].Modified > entries[j].Modified
		}
		return entries[i].SessionID < entries[j].SessionID
	})
	return entries, nil
}

// readCopilotSession builds one entry, or reports false when the session is
// not usable as a conversation. withEvents selects whether the event log is
// scanned for the three fields workspace.yaml does not carry.
//
// "Usable as a conversation" is the SINGLE definition of existence in this
// store — Exists calls this too, so the two can never disagree about a session
// whose workspace.yaml is present but unusable.
//
// The returned error separates "this is not a conversation" (nil: absent,
// unparsable, or cwd-less — all of which a listing skips) from "the store
// could not be read here" (a permission or IO failure). ListConvs treats both
// as skip; Exists must not, since a caller self-healing a stale mapping has to
// tell a vanished conversation from an unreadable disk.
func readCopilotSession(stateDir, id string, withEvents bool) (convops.SessionEntry, bool, error) {
	dir := filepath.Join(stateDir, id)
	workspacePath := filepath.Join(dir, copilotWorkspaceFileName)
	raw, err := os.ReadFile(workspacePath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("copilot convstore: workspace unreadable; session skipped",
				"conv", id, "path", workspacePath, "error", err)
			return convops.SessionEntry{}, false,
				fmt.Errorf("copilot: read %s: %w", workspacePath, err)
		}
		// A directory without workspace.yaml is not yet (or no longer) a
		// conversation — Copilot creates the directory before the file.
		return convops.SessionEntry{}, false, nil
	}
	var workspace copilotWorkspace
	if err := yaml.Unmarshal(raw, &workspace); err != nil {
		slog.Warn("copilot convstore: workspace unparsable; session skipped",
			"conv", id, "path", workspacePath, "error", err)
		return convops.SessionEntry{}, false, nil
	}
	// The directory name is the identity Copilot resumes by (`--resume=<id>`),
	// so it wins over a workspace.yaml `id` that disagrees; the file is only
	// consulted when the directory name is all tclaude has.
	if workspace.ID != "" && workspace.ID != id {
		slog.Warn("copilot convstore: workspace id disagrees with its directory",
			"conv", id, "workspace_id", workspace.ID)
	}
	if workspace.Cwd == "" {
		// Without a cwd the conversation cannot be resumed into a project, and
		// every cwd-scoped caller would mis-file it under "".
		slog.Warn("copilot convstore: workspace has no cwd; session skipped", "conv", id)
		return convops.SessionEntry{}, false, nil
	}

	eventsPath := filepath.Join(dir, copilotEventsFileName)
	var events copilotEventSummary
	if withEvents {
		events = readCopilotEvents(id, eventsPath)
	}

	entry := convops.SessionEntry{
		SessionID:    id,
		FullPath:     eventsPath,
		FileMtime:    events.mtime,
		FileSize:     events.size,
		FirstPrompt:  events.firstPrompt,
		MessageCount: events.userMessages,
		Created:      copilotTimestamp(workspace.CreatedAt),
		Modified:     copilotTimestamp(workspace.UpdatedAt),
		GitBranch:    workspace.Branch,
		ProjectPath:  filepath.Clean(workspace.Cwd),
		Harness:      CopilotName,
		Model:        events.model,
	}
	// `user_named` is Copilot's own flag for "a human chose this", which is
	// precisely tclaude's CustomTitle/Summary split. A generated name is a
	// summary; an operator's `--name` or `/rename` is an override.
	if workspace.UserNamed {
		entry.CustomTitle = workspace.Name
	} else {
		entry.Summary = workspace.Name
	}
	return entry, true, nil
}

// syncCopilotConvIndex mirrors the listing into tclaude's conv_index cache and
// overlays the one tclaude-owned column back onto it.
//
// Copilot's own files stay the source of truth for everything here — this is a
// cache, refreshed from them, never read back as authority. What it buys is
// the ability for a tclaude-side verb to name a Copilot conversation at all.
//
// `tclaude conv archive <copilot-id>` is the concrete case. Archiving is a
// tclaude concept with nowhere to live but `conv_index.archived_at`, and the
// command resolves its argument through that table. Before this sync a Copilot
// conversation had a row only if some OTHER operation had happened to create
// one — a rename, a spawn — so archiving a conversation tclaude could plainly
// list would fail with "no conversation matches". Writing the row as part of
// the listing removes that ordering dependency: anything `conv ls` shows is
// something `conv archive` can name. (`conv archive` also self-heals for a
// conversation that has never been listed; see ensureIndexedConv in pkg/claude/conv.)
//
// The sync is UPSERT-ONLY. OpenCode additionally evicts rows its snapshot did
// not mention, and doing the same here would be actively dangerous: Copilot's
// listing is scoped to whatever COPILOT_HOME currently resolves to, so an
// operator who repoints it — or a fixture run that does — would permanently
// destroy the archived flags of every conversation under the real home.
// Eviction also buys nothing, because Copilot's Resolve reads the session-state
// tree rather than this cache, so a stale row cannot resurrect a deleted
// conversation in any listing.
//
// Every cache failure degrades to "listing without the cache" rather than
// failing the listing. Copilot's conversations exist whether or not tclaude
// can write its own SQLite.
func syncCopilotConvIndex(entries []convops.SessionEntry) {
	if len(entries) == 0 {
		return
	}
	cached := map[string]*db.ConvIndexRow{}
	rows, err := db.ListAllConvIndex()
	if err != nil {
		// Without the current rows there is no archived state to overlay, but
		// refreshing what Copilot just told us is still correct, so this falls
		// through with an empty cache view rather than giving up.
		slog.Warn("copilot convstore: conv_index unreadable; archived state unavailable",
			"error", err)
	} else {
		for _, row := range rows {
			if row.Harness == CopilotName {
				cached[row.ConvID] = row
			}
		}
	}
	convIDs := make([]string, 0, len(entries))
	for i := range entries {
		convIDs = append(convIDs, entries[i].SessionID)
	}
	liveWorkspaces, liveErr := db.ListAgentWorkspacesByConv(convIDs)
	if liveErr != nil {
		// Keep the cache sync useful for titles/prompts even if the optional
		// live overlay cannot be read. Existing conv_index branch fields are
		// retained below, which is safer than replacing a hook observation
		// with a potentially older workspace.yaml value.
		slog.Warn("copilot convstore: live workspaces unreadable; preserving cached branches",
			"error", liveErr)
	}

	for i := range entries {
		cachedRow := cached[entries[i].SessionID]
		if cachedRow != nil && !cachedRow.ArchivedAt.IsZero() {
			entries[i].ArchivedAt = cachedRow.ArchivedAt.UTC().Format(time.RFC3339)
		}
		row := copilotEntryDBRow(entries[i])
		preserveCopilotLiveBranches(row, cachedRow, liveWorkspaces[entries[i].SessionID], liveErr)
		if err := db.UpsertConvIndex(row); err != nil {
			slog.Warn("copilot convstore: conv_index upsert failed; continuing from Copilot",
				"conv", entries[i].SessionID, "error", err)
		}
	}
}

// preserveCopilotLiveBranches merges the cold workspace.yaml observation with
// the hook-owned live workspace snapshot before the full conv_index upsert.
// The full upsert is necessary for Copilot's title/prompt metadata, but without
// this overlay it would blank git_branch_startup and could replace a newer
// hook-time branch with an older workspace.yaml value on every `conv ls`.
func preserveCopilotLiveBranches(row, cached *db.ConvIndexRow, live db.AgentWorkspace, liveErr error) {
	if row == nil {
		return
	}
	if cached != nil && cached.GitBranchStartup != "" {
		row.GitBranchStartup = cached.GitBranchStartup
	} else {
		row.GitBranchStartup = row.GitBranch
	}
	if liveErr != nil {
		if cached != nil && cached.GitBranch != "" {
			row.GitBranch = cached.GitBranch
		}
		return
	}
	// workspace.yaml.updated_at covers every metadata write (rename, summary,
	// checkpoint, ...), not specifically a branch observation, so it cannot be
	// compared meaningfully with the hook timestamp. Once a live workspace row
	// exists, that hook-owned observation is the dashboard branch authority.
	if live.ConvID != "" && live.Branch != "" {
		row.GitBranch = live.Branch
	}
}

// copilotEntryDBRow projects a listing entry onto the cache row.
//
// ArchivedAt is deliberately absent: it is the one column tclaude owns rather
// than derives, `SetConvIndexArchived` is what writes it, and re-sending it
// through a full upsert on every listing is how a cache overwrites the only
// value it is not the source of.
func copilotEntryDBRow(entry convops.SessionEntry) *db.ConvIndexRow {
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
		GitBranch:    entry.GitBranch,
		// The cold store has only one branch value. Treat it as startup on the
		// first index; syncCopilotConvIndex preserves an established startup
		// value from a hook-owned snapshot on subsequent refreshes.
		GitBranchStartup: entry.GitBranch,
		IndexedAt:        time.Now(),
		Harness:          CopilotName,
	}
}

// applyCopilotArchivedState overlays tclaude's own archived flag onto the
// listing.
//
// Archiving is a TCLAUDE concept: Copilot has no equivalent, so `tclaude conv
// archive` records it in `conv_index.archived_at` and nowhere else. Without
// this overlay `IsArchived()` is false for every Copilot conversation and an
// archived one would never actually hide — the same reason the OpenCode store
// reads the column back. Copilot's own files stay the source of truth for
// everything else; only this one tclaude-owned field comes from the cache.
//
// An unreadable cache degrades to "nothing is archived" rather than failing
// the listing, which is the safer direction: showing an archived conversation
// beats hiding every conversation.
func applyCopilotArchivedState(entries []convops.SessionEntry) {
	if len(entries) == 0 {
		return
	}
	rows, err := db.ListAllConvIndex()
	if err != nil {
		slog.Warn("copilot convstore: conv_index unreadable; archived state unavailable",
			"error", err)
		return
	}
	archived := make(map[string]time.Time, len(rows))
	for _, row := range rows {
		if row.Harness == CopilotName && !row.ArchivedAt.IsZero() {
			archived[row.ConvID] = row.ArchivedAt
		}
	}
	if len(archived) == 0 {
		return
	}
	for i := range entries {
		if at, ok := archived[entries[i].SessionID]; ok {
			entries[i].ArchivedAt = at.UTC().Format(time.RFC3339)
		}
	}
}

// copilotTimestamp normalizes workspace.yaml's ISO-8601 stamps to the RFC3339
// UTC form the rest of tclaude compares and renders. An unparsable value is
// passed through rather than dropped: a caller showing a slightly odd string
// beats one showing nothing.
func copilotTimestamp(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	return parsed.UTC().Format(time.RFC3339)
}

// copilotEventSummary is what one events.jsonl scan yields.
type copilotEventSummary struct {
	firstPrompt  string
	userMessages int
	model        string
	mtime        time.Time
	size         int64
}

// copilotEvent is the minimal decode of one event line.
type copilotEvent struct {
	Type string `json:"type"`
	Data struct {
		// user.message
		Content string `json:"content"`
		// session.start / session.resume
		SelectedModel string `json:"selectedModel"`
		// session.model_change
		NewModel string `json:"newModel"`
	} `json:"data"`
}

// copilotEventLineLimit bounds how much of one event line is buffered.
//
// Copilot writes its ~26 kB system prompt as a single `system.message` line
// and a tool result can be far larger, so no small bound is safe. A line past
// this limit is DISCARDED and the scan continues with the next one — a
// `bufio.Scanner` would instead stop for good on `ErrTooLong`, which in an
// append-only log means one huge tool result permanently freezes a
// conversation's turn count and model at whatever preceded it. Losing one line
// is recoverable; losing every line after it is not.
const copilotEventLineLimit = 8 << 20

// copilotEventPrefilters are the only line types this scan cares about.
//
// The check is a substring test on the RAW line, and it exists because the
// lines this scan must NOT pay for are the expensive ones: a session's
// `system.message` events carry the full system prompt, and decoding one per
// turn to discover it is not a user message would dominate a listing. A false
// positive — the string appearing inside prompt text — costs one decode that
// the type switch then discards, so it is free of consequence.
//
// The one way a relevant line could lack its needle is a writer that escapes
// plain ASCII (`"user.message"` is legal JSON for the same string).
// Copilot's log is machine-written and does not, so the prefilter is exact in
// practice; if that ever changed the symptom would be an undercounted turn,
// not a corrupt one.
var copilotEventPrefilters = [][]byte{
	[]byte("user.message"),
	[]byte("session.start"),
	[]byte("session.resume"),
	[]byte("session.model_change"),
}

// readCopilotEvents scans the append-only event log for the fields
// workspace.yaml does not carry.
//
// Every failure degrades to a partial summary rather than an error. A session
// whose log is missing, truncated mid-line by a live writer, or corrupt still
// lists — with its workspace.yaml identity, cwd, title and timestamps intact.
func readCopilotEvents(convID, path string) copilotEventSummary {
	var summary copilotEventSummary
	file, err := os.Open(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("copilot convstore: event log unreadable; listing without it",
				"conv", convID, "path", path, "error", err)
		}
		return summary
	}
	defer func() { _ = file.Close() }()

	if info, err := file.Stat(); err == nil {
		summary.mtime = info.ModTime().UTC()
		summary.size = info.Size()
	}

	reader := bufio.NewReaderSize(file, 64<<10)
	for {
		line, err := readCopilotEventLine(reader)
		if len(line) == 0 && err != nil {
			if !errors.Is(err, io.EOF) {
				slog.Warn("copilot convstore: event log scan stopped early; listing what was read",
					"conv", convID, "path", path, "error", err)
			}
			break
		}
		if !copilotEventLineOfInterest(line) {
			if err != nil {
				break
			}
			continue
		}
		var event copilotEvent
		if decodeErr := json.Unmarshal(line, &event); decodeErr != nil {
			// A partially flushed final line is normal while a session runs.
			if err != nil {
				break
			}
			continue
		}
		switch event.Type {
		case "user.message":
			summary.userMessages++
			if summary.firstPrompt == "" {
				summary.firstPrompt = strings.TrimSpace(event.Data.Content)
			}
		case "session.start", "session.resume":
			// Last-wins: a resume restates the model in force from then on.
			if event.Data.SelectedModel != "" {
				summary.model = event.Data.SelectedModel
			}
		case "session.model_change":
			if event.Data.NewModel != "" {
				summary.model = event.Data.NewModel
			}
		}
		if err != nil {
			break
		}
	}
	return summary
}

// readCopilotEventLine returns the next line without its terminator.
//
// It returns a non-nil error together with whatever bytes it had: io.EOF for a
// final line with no newline (a live writer's partial flush), or a read error.
// A line longer than copilotEventLineLimit is consumed to its end and reported
// as EMPTY, so the caller drops that one line and keeps scanning — see the
// limit's own comment for why aborting the whole scan is the wrong failure.
func readCopilotEventLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	overLimit := false
	for {
		chunk, err := reader.ReadSlice('\n')
		if !overLimit {
			if len(line)+len(chunk) > copilotEventLineLimit {
				overLimit = true
				line = nil
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			// The line is longer than the reader's buffer; keep consuming.
			continue
		}
		if err != nil {
			return bytes.TrimSuffix(line, []byte("\r")), err
		}
		if overLimit {
			// Drop the oversized line and start the next one.
			line, overLimit = nil, false
			continue
		}
		line = bytes.TrimSuffix(line, []byte("\n"))
		return bytes.TrimSuffix(line, []byte("\r")), nil
	}
}

func copilotEventLineOfInterest(line []byte) bool {
	for _, needle := range copilotEventPrefilters {
		if bytes.Contains(line, needle) {
			return true
		}
	}
	return false
}

// Resolve maps a full id or an id prefix onto a conversation. Copilot's
// session-state tree is indexed by id alone, not by cwd, so `global` only
// widens which conversations are CONSIDERED — an exact id resolves the same
// conversation from anywhere.
func (s copilotConvStore) Resolve(idPrefix, cwd string, global bool) (*ConvRef, error) {
	if idPrefix == "" {
		return nil, nil
	}
	if global {
		cwd = ""
	}
	// A ConvRef is an id, a cwd and a harness — all of them workspace.yaml
	// fields — so resolution never needs the event logs.
	entries, err := s.listConvs(cwd, false)
	if err != nil {
		return nil, fmt.Errorf("resolve Copilot conversation %q: %w", idPrefix, err)
	}
	for i := range entries {
		if entries[i].SessionID == idPrefix {
			return copilotConvRef(entries[i]), nil
		}
	}
	var match *convops.SessionEntry
	for i := range entries {
		if !strings.HasPrefix(entries[i].SessionID, idPrefix) {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf(
				"ambiguous conversation id %q: matches multiple Copilot conversations",
				idPrefix)
		}
		match = &entries[i]
	}
	if match == nil {
		return nil, nil
	}
	return copilotConvRef(*match), nil
}

// copilotSafeConvID reports whether an id may be joined onto the session-state
// path as a single directory segment.
func copilotSafeConvID(convID string) bool {
	return convID != "." && convID != ".." &&
		!strings.ContainsAny(convID, `/\`) &&
		!strings.ContainsRune(convID, 0)
}

// CopilotSafeConvID is copilotSafeConvID for callers outside this package.
//
// agentd's usage sweep uses it on a conv id it is about to send as a SQL
// PARAMETER rather than join onto a path, where the id cannot escape a
// directory in any case. The check still earns its place there: an id that
// could not name a session-state directory cannot be a real row's session_id
// either, so querying for it only widens the sweep's predicate with a term
// that can never match.
func CopilotSafeConvID(convID string) bool { return copilotSafeConvID(convID) }

func copilotConvRef(entry convops.SessionEntry) *ConvRef {
	return &ConvRef{
		ConvID:      entry.SessionID,
		ProjectPath: entry.ProjectPath,
		Harness:     CopilotName,
	}
}

// Title returns workspace.yaml's `name` through the standard DisplayTitle
// precedence, falling back to the first prompt for a session Copilot has not
// summarized yet. An unknown conversation is ("", nil), not an error.
func (s copilotConvStore) Title(convID string) (string, error) {
	if convID == "" || !copilotSafeConvID(convID) {
		return "", nil
	}
	stateDir, err := s.sessionStateDir()
	if err != nil {
		return "", err
	}
	// One session, and its event log only when workspace.yaml has no name to
	// give — the FirstPrompt fallback is the sole reason a title read would
	// ever need the log.
	entry, ok, err := readCopilotSession(stateDir, convID, false)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	if title := entry.DisplayTitle(); title != "" {
		return title, nil
	}
	entry, ok, err = readCopilotSession(stateDir, convID, true)
	if err != nil || !ok {
		return "", err
	}
	return entry.DisplayTitle(), nil
}

// SetTitle is unsupported by design. Copilot renames through its `/rename`
// slash command, so agentd's rename dispatch takes the in-pane injection path
// (Lifecycle.RenameCommand) and never reaches here. workspace.yaml is
// Copilot-managed state: writing `name` behind the CLI's back would be the
// same class of mistake as editing its SQLite.
func (copilotConvStore) SetTitle(convID, title string) error {
	return fmt.Errorf("copilot renames via the %q slash injection, not a direct title write", "/rename")
}

// Exists reports whether convID's session-state directory is still present.
// cwd is ignored: Copilot indexes session state by id alone, so a conversation
// is equally present from any working directory. (true, nil) present,
// (false, nil) confirmed absent, (false, err) the store could not be read — a
// caller self-healing a stale mapping must not act on a transient failure.
func (s copilotConvStore) Exists(convID, _ string) (bool, error) {
	if convID == "" || !copilotSafeConvID(convID) {
		// A conversation id is a path segment here. An id carrying a separator
		// or a `..` is not a Copilot session id at all, and joining it would
		// stat somewhere outside the session-state tree.
		return false, nil
	}
	stateDir, err := s.sessionStateDir()
	if err != nil {
		return false, err
	}
	// Deliberately the SAME readCopilotSession the listing uses, rather than a
	// cheaper stat: a workspace.yaml that is present but unparsable, or that
	// carries no cwd, is not a conversation ListConvs would return, and Exists
	// answering "present" for one the rest of the store cannot see is exactly
	// the inconsistency a self-healing caller would act on. The event scan is
	// skipped — existence never depends on it.
	_, ok, err := readCopilotSession(stateDir, convID, false)
	if err != nil {
		return false, err
	}
	return ok, nil
}
