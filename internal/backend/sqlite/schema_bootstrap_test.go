package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSchemaBatchRollsBackOnFailureAndCanRetry(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "schema.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	ctx := context.Background()
	_, err = db.ExecContext(ctx, `CREATE TABLE retained(value TEXT); INSERT INTO retained VALUES('original')`)
	require.NoError(t, err)
	err = store.executeSchema(ctx,
		`CREATE TABLE added(value TEXT); UPDATE retained SET value='changed'`,
		`CREATE TABLE retained(value TEXT)`)
	require.Error(t, err)
	var value string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT value FROM retained`).Scan(&value))
	require.Equal(t, "original", value)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name='added'`).Scan(&count))
	require.Zero(t, count)
	require.NoError(t, store.executeSchema(ctx,
		`CREATE TABLE added(value TEXT); INSERT INTO added VALUES('committed')`,
		`UPDATE retained SET value='updated'`))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT value FROM added`).Scan(&value))
	require.Equal(t, "committed", value)
	require.NoError(t, db.QueryRowContext(ctx, `SELECT value FROM retained`).Scan(&value))
	require.Equal(t, "updated", value)
}

func BenchmarkOpenNewDatabase(b *testing.B) {
	for b.Loop() {
		b.StopTimer()
		path := filepath.Join(b.TempDir(), "backend.db")
		b.StartTimer()
		store, err := Open(path)
		if err != nil {
			b.Fatal(err)
		}
		if err := store.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
