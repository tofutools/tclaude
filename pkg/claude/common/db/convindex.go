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
	GitBranch    string
	ProjectPath  string // Working directory path
	IsSidechain  bool
	IndexedAt    time.Time
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

	_, err = db.Exec(`INSERT INTO conv_index
		(conv_id, project_dir, full_path, file_mtime, file_size,
		 first_prompt, summary, custom_title, message_count,
		 created, modified, git_branch, project_path, is_sidechain, indexed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(conv_id) DO UPDATE SET
		 project_dir=excluded.project_dir, full_path=excluded.full_path,
		 file_mtime=excluded.file_mtime, file_size=excluded.file_size,
		 first_prompt=excluded.first_prompt, summary=excluded.summary,
		 custom_title=excluded.custom_title, message_count=excluded.message_count,
		 created=excluded.created, modified=excluded.modified,
		 git_branch=excluded.git_branch, project_path=excluded.project_path,
		 is_sidechain=excluded.is_sidechain, indexed_at=excluded.indexed_at`,
		row.ConvID, row.ProjectDir, row.FullPath, row.FileMtime, row.FileSize,
		row.FirstPrompt, row.Summary, row.CustomTitle, row.MessageCount,
		row.Created, row.Modified, row.GitBranch, row.ProjectPath,
		sidechain, row.IndexedAt.Format(time.RFC3339Nano))
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
		created, modified, git_branch, project_path, is_sidechain, indexed_at
		FROM conv_index WHERE project_dir = ?`, projectDir)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
		created, modified, git_branch, project_path, is_sidechain, indexed_at
		FROM conv_index`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
		created, modified, git_branch, project_path, is_sidechain, indexed_at
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
		created, modified, git_branch, project_path, is_sidechain, indexed_at
		FROM conv_index WHERE conv_id LIKE ? || '%'`, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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
	var indexedAt string
	err := s.Scan(&r.ConvID, &r.ProjectDir, &r.FullPath, &r.FileMtime, &r.FileSize,
		&r.FirstPrompt, &r.Summary, &r.CustomTitle, &r.MessageCount,
		&r.Created, &r.Modified, &r.GitBranch, &r.ProjectPath,
		&sidechain, &indexedAt)
	if err != nil {
		return nil, err
	}
	r.IsSidechain = sidechain != 0
	r.IndexedAt, _ = time.Parse(time.RFC3339Nano, indexedAt)
	return &r, nil
}

func scanConvIndexRow(row *sql.Row) (*ConvIndexRow, error) {
	r, err := scanOneConvIndex(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}
