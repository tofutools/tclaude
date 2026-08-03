package harness

import (
	"bufio"
	"bytes"
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

	"gopkg.in/yaml.v3"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
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
// cwd is the documented "everything, everywhere" sentinel.
//
// A session whose own files are unreadable or malformed is SKIPPED with a
// warning rather than failing the listing: Copilot writes these files while
// tclaude reads them, and one half-written workspace.yaml must not hide every
// other conversation. Only a failure to enumerate the directory itself — the
// store as a whole being unreadable — is an error, since that is the case a
// caller genuinely cannot distinguish from "no conversations".
func (s copilotConvStore) ListConvs(cwd string) ([]convops.SessionEntry, error) {
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

	if cwd != "" {
		cwd = filepath.Clean(cwd)
	}
	entries := make([]convops.SessionEntry, 0, len(dirEntries))
	for _, dirEntry := range dirEntries {
		if !dirEntry.IsDir() {
			continue
		}
		entry, ok := s.readSession(stateDir, dirEntry.Name())
		if !ok {
			continue
		}
		if cwd != "" && filepath.Clean(entry.ProjectPath) != cwd {
			continue
		}
		entries = append(entries, entry)
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

// readSession builds one entry, or reports false when the session is not
// usable as a conversation.
func (s copilotConvStore) readSession(stateDir, id string) (convops.SessionEntry, bool) {
	dir := filepath.Join(stateDir, id)
	workspacePath := filepath.Join(dir, copilotWorkspaceFileName)
	raw, err := os.ReadFile(workspacePath)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("copilot convstore: workspace unreadable; session skipped",
				"conv", id, "path", workspacePath, "error", err)
		}
		// A directory without workspace.yaml is not yet (or no longer) a
		// conversation — Copilot creates the directory before the file.
		return convops.SessionEntry{}, false
	}
	var workspace copilotWorkspace
	if err := yaml.Unmarshal(raw, &workspace); err != nil {
		slog.Warn("copilot convstore: workspace unparsable; session skipped",
			"conv", id, "path", workspacePath, "error", err)
		return convops.SessionEntry{}, false
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
		return convops.SessionEntry{}, false
	}

	eventsPath := filepath.Join(dir, copilotEventsFileName)
	events := readCopilotEvents(id, eventsPath)

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
	return entry, true
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

// copilotEventScanBuffer bounds one event line. Copilot writes its ~26 kB
// system prompt as a single `system.message` line and tool results can be far
// larger, so bufio's 64 kB default would abort a scan on an ordinary session.
const copilotEventScanBuffer = 8 << 20

// copilotEventPrefilters are the only line types this scan cares about.
//
// The check is a substring test on the RAW line, and it exists because the
// lines this scan must NOT pay for are the expensive ones: a session's
// `system.message` events carry the full system prompt, and decoding one per
// turn to discover it is not a user message would dominate a listing. None of
// these needles contains a character JSON can escape, so a line whose `type`
// is one of them always contains the literal bytes. A false positive — the
// string appearing inside prompt text — costs one decode that then rejects it,
// so the prefilter can only make the scan faster, never wrong.
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

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64<<10), copilotEventScanBuffer)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !copilotEventLineOfInterest(line) {
			continue
		}
		var event copilotEvent
		if err := json.Unmarshal(line, &event); err != nil {
			// A partially flushed final line is normal while a session runs.
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
	}
	if err := scanner.Err(); err != nil {
		slog.Warn("copilot convstore: event log scan stopped early; listing what was read",
			"conv", convID, "path", path, "error", err)
	}
	return summary
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
	entries, err := s.ListConvs(cwd)
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
	// A conversation is identified by its directory; the workspace file is
	// what makes it readable. Checking the file keeps Exists consistent with
	// ListConvs, which skips a directory that has none.
	info, err := os.Stat(filepath.Join(stateDir, convID, copilotWorkspaceFileName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("copilot: stat conversation %s: %w", convID, err)
	}
	return info.Mode().IsRegular(), nil
}
