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

// SaveSavedPrompt creates when previousName is empty; otherwise it replaces an existing row.
func SaveSavedPrompt(previousName string, p SavedPrompt) error {
	d, err := Open()
	if err != nil {
		return err
	}
	var res sql.Result
	if previousName == "" {
		res, err = d.Exec(`INSERT INTO saved_prompts (name, body) VALUES (?, ?)`, p.Name, p.Body)
	} else {
		res, err = d.Exec(`UPDATE saved_prompts SET name = ?, body = ? WHERE name = ?`, p.Name, p.Body, previousName)
	}
	if isUniqueViolation(err) {
		return ErrPromptNameTaken
	}
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return sql.ErrNoRows
	}
	return err
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
