package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CopilotModelCatalogEntry is one model returned by Copilot's authenticated
// model catalog. MaxPromptTokens is the effective input/context-meter cap;
// MaxContextWindowTokens also includes capacity reserved for model output.
type CopilotModelCatalogEntry struct {
	ModelID                string
	MaxContextWindowTokens int64
	MaxPromptTokens        int64
	MaxOutputTokens        int64
	RawJSON                string
}

// ReplaceCopilotModelCatalog atomically replaces the previous mirror. A failed
// or empty fetch must never call this function: retaining a stale last-known-good
// catalog gives readers their full 24-hour fallback window.
func ReplaceCopilotModelCatalog(entries []CopilotModelCatalogEntry, fetchedAt time.Time) error {
	if len(entries) == 0 {
		return errors.New("replace Copilot model catalog: empty catalog")
	}
	if fetchedAt.IsZero() {
		fetchedAt = time.Now().UTC()
	} else {
		fetchedAt = fetchedAt.UTC()
	}
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("replace Copilot model catalog: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM copilot_model_catalog`); err != nil {
		return fmt.Errorf("replace Copilot model catalog: clear: %w", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO copilot_model_catalog
		(model_id, max_context_window_tokens, max_prompt_tokens,
		 max_output_tokens, fetched_at, raw_json)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("replace Copilot model catalog: prepare: %w", err)
	}
	defer func() { _ = stmt.Close() }()
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		entry.ModelID = strings.TrimSpace(entry.ModelID)
		if entry.ModelID == "" {
			return errors.New("replace Copilot model catalog: empty model id")
		}
		if _, duplicate := seen[entry.ModelID]; duplicate {
			return fmt.Errorf("replace Copilot model catalog: duplicate model id %q", entry.ModelID)
		}
		seen[entry.ModelID] = struct{}{}
		if entry.MaxContextWindowTokens < 0 || entry.MaxPromptTokens < 0 || entry.MaxOutputTokens < 0 {
			return fmt.Errorf("replace Copilot model catalog: negative token limit for %q", entry.ModelID)
		}
		if _, err := stmt.Exec(entry.ModelID, entry.MaxContextWindowTokens,
			entry.MaxPromptTokens, entry.MaxOutputTokens, dbTime(fetchedAt), entry.RawJSON); err != nil {
			return fmt.Errorf("replace Copilot model catalog model %q: %w", entry.ModelID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("replace Copilot model catalog: commit: %w", err)
	}
	return nil
}

// FreshCopilotModelPromptLimit returns a model's mirrored prompt limit only
// while that exact row is younger than maxAge. Copilot treats its prompt limit
// as optional and falls back to max_context_window_tokens when it is absent;
// mirror that behavior before sending callers to their static fallback.
func FreshCopilotModelPromptLimit(modelID string, now time.Time, maxAge time.Duration) (int64, bool, error) {
	modelID = strings.TrimSpace(modelID)
	if modelID == "" || maxAge <= 0 {
		return 0, false, nil
	}
	d, err := Open()
	if err != nil {
		return 0, false, err
	}
	var limit, fetchedAt int64
	err = d.QueryRow(`SELECT
		CASE WHEN max_prompt_tokens > 0 THEN max_prompt_tokens
		     ELSE max_context_window_tokens END,
		fetched_at
		FROM copilot_model_catalog WHERE model_id = ?`, modelID).Scan(&limit, &fetchedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("load Copilot model catalog model %q: %w", modelID, err)
	}
	age := now.UTC().Sub(time.Unix(0, fetchedAt).UTC())
	if limit <= 0 || age >= maxAge {
		return 0, false, nil
	}
	return limit, true, nil
}
