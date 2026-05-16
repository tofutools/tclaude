package groupexport

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sampleExport builds an Export covering a representative slice of every
// section, used as the round-trip fixture.
func sampleExport() *Export {
	return &Export{
		FormatVersion: FormatVersion,
		ExportedAt:    "2026-05-16T12:00:00Z",
		SchemaVersion: 40,
		SourceGroup:   "team",
		SourceHome:    "/home/alice",
		SourceOS:      "linux",
		Group: Group{
			Descr:          "the team",
			DefaultContext: "be nice",
			MaxMembers:     5,
			CreatedAt:      "2026-05-01T00:00:00Z",
		},
		Members: []Member{
			{ConvID: "conv-a", Role: "lead", JoinedAt: "2026-05-01T01:00:00Z"},
			{ConvID: "conv-b", Role: "worker", JoinedAt: "2026-05-01T02:00:00Z"},
		},
		Owners:      []Owner{{ConvID: "conv-a", GrantedAt: "2026-05-01T03:00:00Z", GrantedBy: "human"}},
		Permissions: []Permission{{ConvID: "conv-a", Slug: "groups.spawn", Effect: "grant", GrantedAt: "2026-05-01T04:00:00Z"}},
		Messages:    []Message{{ID: 1, FromConv: "conv-a", ToConv: "conv-b", Body: "hi", CreatedAt: "2026-05-01T05:00:00Z"}},
		Convs: []Conv{
			{ConvID: "conv-a", SourceCwd: "/home/alice/proj", Title: "alice", Content: []byte(`{"type":"summary"}` + "\n")},
			{ConvID: "conv-b", SourceCwd: "/home/alice/proj", Title: "bob", Missing: true},
		},
	}
}

// TestMarshalUnmarshalRoundTrip pins that a full Export survives a
// serialize → deserialize cycle byte-for-byte, including the manifest
// rows and the per-conv .jsonl bytes carried as real zip file entries.
func TestMarshalUnmarshalRoundTrip(t *testing.T) {
	orig := sampleExport()

	archive, err := Marshal(orig)
	require.NoError(t, err, "Marshal")

	got, err := Unmarshal(archive)
	require.NoError(t, err, "Unmarshal")

	assert.Equal(t, FormatVersion, got.FormatVersion)
	assert.Equal(t, orig.SourceGroup, got.SourceGroup)
	assert.Equal(t, orig.SourceHome, got.SourceHome)
	assert.Equal(t, orig.Group, got.Group)
	assert.Equal(t, orig.Members, got.Members)
	assert.Equal(t, orig.Owners, got.Owners)
	assert.Equal(t, orig.Permissions, got.Permissions)
	assert.Equal(t, orig.Messages, got.Messages)

	require.Len(t, got.Convs, 2)
	assert.Equal(t, []byte(`{"type":"summary"}`+"\n"), got.Convs[0].Content,
		"conv-a content round-trips")
	assert.Equal(t, "alice", got.Convs[0].Title)
	assert.True(t, got.Convs[1].Missing, "conv-b stays flagged Missing")
	assert.Nil(t, got.Convs[1].Content, "a Missing conv carries no bytes")
}

// TestMarshalStampsFormatVersion confirms Marshal always writes the
// current FormatVersion regardless of what the caller set.
func TestMarshalStampsFormatVersion(t *testing.T) {
	exp := sampleExport()
	exp.FormatVersion = 999
	archive, err := Marshal(exp)
	require.NoError(t, err)
	got, err := Unmarshal(archive)
	require.NoError(t, err)
	assert.Equal(t, FormatVersion, got.FormatVersion)
}

// TestUnmarshalNotAZip rejects bytes that are not a zip archive at all —
// the "someone uploaded a random file" case.
func TestUnmarshalNotAZip(t *testing.T) {
	_, err := Unmarshal([]byte("this is definitely not a zip archive"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a valid group-export archive")
}

// TestUnmarshalNoManifest rejects a zip that is structurally fine but
// carries no manifest.json.
func TestUnmarshalNoManifest(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(convDirPrefix + "conv-x.jsonl")
	require.NoError(t, err)
	_, _ = w.Write([]byte("{}"))
	require.NoError(t, zw.Close())

	_, err = Unmarshal(buf.Bytes())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no manifest.json")
}

// TestUnmarshalUnsupportedVersion rejects an archive whose manifest
// declares a FormatVersion newer than this binary understands, and the
// error is identifiable via errors.Is(ErrUnsupportedFormat).
func TestUnmarshalUnsupportedVersion(t *testing.T) {
	archive := zipWithManifest(t, `{"format_version":999,"source_group":"team"}`)
	_, err := Unmarshal(archive)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnsupportedFormat),
		"a too-new format must surface as ErrUnsupportedFormat, got: %v", err)
}

// TestUnmarshalMissingFormatVersion rejects a manifest with no (or zero)
// format_version — a malformed or hand-mangled archive.
func TestUnmarshalMissingFormatVersion(t *testing.T) {
	archive := zipWithManifest(t, `{"source_group":"team"}`)
	_, err := Unmarshal(archive)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "format_version")
}

// TestUnmarshalMalformedManifest rejects a manifest that is not valid
// JSON — the "truncated / corrupted archive" case.
func TestUnmarshalMalformedManifest(t *testing.T) {
	archive := zipWithManifest(t, `{"format_version":1,"source_group":`)
	_, err := Unmarshal(archive)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "malformed")
}

// TestUnmarshalTruncatedConv rejects an archive whose manifest lists a
// non-Missing conv whose .jsonl file entry is absent.
func TestUnmarshalTruncatedConv(t *testing.T) {
	exp := &Export{
		FormatVersion: FormatVersion,
		SourceGroup:   "team",
		Convs:         []Conv{{ConvID: "conv-gone", Content: []byte("x")}},
	}
	manifest, err := json.Marshal(exp)
	require.NoError(t, err)
	// Zip with the manifest only — the conv's file entry is deliberately
	// omitted, modelling a truncated archive.
	archive := zipWithManifest(t, string(manifest))

	_, err = Unmarshal(archive)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "truncated")
}

// zipWithManifest builds a zip carrying just manifest.json with the
// given raw content — the fixture for the malformed-archive cases.
func zipWithManifest(t *testing.T, manifestJSON string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(manifestName)
	require.NoError(t, err)
	_, err = w.Write([]byte(manifestJSON))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}
