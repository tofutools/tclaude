package groupexport

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

// container.go is the single, swappable boundary between the in-memory
// Export model and the bytes on disk. Phase 1 ships ONE container: a zip
// archive. If a second on-disk format is ever needed, this file is the
// only place that changes — model.go, DB collection, import, conv-id
// remap and path rewriting are all container-agnostic.
//
// Zip layout:
//
//	manifest.json              — the whole Export as one JSON document:
//	                             format metadata + every DB table's rows.
//	                             Conv.Content is json:"-", so the manifest
//	                             never inlines conversation bytes.
//	projects/<conv-id>.jsonl   — each agent's conversation as a real,
//	                             deflate-compressed file entry. Flat by
//	                             conv-id (a unique UUID); the manifest's
//	                             per-conv source_cwd records where it lived.
//
// No base64 anywhere: the .jsonl files are stored as-is, so the archive
// is compact (deflate) and the conversations stay directly inspectable.

const (
	// manifestName is the zip entry holding the structured Export.
	manifestName = "manifest.json"
	// convDirPrefix is the zip path prefix under which conversation
	// .jsonl files live.
	convDirPrefix = "projects/"

	// ContentType is the MIME type of a group-export artifact — used by
	// the daemon's HTTP export response.
	ContentType = "application/zip"
	// FileExtension is the conventional extension of an export artifact.
	FileExtension = ".zip"
)

// ErrUnsupportedFormat is returned by Unmarshal when the archive's
// manifest declares a FormatVersion this binary does not understand
// (typically a newer export opened by an older tclaude). The importer
// surfaces it as a clean "upgrade tclaude" error rather than mangling
// the data.
var ErrUnsupportedFormat = errors.New("unsupported group-export format version")

// Marshal serializes an Export into a zip archive.
//
// FormatVersion is stamped to the current constant regardless of the
// caller-supplied value, so an export always declares the format it was
// actually written in. Convs flagged Missing (no .jsonl found at export
// time) contribute a manifest entry but no file.
func Marshal(exp *Export) ([]byte, error) {
	if exp == nil {
		return nil, errors.New("groupexport.Marshal: nil export")
	}

	// Work on a shallow copy so stamping FormatVersion does not mutate
	// the caller's struct.
	manifest := *exp
	manifest.FormatVersion = FormatVersion

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	manifestBytes, err := json.MarshalIndent(&manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("groupexport.Marshal: encode manifest: %w", err)
	}
	if err := writeZipEntry(zw, manifestName, manifestBytes); err != nil {
		return nil, err
	}

	// Sort convs by id so the archive's entry order is stable across
	// exports of the same data.
	convs := append([]Conv(nil), exp.Convs...)
	sort.Slice(convs, func(i, j int) bool { return convs[i].ConvID < convs[j].ConvID })
	for _, c := range convs {
		// A non-Missing conv always gets a file entry — even one with
		// empty content (a valid, if degenerate, empty conversation).
		// Unmarshal requires a file for every non-Missing conv, so
		// skipping an empty one here would make the archive un-importable.
		if c.Missing {
			continue
		}
		name := convDirPrefix + c.ConvID + ".jsonl"
		if err := writeZipEntry(zw, name, c.Content); err != nil {
			return nil, err
		}
	}

	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("groupexport.Marshal: finalize zip: %w", err)
	}
	return buf.Bytes(), nil
}

// Unmarshal parses a zip archive back into an Export, populating each
// non-Missing Conv's Content from its projects/ file entry.
//
// It rejects, with a descriptive error, anything that is not a
// well-formed group-export archive: not a zip, no manifest, malformed
// manifest JSON, an unrecognised FormatVersion (ErrUnsupportedFormat),
// or a manifest conv whose .jsonl file entry is absent (a truncated
// archive).
func Unmarshal(data []byte) (*Export, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("not a valid group-export archive: %w", err)
	}

	var manifestFile *zip.File
	convFiles := make(map[string]*zip.File)
	for _, f := range zr.File {
		switch {
		case f.Name == manifestName:
			manifestFile = f
		case strings.HasPrefix(f.Name, convDirPrefix) && strings.HasSuffix(f.Name, ".jsonl"):
			convID := strings.TrimSuffix(path.Base(f.Name), ".jsonl")
			convFiles[convID] = f
		}
	}
	if manifestFile == nil {
		return nil, fmt.Errorf("group-export archive has no %s", manifestName)
	}

	manifestBytes, err := readZipFile(manifestFile)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", manifestName, err)
	}
	var exp Export
	if err := json.Unmarshal(manifestBytes, &exp); err != nil {
		return nil, fmt.Errorf("malformed %s: %w", manifestName, err)
	}

	if exp.FormatVersion <= 0 {
		return nil, fmt.Errorf("malformed %s: missing or invalid format_version", manifestName)
	}
	if exp.FormatVersion > FormatVersion {
		return nil, fmt.Errorf("%w: archive is format v%d, this tclaude understands up to v%d — upgrade tclaude",
			ErrUnsupportedFormat, exp.FormatVersion, FormatVersion)
	}

	// Re-attach each non-Missing conv's bytes from its file entry.
	for i := range exp.Convs {
		c := &exp.Convs[i]
		if c.Missing {
			continue
		}
		f, ok := convFiles[c.ConvID]
		if !ok {
			return nil, fmt.Errorf("truncated archive: manifest lists conv %s but %s%s.jsonl is missing",
				c.ConvID, convDirPrefix, c.ConvID)
		}
		content, err := readZipFile(f)
		if err != nil {
			return nil, fmt.Errorf("read conv %s: %w", c.ConvID, err)
		}
		c.Content = content
	}
	return &exp, nil
}

// writeZipEntry adds one deflate-compressed file to the archive.
func writeZipEntry(zw *zip.Writer, name string, content []byte) error {
	w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate})
	if err != nil {
		return fmt.Errorf("groupexport: create zip entry %q: %w", name, err)
	}
	if _, err := w.Write(content); err != nil {
		return fmt.Errorf("groupexport: write zip entry %q: %w", name, err)
	}
	return nil
}

// readZipFile reads one zip entry fully into memory.
func readZipFile(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}
