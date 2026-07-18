package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	ProcessSnippetIDPrefix          = "psn_"
	MaxProcessSnippetCount          = 128
	MaxProcessSnippetAggregateBytes = 4 << 20
	MaxProcessSnippetEnvelopeBytes  = 256 << 10
	MaxProcessSnippetNameRunes      = 80
	MaxProcessSnippetNameBytes      = 160
)

var (
	ErrProcessSnippetNotFound     = errors.New("process snippet not found")
	ErrProcessSnippetConflict     = errors.New("process snippet revision conflict")
	ErrProcessSnippetNameExists   = errors.New("process snippet name already exists")
	ErrProcessSnippetCountLimit   = errors.New("process snippet count limit reached")
	ErrProcessSnippetByteLimit    = errors.New("process snippet storage limit reached")
	ErrProcessSnippetStoreCorrupt = errors.New("process snippet store is inconsistent")
)

type ProcessSnippet struct {
	ID           string
	Name         string
	EnvelopeJSON string
	Revision     int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Corrupt      bool
}

type ProcessSnippetLibrary struct {
	Generation int64
	Snippets   []ProcessSnippet
}

func NewProcessSnippetID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("db: crypto/rand failed generating process snippet id: " + err.Error())
	}
	return ProcessSnippetIDPrefix + hex.EncodeToString(b[:])
}

func parseSnippetTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid process snippet timestamp: %w", err)
	}
	return parsed, nil
}

func scanProcessSnippet(scanner interface{ Scan(...any) error }) (ProcessSnippet, error) {
	var snippet ProcessSnippet
	var created, updated string
	if err := scanner.Scan(&snippet.ID, &snippet.Name, &snippet.EnvelopeJSON, &snippet.Revision, &created, &updated); err != nil {
		return ProcessSnippet{}, err
	}
	var err error
	if snippet.CreatedAt, err = parseSnippetTime(created); err != nil {
		snippet.Corrupt = true
	}
	if snippet.UpdatedAt, err = parseSnippetTime(updated); err != nil {
		snippet.Corrupt = true
	}
	if snippet.Revision <= 0 || !strings.HasPrefix(snippet.ID, ProcessSnippetIDPrefix) {
		snippet.Corrupt = true
	}
	return snippet, nil
}

func scanListedProcessSnippet(scanner interface{ Scan(...any) error }) (ProcessSnippet, error) {
	var snippet ProcessSnippet
	var created, updated string
	var storedBytes int64
	if err := scanner.Scan(&snippet.ID, &snippet.Name, &snippet.EnvelopeJSON, &snippet.Revision, &created, &updated, &storedBytes); err != nil {
		return ProcessSnippet{}, err
	}
	var err error
	if snippet.CreatedAt, err = parseSnippetTime(created); err != nil {
		snippet.Corrupt = true
	}
	if snippet.UpdatedAt, err = parseSnippetTime(updated); err != nil {
		snippet.Corrupt = true
	}
	if snippet.Revision <= 0 || !strings.HasPrefix(snippet.ID, ProcessSnippetIDPrefix) || storedBytes < 0 || storedBytes > MaxProcessSnippetEnvelopeBytes {
		snippet.Corrupt = true
		snippet.EnvelopeJSON = ""
	}
	return snippet, nil
}

func ListProcessSnippets() (ProcessSnippetLibrary, error) {
	d, err := Open()
	if err != nil {
		return ProcessSnippetLibrary{}, err
	}
	var library ProcessSnippetLibrary
	if err := d.QueryRow(`SELECT generation FROM process_snippet_library WHERE id = 1`).Scan(&library.Generation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ProcessSnippetLibrary{}, ErrProcessSnippetStoreCorrupt
		}
		return ProcessSnippetLibrary{}, err
	}
	// Never materialize a manually corrupted oversized payload, and never let
	// rows injected outside the bounded writer turn a list into an unbounded
	// response. Valid application state has at most MaxProcessSnippetCount rows.
	rows, err := d.Query(`SELECT id, name,
		CASE WHEN length(CAST(envelope_json AS BLOB)) <= ? THEN envelope_json ELSE '' END,
		revision, created_at, updated_at, length(CAST(envelope_json AS BLOB))
		FROM process_snippets ORDER BY name_key, id LIMIT ?`, MaxProcessSnippetEnvelopeBytes, MaxProcessSnippetCount)
	if err != nil {
		return ProcessSnippetLibrary{}, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		snippet, err := scanListedProcessSnippet(rows)
		if err != nil {
			return ProcessSnippetLibrary{}, err
		}
		library.Snippets = append(library.Snippets, snippet)
	}
	return library, rows.Err()
}

func lockProcessSnippetLibrary(tx *sql.Tx) (int64, error) {
	// This no-op write obtains SQLite's single-writer lock before quota reads,
	// so concurrent creates cannot both observe the same remaining capacity.
	result, err := tx.Exec(`UPDATE process_snippet_library SET generation = generation WHERE id = 1`)
	if err != nil {
		return 0, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if rows != 1 {
		return 0, ErrProcessSnippetStoreCorrupt
	}
	var generation int64
	if err := tx.QueryRow(`SELECT generation FROM process_snippet_library WHERE id = 1`).Scan(&generation); err != nil {
		return 0, err
	}
	return generation, nil
}

func processSnippetUsage(tx *sql.Tx) (count int64, bytes int64, err error) {
	// Cap each contribution before SUM so manually corrupted rows cannot make
	// quota accounting overflow. Any over-limit result fails closed below.
	err = tx.QueryRow(`SELECT COUNT(*), COALESCE(SUM(
		CASE WHEN length(CAST(envelope_json AS BLOB)) > ? THEN ?
		ELSE length(CAST(envelope_json AS BLOB)) END), 0)
		FROM process_snippets`, MaxProcessSnippetAggregateBytes+1, MaxProcessSnippetAggregateBytes+1).Scan(&count, &bytes)
	return
}

func bumpProcessSnippetGeneration(tx *sql.Tx) (int64, error) {
	if _, err := tx.Exec(`UPDATE process_snippet_library SET generation = generation + 1 WHERE id = 1`); err != nil {
		return 0, err
	}
	var generation int64
	if err := tx.QueryRow(`SELECT generation FROM process_snippet_library WHERE id = 1`).Scan(&generation); err != nil {
		return 0, err
	}
	return generation, nil
}

func CreateProcessSnippet(name, nameKey, envelopeJSON string) (ProcessSnippet, int64, error) {
	if len(envelopeJSON) > MaxProcessSnippetEnvelopeBytes {
		return ProcessSnippet{}, 0, ErrProcessSnippetByteLimit
	}
	d, err := Open()
	if err != nil {
		return ProcessSnippet{}, 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return ProcessSnippet{}, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := lockProcessSnippetLibrary(tx); err != nil {
		return ProcessSnippet{}, 0, err
	}
	count, used, err := processSnippetUsage(tx)
	if err != nil {
		return ProcessSnippet{}, 0, err
	}
	if count >= MaxProcessSnippetCount {
		return ProcessSnippet{}, 0, ErrProcessSnippetCountLimit
	}
	if used < 0 || used > MaxProcessSnippetAggregateBytes || int64(len(envelopeJSON)) > int64(MaxProcessSnippetAggregateBytes)-used {
		return ProcessSnippet{}, 0, ErrProcessSnippetByteLimit
	}
	now := time.Now().UTC()
	snippet := ProcessSnippet{
		ID: NewProcessSnippetID(), Name: name, EnvelopeJSON: envelopeJSON,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	_, err = tx.Exec(`INSERT INTO process_snippets
		(id, name, name_key, envelope_json, revision, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, snippet.ID, snippet.Name, nameKey,
		snippet.EnvelopeJSON, snippet.Revision, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return ProcessSnippet{}, 0, ErrProcessSnippetNameExists
		}
		return ProcessSnippet{}, 0, err
	}
	generation, err := bumpProcessSnippetGeneration(tx)
	if err != nil {
		return ProcessSnippet{}, 0, err
	}
	if err := tx.Commit(); err != nil {
		return ProcessSnippet{}, 0, err
	}
	return snippet, generation, nil
}

func RenameProcessSnippet(id, name, nameKey string, revision int64) (ProcessSnippet, int64, error) {
	d, err := Open()
	if err != nil {
		return ProcessSnippet{}, 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return ProcessSnippet{}, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := lockProcessSnippetLibrary(tx); err != nil {
		return ProcessSnippet{}, 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.Exec(`UPDATE process_snippets
		SET name = ?, name_key = ?, revision = revision + 1, updated_at = ?
		WHERE id = ? AND revision = ?`, name, nameKey, now, id, revision)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return ProcessSnippet{}, 0, ErrProcessSnippetNameExists
		}
		return ProcessSnippet{}, 0, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return ProcessSnippet{}, 0, err
	}
	if rows != 1 {
		var exists int
		if err := tx.QueryRow(`SELECT 1 FROM process_snippets WHERE id = ?`, id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return ProcessSnippet{}, 0, ErrProcessSnippetNotFound
		} else if err != nil {
			return ProcessSnippet{}, 0, err
		}
		return ProcessSnippet{}, 0, ErrProcessSnippetConflict
	}
	snippet, err := scanProcessSnippet(tx.QueryRow(`SELECT id, name, envelope_json, revision, created_at, updated_at
		FROM process_snippets WHERE id = ?`, id))
	if err != nil {
		return ProcessSnippet{}, 0, err
	}
	generation, err := bumpProcessSnippetGeneration(tx)
	if err != nil {
		return ProcessSnippet{}, 0, err
	}
	if err := tx.Commit(); err != nil {
		return ProcessSnippet{}, 0, err
	}
	return snippet, generation, nil
}

func DeleteProcessSnippet(id string, revision int64) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := lockProcessSnippetLibrary(tx); err != nil {
		return 0, err
	}
	result, err := tx.Exec(`DELETE FROM process_snippets WHERE id = ? AND revision = ?`, id, revision)
	if err != nil {
		return 0, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if rows != 1 {
		var exists int
		if err := tx.QueryRow(`SELECT 1 FROM process_snippets WHERE id = ?`, id).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return 0, ErrProcessSnippetNotFound
		} else if err != nil {
			return 0, err
		}
		return 0, ErrProcessSnippetConflict
	}
	generation, err := bumpProcessSnippetGeneration(tx)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return generation, nil
}
