package db

import (
	"database/sql"
	"errors"
)

// A group page's marks and live patches (tcl-01arix), saved so a page that
// closes or outlives a daemon restart reopens with them. Each Send of marks
// is a row of group_page_marks, as JSON; the patch journal is one JSON array
// in group_page_patches, which also keeps both sequences, so a reader's
// --since still works after a reopen. Rows go with the page's group_pages
// row; a page without one (a private page) saves nothing.

// GroupPageCollab is what a saved page has kept: its mark batches oldest
// first, its patch journal, and the sequences they are numbered by.
type GroupPageCollab struct {
	MarkSeq  int64
	PatchSeq int64
	Batches  []string
	Patches  string
}

const groupPageExists = `EXISTS (SELECT 1 FROM group_pages WHERE id = ?)`

// AddGroupPageMarks saves one mark batch, numbered seq, and drops the oldest
// beyond keep. A page with no saved row saves nothing.
func AddGroupPageMarks(pageID string, seq int64, batch string, keep int) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`INSERT OR REPLACE INTO group_page_marks (page_id, seq, batch) SELECT ?, ?, ? WHERE `+groupPageExists,
		pageID, seq, batch, pageID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if _, err := tx.Exec(`DELETE FROM group_page_marks WHERE page_id = ? AND seq NOT IN (
			SELECT seq FROM group_page_marks WHERE page_id = ? ORDER BY seq DESC LIMIT ?)`, pageID, pageID, keep); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO group_page_patches (page_id, mark_seq) VALUES (?, ?)
		ON CONFLICT(page_id) DO UPDATE SET mark_seq = MAX(mark_seq, excluded.mark_seq)`, pageID, seq); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearGroupPageMarks drops a page's saved mark batches, keeping its mark
// sequence.
func ClearGroupPageMarks(pageID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM group_page_marks WHERE page_id = ?`, pageID)
	return err
}

// SaveGroupPagePatches saves a page's patch journal (a JSON array) as it is
// now, and the patch sequence. A page with no saved row saves nothing.
func SaveGroupPagePatches(pageID string, patchSeq int64, patches string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO group_page_patches (page_id, patch_seq, patches) SELECT ?, ?, ? WHERE `+groupPageExists+`
		ON CONFLICT(page_id) DO UPDATE SET patch_seq = excluded.patch_seq, patches = excluded.patches`,
		pageID, patchSeq, patches, pageID)
	return err
}

// LoadGroupPageCollab returns what a saved page has kept, nil when nothing.
func LoadGroupPageCollab(pageID string) (*GroupPageCollab, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var c GroupPageCollab
	err = d.QueryRow(`SELECT mark_seq, patch_seq, patches FROM group_page_patches WHERE page_id = ?`, pageID).
		Scan(&c.MarkSeq, &c.PatchSeq, &c.Patches)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT batch FROM group_page_marks WHERE page_id = ? ORDER BY seq`, pageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		c.Batches = append(c.Batches, b)
	}
	return &c, rows.Err()
}
