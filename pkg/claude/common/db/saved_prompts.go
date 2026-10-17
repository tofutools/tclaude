package db

import (
	"database/sql"
	"errors"
)

// SavedPrompt is editable text inserted into an operator's draft, never sent automatically.
type SavedPrompt struct {
	Name string `json:"name"`
	Body string `json:"body"`
}

var ErrPromptNameTaken = errors.New("a saved prompt with that name already exists")

func ListSavedPrompts() ([]SavedPrompt, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT name, body FROM saved_prompts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []SavedPrompt{}
	for rows.Next() {
		var p SavedPrompt
		if err := rows.Scan(&p.Name, &p.Body); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func GetSavedPrompt(name string) (*SavedPrompt, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var p SavedPrompt
	err = d.QueryRow(`SELECT name, body FROM saved_prompts WHERE name = ?`, name).Scan(&p.Name, &p.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &p, err
}

// SaveSavedPrompt creates when previousName is empty; otherwise it replaces an
// existing row, carrying a rename into every profile and group reference the
// way a role rename does.
func SaveSavedPrompt(previousName string, p SavedPrompt) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var res sql.Result
	if previousName == "" {
		res, err = tx.Exec(`INSERT INTO saved_prompts (name, body) VALUES (?, ?)`, p.Name, p.Body)
	} else {
		res, err = tx.Exec(`UPDATE saved_prompts SET name = ?, body = ? WHERE name = ?`, p.Name, p.Body, previousName)
	}
	if isUniqueViolation(err) {
		return ErrPromptNameTaken
	}
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	if previousName != "" && previousName != p.Name {
		for _, col := range []struct{ table, column string }{
			{"spawn_profiles", "prompt_refs"},
			{"agent_groups", "default_prompts"},
		} {
			if _, err := tx.Exec(`UPDATE `+col.table+`
				SET `+col.column+` = (SELECT json_group_array(CASE value WHEN ? THEN ? ELSE value END) FROM json_each(`+col.column+`))
				WHERE EXISTS (SELECT 1 FROM json_each(`+col.column+`) WHERE value = ?)`, previousName, p.Name, previousName); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// SavedPromptReferences names the spawn profiles and groups that compose the
// named prompt into their spawns. Deletion is refused while any remain, for
// the same reason a referenced role cannot be deleted.
func SavedPromptReferences(name string) (profiles, groups []string, err error) {
	d, err := Open()
	if err != nil {
		return nil, nil, err
	}
	query := func(q string) ([]string, error) {
		rows, err := d.Query(q, name)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rows.Close() }()
		out := []string{}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return nil, err
			}
			out = append(out, n)
		}
		return out, rows.Err()
	}
	if profiles, err = query(`SELECT name FROM spawn_profiles
		WHERE EXISTS (SELECT 1 FROM json_each(prompt_refs) WHERE value = ?) ORDER BY name`); err != nil {
		return nil, nil, err
	}
	if groups, err = query(`SELECT name FROM agent_groups
		WHERE EXISTS (SELECT 1 FROM json_each(default_prompts) WHERE value = ?) ORDER BY name`); err != nil {
		return nil, nil, err
	}
	return profiles, groups, nil
}

func DeleteSavedPrompt(name string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`DELETE FROM saved_prompts WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return err
}
