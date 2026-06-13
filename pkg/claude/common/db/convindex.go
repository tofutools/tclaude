package db

import (
	"database/sql"
	"time"
)

// ConvIndexRow represents a row in the conv_index table.
type ConvIndexRow struct {
	ConvID       string
	ProjectDir   string // Claude project directory path (e.g., ~/.claude/projects/-Users-foo-git-bar)
	FullPath     string
	FileMtime    int64 // Unix seconds
	FileSize     int64
	FirstPrompt  string
	Summary      string
	CustomTitle  string
	MessageCount int
	Created      string // RFC3339
	Modified     string // RFC3339
	GitBranch    string // last-wins: the branch as of the most recent turn ("current")
	// GitBranchStartup is first-wins: the branch the conversation's
	// FIRST turn was stamped with — the branch Claude Code was launched
	// on. Immutable for the life of the conversation. Empty for convs
	// indexed before schema v32, until the next .jsonl rescan heals it.
	GitBranchStartup string
	ProjectPath      string // Working directory path
	IsSidechain      bool
	IndexedAt        time.Time
	ArchivedAt       time.Time // zero = active; non-zero = archived (soft-deleted)
	// Harness is the coding tool this conversation belongs to (e.g.
	// "claude", "codex"). Empty is treated as DefaultHarness ("claude")
	// on write; the scan path sets it so the column self-heals on every
	// rescan (schema v56).
	Harness string
}

// IsArchived reports whether this conv has been soft-deleted via
// reincarnation or a future manual `conv archive` verb. Listing
// surfaces (conv ls) hide archived rows by default; the title-suffix
// fallback (`-x`) covers convs that pre-date the column.
func (r *ConvIndexRow) IsArchived() bool {
	return !r.ArchivedAt.IsZero()
}

// UpsertConvIndex inserts or updates a conversation index entry.
func UpsertConvIndex(row *ConvIndexRow) error {
	db, err := Open()
	if err != nil {
		return err
	}

	sidechain := 0
	if row.IsSidechain {
		sidechain = 1
	}

	// An empty Harness defaults to "claude" so a caller that hasn't set
	// it (or a pre-v56 code path) writes the same value the column
	// DEFAULT would, rather than an empty string. The scan path fills it
	// explicitly, so the column self-heals on every rescan.
	harness := row.Harness
	if harness == "" {
		harness = DefaultHarness
	}

	_, err = db.Exec(`INSERT INTO conv_index
		(conv_id, project_dir, full_path, file_mtime, file_size,
		 first_prompt, summary, custom_title, message_count,
		 created, modified, git_branch, project_path, is_sidechain, indexed_at,
		 git_branch_startup, harness)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(conv_id) DO UPDATE SET
		 project_dir=excluded.project_dir, full_path=excluded.full_path,
		 file_mtime=excluded.file_mtime, file_size=excluded.file_size,
		 first_prompt=excluded.first_prompt, summary=excluded.summary,
		 custom_title=excluded.custom_title, message_count=excluded.message_count,
		 created=excluded.created, modified=excluded.modified,
		 git_branch=excluded.git_branch, project_path=excluded.project_path,
		 is_sidechain=excluded.is_sidechain, indexed_at=excluded.indexed_at,
		 git_branch_startup=excluded.git_branch_startup, harness=excluded.harness`,
		row.ConvID, row.ProjectDir, row.FullPath, row.FileMtime, row.FileSize,
		row.FirstPrompt, row.Summary, row.CustomTitle, row.MessageCount,
		row.Created, row.Modified, row.GitBranch, row.ProjectPath,
		sidechain, row.IndexedAt.Format(time.RFC3339Nano), row.GitBranchStartup, harness)
	return err
}

// ListConvIndex returns all conversation index entries for a project directory.
func ListConvIndex(projectDir string) ([]*ConvIndexRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(`SELECT conv_id, project_dir, full_path, file_mtime, file_size,
		first_prompt, summary, custom_title, message_count,
		created, modified, git_branch, project_path, is_sidechain, indexed_at,
		archived_at, git_branch_startup, harness
		FROM conv_index WHERE project_dir = ?`, projectDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanConvIndexRows(rows)
}

// ListAllConvIndex returns all conversation index entries across all projects.
func ListAllConvIndex() ([]*ConvIndexRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(`SELECT conv_id, project_dir, full_path, file_mtime, file_size,
		first_prompt, summary, custom_title, message_count,
		created, modified, git_branch, project_path, is_sidechain, indexed_at,
		archived_at, git_branch_startup, harness
		FROM conv_index`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanConvIndexRows(rows)
}

// ListRecentConvIndex returns the most-recently-modified conv_index
// rows, newest first, capped at limit (default 50 when limit <= 0).
// Sidechain and archived convs are excluded — they are never agent
// promotion candidates. The dashboard uses this to populate the
// "Conversations" list without dragging the entire conv history into
// the snapshot.
func ListRecentConvIndex(limit int) ([]*ConvIndexRow, error) {
	if limit <= 0 {
		limit = 50
	}
	db, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT conv_id, project_dir, full_path, file_mtime, file_size,
		first_prompt, summary, custom_title, message_count,
		created, modified, git_branch, project_path, is_sidechain, indexed_at,
		archived_at, git_branch_startup, harness
		FROM conv_index
		WHERE is_sidechain = 0 AND archived_at = ''
		ORDER BY file_mtime DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	return scanConvIndexRows(rows)
}

// GetConvIndex returns a single conversation index entry by ID.
func GetConvIndex(convID string) (*ConvIndexRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}

	row := db.QueryRow(`SELECT conv_id, project_dir, full_path, file_mtime, file_size,
		first_prompt, summary, custom_title, message_count,
		created, modified, git_branch, project_path, is_sidechain, indexed_at,
		archived_at, git_branch_startup, harness
		FROM conv_index WHERE conv_id = ?`, convID)

	return scanConvIndexRow(row)
}

// FindConvIndexByPrefix finds a conversation by ID prefix. Returns nil if 0 or 2+ matches.
func FindConvIndexByPrefix(prefix string) (*ConvIndexRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(`SELECT conv_id, project_dir, full_path, file_mtime, file_size,
		first_prompt, summary, custom_title, message_count,
		created, modified, git_branch, project_path, is_sidechain, indexed_at,
		archived_at, git_branch_startup, harness
		FROM conv_index WHERE conv_id LIKE ? || '%'`, prefix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	results, err := scanConvIndexRows(rows)
	if err != nil {
		return nil, err
	}
	if len(results) == 1 {
		return results[0], nil
	}
	return nil, nil
}

// DeleteConvIndex removes a conversation index entry.
func DeleteConvIndex(convID string) error {
	db, err := Open()
	if err != nil {
		return err
	}

	_, err = db.Exec(`DELETE FROM conv_index WHERE conv_id = ?`, convID)
	return err
}

// MaxConvIndexUpdatedAt returns the maximum indexed_at timestamp across all conv_index entries.
// Used by watch mode to detect changes made by other tclaude instances.
func MaxConvIndexUpdatedAt() (time.Time, error) {
	db, err := Open()
	if err != nil {
		return time.Time{}, err
	}

	var indexedAt string
	err = db.QueryRow(`SELECT COALESCE(MAX(indexed_at), '') FROM conv_index`).Scan(&indexedAt)
	if err != nil || indexedAt == "" {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, indexedAt)
}

// MaxConvIndexUpdatedAtForProject returns the maximum indexed_at for a specific project.
func MaxConvIndexUpdatedAtForProject(projectDir string) (time.Time, error) {
	db, err := Open()
	if err != nil {
		return time.Time{}, err
	}

	var indexedAt string
	err = db.QueryRow(`SELECT COALESCE(MAX(indexed_at), '') FROM conv_index WHERE project_dir = ?`, projectDir).Scan(&indexedAt)
	if err != nil || indexedAt == "" {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, indexedAt)
}

// DeleteConvIndexByProjectDir removes all entries for a project directory.
func DeleteConvIndexByProjectDir(projectDir string) error {
	db, err := Open()
	if err != nil {
		return err
	}

	_, err = db.Exec(`DELETE FROM conv_index WHERE project_dir = ?`, projectDir)
	return err
}

func scanConvIndexRows(rows *sql.Rows) ([]*ConvIndexRow, error) {
	var result []*ConvIndexRow
	for rows.Next() {
		r, err := scanOneConvIndex(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

func scanOneConvIndex(s interface{ Scan(...any) error }) (*ConvIndexRow, error) {
	var r ConvIndexRow
	var sidechain int
	var indexedAt, archivedAt string
	err := s.Scan(&r.ConvID, &r.ProjectDir, &r.FullPath, &r.FileMtime, &r.FileSize,
		&r.FirstPrompt, &r.Summary, &r.CustomTitle, &r.MessageCount,
		&r.Created, &r.Modified, &r.GitBranch, &r.ProjectPath,
		&sidechain, &indexedAt, &archivedAt, &r.GitBranchStartup, &r.Harness)
	if err != nil {
		return nil, err
	}
	r.IsSidechain = sidechain != 0
	r.IndexedAt, _ = time.Parse(time.RFC3339Nano, indexedAt)
	if archivedAt != "" {
		r.ArchivedAt, _ = time.Parse(time.RFC3339Nano, archivedAt)
	}
	return &r, nil
}

// SetConvIndexArchived stamps or clears the archived_at column on a
// single conv row. Doesn't touch any other column — UpsertConvIndex
// (the routine .jsonl-scan path) deliberately omits archived_at from
// its ON CONFLICT update so the archived flag survives every
// rescan. Used by the reincarnate orchestrator and the (future)
// manual `conv archive` verb.
//
// Returns sql.ErrNoRows if no row matches convID.
func SetConvIndexArchived(convID string, archived bool) error {
	d, err := Open()
	if err != nil {
		return err
	}
	val := ""
	if archived {
		val = time.Now().UTC().Format(time.RFC3339Nano)
	}
	res, err := d.Exec(`UPDATE conv_index SET archived_at = ? WHERE conv_id = ?`,
		val, convID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetConvIndexProjectPath persists a conversation's working directory
// onto its conv_index row. It backfills a conversation that was named
// before its first turn: Claude Code stamps cwd onto turns, so such a
// conversation has none of its own, and the value is derived from a
// sibling in the same Claude project directory (see
// convops.backfillProjectPaths).
//
// The WHERE clause only fills a row whose project_path is still empty,
// so a real recorded cwd is never overwritten. A conv_id with no row,
// or one that already has a cwd, is a no-op and not an error — this is
// a best-effort setter.
func SetConvIndexProjectPath(convID, projectPath string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(
		`UPDATE conv_index SET project_path = ? WHERE conv_id = ? AND (project_path = '' OR project_path IS NULL)`,
		projectPath, convID)
	return err
}

func scanConvIndexRow(row *sql.Row) (*ConvIndexRow, error) {
	r, err := scanOneConvIndex(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}
