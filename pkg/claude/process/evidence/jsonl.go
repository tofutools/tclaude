package evidence

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func EncodeLogEntry(entry LogEntry) ([]byte, error) {
	if entry.SchemaVersion == 0 {
		entry.SchemaVersion = LogEntrySchemaVersion
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return nil, fmt.Errorf("encode log entry: %w", err)
	}
	return append(data, '\n'), nil
}

func EncodeManifestEntry(entry ManifestEntry) ([]byte, error) {
	if entry.SchemaVersion == 0 {
		entry.SchemaVersion = ManifestEntrySchemaVersion
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return nil, fmt.Errorf("encode manifest entry: %w", err)
	}
	return append(data, '\n'), nil
}

func AppendLogEntry(w io.Writer, entry LogEntry) error {
	data, err := EncodeLogEntry(entry)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func AppendManifestEntry(w io.Writer, entry ManifestEntry) error {
	data, err := EncodeManifestEntry(entry)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func ReadNodeLog(nodeID string, r io.Reader) ([]LogEntry, error) {
	lines, err := readJSONLLines(r)
	if err != nil {
		return nil, err
	}
	entries := make([]LogEntry, 0, len(lines))
	for _, line := range lines {
		var header struct {
			SchemaVersion int `json:"schemaVersion"`
		}
		if err := decodeLineVersion(line.Data, &header); err != nil {
			return nil, &ReadError{Kind: ReadErrorMalformed, Line: line.Number, Err: err}
		}
		if err := checkLogEntryVersion(header.SchemaVersion); err != nil {
			return nil, &ReadError{Kind: ReadErrorMalformed, Line: line.Number, Err: err}
		}
		var entry LogEntry
		if err := strictDecodeLine(line.Data, &entry); err != nil {
			return nil, &ReadError{Kind: ReadErrorMalformed, Line: line.Number, Err: err}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func ReadManifest(r io.Reader) ([]ManifestEntry, error) {
	lines, err := readJSONLLines(r)
	if err != nil {
		return nil, err
	}
	entries := make([]ManifestEntry, 0, len(lines))
	for _, line := range lines {
		var header struct {
			SchemaVersion int `json:"schemaVersion"`
		}
		if err := decodeLineVersion(line.Data, &header); err != nil {
			return nil, &ReadError{Kind: ReadErrorMalformed, Line: line.Number, Err: err}
		}
		if err := checkManifestEntryVersion(header.SchemaVersion); err != nil {
			return nil, &ReadError{Kind: ReadErrorMalformed, Line: line.Number, Err: err}
		}
		var entry ManifestEntry
		if err := strictDecodeLine(line.Data, &entry); err != nil {
			return nil, &ReadError{Kind: ReadErrorMalformed, Line: line.Number, Err: err}
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

type jsonlLine struct {
	Number int
	Data   []byte
}

func readJSONLLines(r io.Reader) ([]jsonlLine, error) {
	reader := bufio.NewReader(r)
	var lines []jsonlLine
	lineNumber := 0
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNumber++
			if line[len(line)-1] != '\n' {
				if errors.Is(err, io.EOF) {
					return nil, &ReadError{Kind: ReadErrorTornTail, Line: lineNumber, Err: fmt.Errorf("final JSONL line is not newline-terminated")}
				}
				return nil, err
			}
			line = bytes.TrimSuffix(line, []byte{'\n'})
			line = bytes.TrimSuffix(line, []byte{'\r'})
			if len(bytes.TrimSpace(line)) > 0 {
				lines = append(lines, jsonlLine{Number: lineNumber, Data: line})
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return lines, nil
}

func strictDecodeLine(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}

func decodeLineVersion(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	return dec.Decode(dst)
}

func checkLogEntryVersion(version int) error {
	if version != LogEntrySchemaVersion {
		return fmt.Errorf("unsupported log entry schema version %d", version)
	}
	return nil
}

func checkManifestEntryVersion(version int) error {
	if version != ManifestEntrySchemaVersion {
		return fmt.Errorf("unsupported manifest entry schema version %d", version)
	}
	return nil
}
