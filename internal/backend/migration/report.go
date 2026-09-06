package migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/tofutools/tclaude/internal/backend/model"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

// ReadImportReport inspects a completed replacement database without running
// Store initialization or any other write-capable product path.
func ReadImportReport(ctx context.Context, databasePath string) (model.ImportReport, error) {
	db, err := openReadOnlyDatabase(ctx, databasePath)
	if err != nil {
		return model.ImportReport{}, err
	}
	defer func() { _ = db.Close() }()
	return backendsqlite.ReadImportReport(ctx, db)
}

func openReadOnlyDatabase(ctx context.Context, databasePath string) (*sql.DB, error) {
	if databasePath == "" {
		return nil, fmt.Errorf("replacement backend database path is required")
	}
	path, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("replacement backend database does not exist: %w", err)
		}
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("replacement backend database is not a regular file")
	}
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite", u.String()+"?mode=ro&immutable=1")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA query_only=ON`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("enable read-only import report: %w", err)
	}
	return db, nil
}
