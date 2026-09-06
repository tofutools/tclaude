package migration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	_ "modernc.org/sqlite"

	sourcev228 "github.com/tofutools/tclaude/internal/backend/migration/source/v228"
)

const maxManifestBytes = 4 << 20
const maxConfigBytes = 4 << 20

type Service struct{}

func (Service) Inspect(ctx context.Context, bundle Bundle) (Inspection, error) {
	return Inspect(ctx, bundle)
}
func (Service) Plan(inspection Inspection) (MigrationPlan, error) { return Plan(inspection) }

func Inspect(ctx context.Context, bundle Bundle) (Inspection, error) {
	root, manifestPath, err := explicitBundlePaths(bundle)
	if err != nil {
		return Inspection{}, err
	}
	manifestBytes, err := readBounded(manifestPath, maxManifestBytes)
	if err != nil {
		return Inspection{}, fmt.Errorf("read snapshot manifest: %w", err)
	}
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Inspection{}, fmt.Errorf("decode snapshot manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return Inspection{}, fmt.Errorf("decode snapshot manifest: %w", err)
	}

	inspection := Inspection{
		Source:   SourceSummary{ManifestHash: digest(manifestBytes)},
		Counts:   map[string]int64{},
		Snapshot: sourcev228.Snapshot{Rows: map[string][]sourcev228.Row{}, Malformed: map[string]int64{}},
	}
	if manifest.FormatVersion != BundleFormatVersion {
		inspection.add(SeverityBlocking, "unsupported_bundle_format", "", 0, "bundle format is not supported")
	}

	databasePath, err := verifyManifestFile(root, manifest.Database)
	if err != nil {
		return Inspection{}, fmt.Errorf("verify snapshot database: %w", err)
	}
	if manifest.Database.SHA256 != "" && !strings.EqualFold(manifest.Database.SHA256, fileDigest(databasePath)) {
		inspection.add(SeverityBlocking, "database_manifest_hash_mismatch", "", 0, "database bytes do not match the manifest")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(databasePath + suffix); err == nil {
			inspection.add(SeverityBlocking, "database_sidecar_present", "", 0, "snapshot is not a self-contained SQLite backup")
		} else if !errors.Is(err, os.ErrNotExist) {
			return Inspection{}, err
		}
	}
	beforeHash := fileDigest(databasePath)
	info, err := os.Stat(databasePath)
	if err != nil {
		return Inspection{}, err
	}
	inspection.Source.DatabaseHash, inspection.Source.DatabaseSize = beforeHash, info.Size()
	if manifest.Database.Size >= 0 && manifest.Database.Size != info.Size() {
		inspection.add(SeverityBlocking, "database_manifest_size_mismatch", "", 0, "database size does not match the manifest")
	}

	db, err := openImmutable(databasePath)
	if err != nil {
		return Inspection{}, err
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `PRAGMA query_only=ON`); err != nil {
		return Inspection{}, err
	}
	validateIntegrity(ctx, db, &inspection)
	version, versionRows, err := sourcev228.SchemaVersionValue(ctx, db)
	if err != nil {
		return Inspection{}, err
	}
	inspection.Source.SchemaVersion = version
	if versionRows != 1 {
		inspection.add(SeverityBlocking, "invalid_schema_version_cardinality", "schema_version", versionRows, "source must contain exactly one schema version row")
	} else if version != SourceSchemaVersion {
		inspection.add(SeverityBlocking, "unsupported_source_schema", "schema_version", 1, "source schema version is not supported")
	}
	missing, err := sourcev228.ValidateSignatures(ctx, db)
	if err != nil {
		return Inspection{}, err
	}
	if len(missing) > 0 {
		inspection.add(SeverityBlocking, "source_signature_mismatch", "", int64(len(missing)), "preservation-critical tables or columns are missing")
	}
	counts, err := sourcev228.TableCounts(ctx, db)
	if err != nil {
		return Inspection{}, err
	}
	inspection.Counts = counts
	if len(missing) == 0 {
		snapshot, err := sourcev228.ReadSnapshot(ctx, db)
		if err != nil {
			return Inspection{}, err
		}
		inspection.Snapshot = snapshot
		sourcev228.ValidateJSON(&inspection.Snapshot)
		for field, count := range inspection.Snapshot.Malformed {
			table := strings.SplitN(field, ".", 2)[0]
			inspection.add(SeverityBlocking, "malformed_or_oversize_json", table, count, "authored or checkpoint JSON cannot be safely converted")
		}
		validateReferences(&inspection)
	}

	if manifest.Config != nil {
		configPath, err := verifyManifestFile(root, *manifest.Config)
		if err != nil {
			return Inspection{}, fmt.Errorf("verify snapshot config: %w", err)
		}
		configBytes, err := readBounded(configPath, maxConfigBytes)
		if err != nil {
			return Inspection{}, fmt.Errorf("read snapshot config: %w", err)
		}
		if !json.Valid(configBytes) {
			inspection.add(SeverityBlocking, "malformed_config_json", "", 1, "optional authored config is not valid JSON")
		} else {
			inspection.Snapshot.Config = append(json.RawMessage(nil), configBytes...)
		}
	}
	validateAttachments(root, manifest.Attachments, &inspection)

	afterHash := fileDigest(databasePath)
	if afterHash != beforeHash {
		inspection.add(SeverityBlocking, "source_changed_during_inspection", "", 0, "source database bytes changed during inspection")
	}
	inspection.Valid = !hasBlocking(inspection.Diagnostics)
	sortDiagnostics(inspection.Diagnostics)
	return inspection, nil
}

func explicitBundlePaths(bundle Bundle) (string, string, error) {
	if strings.TrimSpace(bundle.Root) == "" || strings.TrimSpace(bundle.ManifestPath) == "" {
		return "", "", errors.New("snapshot bundle root and manifest path are required")
	}
	root, err := filepath.Abs(bundle.Root)
	if err != nil {
		return "", "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", fmt.Errorf("resolve snapshot root: %w", err)
	}
	manifest := bundle.ManifestPath
	if !filepath.IsAbs(manifest) {
		manifest = filepath.Join(root, manifest)
	}
	manifest, err = containedRegularFile(root, manifest)
	return root, manifest, err
}

func verifyManifestFile(root string, file ManifestFile) (string, error) {
	if file.Path == "" || filepath.IsAbs(file.Path) {
		return "", errors.New("manifest file path must be relative")
	}
	path, err := containedRegularFile(root, filepath.Join(root, filepath.FromSlash(file.Path)))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if file.Size >= 0 && info.Size() != file.Size {
		return "", errors.New("manifest file size mismatch")
	}
	if !validDigest(file.SHA256) {
		return "", errors.New("manifest file sha256 must be 64 lowercase hexadecimal characters")
	}
	if fileDigest(path) != file.SHA256 {
		return "", errors.New("manifest file hash mismatch")
	}
	return path, nil
}

func containedRegularFile(root, path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("snapshot file escapes bundle root")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("snapshot entry is not a regular file")
	}
	return resolved, nil
}

func openImmutable(path string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite", u.String()+"?mode=ro&immutable=1")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open immutable snapshot: %w", err)
	}
	return db, nil
}

func validateIntegrity(ctx context.Context, db *sql.DB, inspection *Inspection) {
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		inspection.add(SeverityBlocking, "integrity_check_failed", "", 0, "SQLite integrity check could not run")
		return
	}
	defer rows.Close()
	var failures int64
	for rows.Next() {
		var result string
		if rows.Scan(&result) != nil || result != "ok" {
			failures++
		}
	}
	if failures > 0 {
		inspection.add(SeverityBlocking, "integrity_check_failed", "", failures, "SQLite integrity check reported failures")
	}
	refs, err := db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		inspection.add(SeverityBlocking, "foreign_key_check_failed", "", 0, "SQLite foreign-key check could not run")
		return
	}
	defer refs.Close()
	var orphans int64
	for refs.Next() {
		orphans++
	}
	if orphans > 0 {
		inspection.add(SeverityBlocking, "foreign_key_violation", "", orphans, "declared source references are orphaned")
	}
}

func validateReferences(inspection *Inspection) {
	agents, groups, conversations := keySet(inspection.Snapshot.Rows["agents"]), keySet(inspection.Snapshot.Rows["agent_groups"]), keySet(inspection.Snapshot.Rows["logical_conversations"])
	check := func(table, field string, targets map[string]bool, allowEmpty bool) {
		var missing int64
		for _, row := range inspection.Snapshot.Rows[table] {
			value := sourcev228.String(row.Values[field])
			if value == "" && allowEmpty {
				continue
			}
			if !targets[value] {
				missing++
			}
		}
		if missing > 0 {
			inspection.add(SeverityBlocking, "unresolved_identity_reference", table, missing, "identity references cannot be resolved without guessing")
		}
	}
	check("agent_conversations", "agent_id", agents, false)
	check("agent_group_members", "agent_id", agents, false)
	check("agent_group_members", "group_id", groups, false)
	check("agent_group_owners", "agent_id", agents, false)
	check("agent_group_owners", "group_id", groups, false)
	check("conversation_attempt_bindings", "conversation_id", conversations, false)
	check("conversation_reference_bindings", "conversation_id", conversations, false)
}

func validateAttachments(root string, attachments []Attachment, inspection *Inspection) {
	seen := map[string]bool{}
	for _, attachment := range attachments {
		key := attachment.SourceTable + "\x1f" + attachment.SourceID
		if seen[key] {
			inspection.add(SeverityBlocking, "duplicate_attachment_manifest_entry", attachment.SourceTable, 1, "attachment reference occurs more than once")
			continue
		}
		seen[key] = true
		if attachment.SourceTable != "agent_message_attachments" && attachment.SourceTable != "human_message_attachments" {
			inspection.add(SeverityBlocking, "unknown_attachment_source", "", 1, "attachment source table is not supported")
			continue
		}
		path, err := verifyManifestFile(root, ManifestFile{Path: attachment.Path, Size: attachment.Size, SHA256: attachment.SHA256})
		if err != nil {
			inspection.add(SeverityBlocking, "attachment_validation_failed", attachment.SourceTable, 1, "attachment bytes failed containment, size, or hash validation")
			continue
		}
		_ = path
		var matched bool
		for _, row := range inspection.Snapshot.Rows[attachment.SourceTable] {
			if sourcev228.String(row.Values["id"]) == attachment.SourceID {
				matched = true
				if size, ok := sourcev228.Int64(row.Values["size_bytes"]); !ok || size != attachment.Size {
					inspection.add(SeverityBlocking, "attachment_metadata_mismatch", attachment.SourceTable, 1, "attachment size differs from source metadata")
				}
				break
			}
		}
		if !matched {
			inspection.add(SeverityBlocking, "attachment_reference_missing", attachment.SourceTable, 1, "manifest attachment has no source row")
		}
	}
	for _, table := range []string{"agent_message_attachments", "human_message_attachments"} {
		var missing int64
		for _, row := range inspection.Snapshot.Rows[table] {
			if !seen[table+"\x1f"+sourcev228.String(row.Values["id"])] {
				missing++
			}
		}
		if missing > 0 {
			inspection.add(SeverityBlocking, "attachment_manifest_missing", table, missing, "source attachment rows have no bundled bytes")
		}
	}
}

func keySet(rows []sourcev228.Row) map[string]bool {
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		out[row.Key] = true
	}
	return out
}

func (i *Inspection) add(severity Severity, code, table string, count int64, detail string) {
	i.Diagnostics = append(i.Diagnostics, Diagnostic{Severity: severity, Code: code, Table: table, Count: count, Detail: detail})
}

func hasBlocking(diagnostics []Diagnostic) bool {
	for _, d := range diagnostics {
		if d.Severity == SeverityBlocking {
			return true
		}
	}
	return false
}

func sortDiagnostics(diagnostics []Diagnostic) {
	sort.Slice(diagnostics, func(i, j int) bool {
		a, b := diagnostics[i], diagnostics[j]
		if a.Severity != b.Severity {
			return a.Severity < b.Severity
		}
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		return a.Table < b.Table
	})
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("file exceeds inspection size limit")
	}
	return data, nil
}

func fileDigest(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func validDigest(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
