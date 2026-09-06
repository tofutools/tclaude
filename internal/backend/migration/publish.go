package migration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

type ImportOptions struct {
	DestinationPath         string
	MetadataOnlyAttachments bool
}

type ImportResult struct {
	Receipt  model.ImportReceipt
	Repeated bool
}

// ImportSnapshot converts an explicit immutable bundle into an explicitly
// selected fresh database. The completed SQLite file is published with an
// exclusive hard link, so a racing destination is never overwritten.
func ImportSnapshot(ctx context.Context, bundle Bundle, options ImportOptions) (ImportResult, error) {
	if strings.TrimSpace(options.DestinationPath) == "" {
		return ImportResult{}, fmt.Errorf("fresh replacement destination path is required")
	}
	inspection, err := Inspect(ctx, bundle)
	if err != nil {
		return ImportResult{}, err
	}
	plan, err := Plan(inspection)
	if err != nil {
		return ImportResult{}, err
	}
	if !conversionAllowed(plan, options.MetadataOnlyAttachments) {
		return ImportResult{}, fmt.Errorf("offline import preflight has blocking diagnostics")
	}
	destination, err := filepath.Abs(options.DestinationPath)
	if err != nil {
		return ImportResult{}, err
	}
	if existing, exists, err := readPublishedReceipt(ctx, destination); err != nil {
		return ImportResult{}, err
	} else if exists {
		if receiptMatchesPlan(existing, inspection, plan, options.MetadataOnlyAttachments) {
			return ImportResult{Receipt: existing, Repeated: true}, nil
		}
		return ImportResult{}, fmt.Errorf("replacement destination already exists and is not the exact completed import")
	}
	payloads, err := readAttachmentPayloads(bundle, options.MetadataOnlyAttachments)
	if err != nil {
		return ImportResult{}, err
	}
	confirmed, err := Inspect(ctx, bundle)
	if err != nil {
		return ImportResult{}, err
	}
	confirmedPlan, err := Plan(confirmed)
	if err != nil {
		return ImportResult{}, err
	}
	if confirmed.Source != inspection.Source || confirmedPlan.PlanHash != plan.PlanHash {
		return ImportResult{}, fmt.Errorf("snapshot bundle changed after preflight")
	}
	batch, err := Translate(inspection, plan, payloads, TranslationOptions{MetadataOnlyAttachments: options.MetadataOnlyAttachments})
	if err != nil {
		return ImportResult{}, err
	}
	directory := filepath.Dir(destination)
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return ImportResult{}, fmt.Errorf("replacement destination directory must already exist")
	}
	temporary, err := os.CreateTemp(directory, ".tclaude-import-*.sqlite")
	if err != nil {
		return ImportResult{}, err
	}
	temporaryPath := temporary.Name()
	if err = temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return ImportResult{}, err
	}
	defer func() { _ = os.Remove(temporaryPath) }()
	store, err := backendsqlite.Open(temporaryPath)
	if err != nil {
		return ImportResult{}, err
	}
	if err = store.ApplyImport(ctx, batch); err == nil {
		err = store.VerifyImport(ctx, batch)
	}
	closeErr := store.Close()
	if err != nil {
		return ImportResult{}, err
	}
	if closeErr != nil {
		return ImportResult{}, closeErr
	}
	if err = syncFile(temporaryPath); err != nil {
		return ImportResult{}, err
	}
	if err = publishNoClobber(temporaryPath, destination); err != nil {
		if existing, exists, readErr := readPublishedReceipt(ctx, destination); readErr == nil && exists && receiptMatchesPlan(existing, inspection, plan, options.MetadataOnlyAttachments) {
			return ImportResult{Receipt: existing, Repeated: true}, nil
		}
		return ImportResult{}, err
	}
	return ImportResult{Receipt: batch.Receipt}, nil
}

func readAttachmentPayloads(bundle Bundle, metadataOnly bool) ([]AttachmentPayload, error) {
	root, manifestPath, err := explicitBundlePaths(bundle)
	if err != nil {
		return nil, err
	}
	manifestBytes, err := readBounded(manifestPath, maxManifestBytes)
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return nil, err
	}
	var out []AttachmentPayload
	for _, attachment := range manifest.Attachments {
		path, err := verifyManifestFile(root, ManifestFile{Path: attachment.Path, Size: attachment.Size, SHA256: attachment.SHA256})
		if err != nil {
			if metadataOnly && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		data, readErr := io.ReadAll(file)
		closeErr := file.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if digest(data) != attachment.SHA256 {
			return nil, fmt.Errorf("attachment changed after preflight")
		}
		out = append(out, AttachmentPayload{SourceTable: attachment.SourceTable, SourceID: attachment.SourceID, SHA256: attachment.SHA256, Data: data})
	}
	return out, nil
}

func publishNoClobber(temporary, destination string) error {
	if err := os.Link(temporary, destination); err != nil {
		if errors.Is(err, os.ErrExist) || errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("replacement destination appeared during publication: %w", err)
		}
		return fmt.Errorf("publish completed replacement database: %w", err)
	}
	if err := syncDirectory(filepath.Dir(destination)); err != nil {
		return fmt.Errorf("sync published replacement directory: %w", err)
	}
	if err := os.Remove(temporary); err != nil {
		return fmt.Errorf("remove completed import staging link: %w", err)
	}
	return syncDirectory(filepath.Dir(destination))
}

func syncFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	err = file.Sync()
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func readPublishedReceipt(ctx context.Context, path string) (model.ImportReceipt, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return model.ImportReceipt{}, false, nil
	} else if err != nil {
		return model.ImportReceipt{}, false, err
	}
	if !info.Mode().IsRegular() {
		return model.ImportReceipt{}, true, fmt.Errorf("existing replacement destination is not a regular file")
	}
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite", u.String()+"?mode=ro&immutable=1")
	if err != nil {
		return model.ImportReceipt{}, true, err
	}
	defer func() { _ = db.Close() }()
	var table int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='import_receipts'`).Scan(&table); err != nil || table != 1 {
		if err == nil {
			err = fmt.Errorf("existing destination is not a completed replacement import")
		}
		return model.ImportReceipt{}, true, err
	}
	var out model.ImportReceipt
	var counts []byte
	var completed int64
	if err := db.QueryRowContext(ctx, `SELECT id,source_schema_version,source_database_sha256,manifest_sha256,importer_format_version,plan_format_version,target_schema_version,plan_sha256,semantic_sha256,metadata_only_attachments,counts_json,completed_at FROM import_receipts LIMIT 1`).Scan(&out.ID, &out.SourceSchemaVersion, &out.SourceDatabaseSHA256, &out.ManifestSHA256, &out.ImporterFormatVersion, &out.PlanFormatVersion, &out.TargetSchemaVersion, &out.PlanSHA256, &out.SemanticSHA256, &out.MetadataOnlyAttachments, &counts, &completed); err != nil {
		return model.ImportReceipt{}, true, err
	}
	out.CompletedAt = timeFromNanos(completed)
	if err := json.Unmarshal(counts, &out.Counts); err != nil {
		return model.ImportReceipt{}, true, err
	}
	return out, true, nil
}

func receiptMatchesPlan(receipt model.ImportReceipt, inspection Inspection, plan MigrationPlan, metadataOnly bool) bool {
	return receipt.SourceSchemaVersion == inspection.Source.SchemaVersion &&
		receipt.SourceDatabaseSHA256 == inspection.Source.DatabaseHash &&
		receipt.ManifestSHA256 == inspection.Source.ManifestHash &&
		receipt.ImporterFormatVersion == ImporterFormatVersion &&
		receipt.PlanFormatVersion == plan.FormatVersion &&
		receipt.TargetSchemaVersion == TargetSchemaVersion &&
		receipt.PlanSHA256 == plan.PlanHash && receipt.MetadataOnlyAttachments == metadataOnly
}

func timeFromNanos(value int64) time.Time { return time.Unix(0, value).UTC() }
