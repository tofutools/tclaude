package db

import (
	"database/sql"
	"time"
)

// EmbeddingRow represents a row in the conv_embeddings table.
type EmbeddingRow struct {
	ConvID     string
	ChunkIndex int
	ChunkType  string // "metadata" or "content"
	ChunkText  string
	Embedding  []byte // raw float32 bytes (e.g. 1024 floats × 4 bytes = 4096 bytes for qwen3-embedding)
	Model      string
	CreatedAt  time.Time
}

// UpsertEmbedding inserts or updates an embedding row.
func UpsertEmbedding(row *EmbeddingRow) error {
	db, err := Open()
	if err != nil {
		return err
	}

	_, err = db.Exec(`INSERT INTO conv_embeddings
		(conv_id, chunk_index, chunk_type, chunk_text, embedding, model, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(conv_id, chunk_index) DO UPDATE SET
		 chunk_type=excluded.chunk_type, chunk_text=excluded.chunk_text,
		 embedding=excluded.embedding, model=excluded.model,
		 created_at=excluded.created_at`,
		row.ConvID, row.ChunkIndex, row.ChunkType, row.ChunkText,
		row.Embedding, row.Model, row.CreatedAt.Format(time.RFC3339Nano))
	return err
}

// ListAllEmbeddings returns all embedding rows across all conversations.
func ListAllEmbeddings() ([]*EmbeddingRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(`SELECT conv_id, chunk_index, chunk_type, chunk_text, embedding, model, created_at
		FROM conv_embeddings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanEmbeddingRows(rows)
}

// ListEmbeddingsForConv returns all embedding rows for a specific conversation.
func ListEmbeddingsForConv(convID string) ([]*EmbeddingRow, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(`SELECT conv_id, chunk_index, chunk_type, chunk_text, embedding, model, created_at
		FROM conv_embeddings WHERE conv_id = ? ORDER BY chunk_index`, convID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanEmbeddingRows(rows)
}

// DeleteEmbeddingsForConv removes all embeddings for a conversation.
func DeleteEmbeddingsForConv(convID string) error {
	db, err := Open()
	if err != nil {
		return err
	}

	_, err = db.Exec(`DELETE FROM conv_embeddings WHERE conv_id = ?`, convID)
	return err
}

// CountEmbeddings returns the total number of embedding rows.
func CountEmbeddings() (int, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}

	var count int
	err = db.QueryRow(`SELECT COUNT(*) FROM conv_embeddings`).Scan(&count)
	return count, err
}

// CountEmbeddedConversations returns the number of distinct conversations with embeddings.
func CountEmbeddedConversations() (int, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}

	var count int
	err = db.QueryRow(`SELECT COUNT(DISTINCT conv_id) FROM conv_embeddings`).Scan(&count)
	return count, err
}

// ListEmbeddedConvIDs returns all conversation IDs that have embeddings.
func ListEmbeddedConvIDs() (map[string]time.Time, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(`SELECT conv_id, MAX(created_at) FROM conv_embeddings GROUP BY conv_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]time.Time)
	for rows.Next() {
		var convID, createdAt string
		if err := rows.Scan(&convID, &createdAt); err != nil {
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339Nano, createdAt)
		result[convID] = t
	}
	return result, rows.Err()
}

// ListEmbeddedConvIDsForProject returns conversation IDs that have embeddings
// and belong to the given project directory (matched via conv_index).
func ListEmbeddedConvIDsForProject(projectDir string) (map[string]time.Time, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(`SELECT e.conv_id, MAX(e.created_at)
		FROM conv_embeddings e
		JOIN conv_index ci ON ci.conv_id = e.conv_id
		WHERE ci.project_dir = ?
		GROUP BY e.conv_id`, projectDir)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]time.Time)
	for rows.Next() {
		var convID, createdAt string
		if err := rows.Scan(&convID, &createdAt); err != nil {
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339Nano, createdAt)
		result[convID] = t
	}
	return result, rows.Err()
}

// ListEmbeddingModels returns the distinct model names used in stored embeddings.
func ListEmbeddingModels() ([]string, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(`SELECT DISTINCT model FROM conv_embeddings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var models []string
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, err
		}
		models = append(models, model)
	}
	return models, rows.Err()
}

// DeleteAllEmbeddings removes all embedding rows.
func DeleteAllEmbeddings() error {
	db, err := Open()
	if err != nil {
		return err
	}

	_, err = db.Exec(`DELETE FROM conv_embeddings`)
	return err
}

func scanEmbeddingRows(rows *sql.Rows) ([]*EmbeddingRow, error) {
	var result []*EmbeddingRow
	for rows.Next() {
		var r EmbeddingRow
		var createdAt string
		err := rows.Scan(&r.ConvID, &r.ChunkIndex, &r.ChunkType, &r.ChunkText,
			&r.Embedding, &r.Model, &createdAt)
		if err != nil {
			return nil, err
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt)
		result = append(result, &r)
	}
	return result, rows.Err()
}
