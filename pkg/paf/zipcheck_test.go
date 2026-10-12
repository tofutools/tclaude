package paf

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"
)

// zip64LyingCount rewrites a one-entry ZIP into ZIP64 form whose end
// records declare 2^32+1 entries.
func zip64LyingCount(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "a", Method: zip.Store})
	require.NoError(t, err)
	_, err = w.Write([]byte("x"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	raw := buf.Bytes()
	eocd := append([]byte(nil), raw[len(raw)-22:]...)
	cdSize := uint64(binary.LittleEndian.Uint32(eocd[12:]))
	cdOff := uint64(binary.LittleEndian.Uint32(eocd[16:]))
	const lie = 1<<32 + 1
	rec := make([]byte, 56)
	binary.LittleEndian.PutUint32(rec, sigEnd64)
	binary.LittleEndian.PutUint64(rec[4:], 44)
	binary.LittleEndian.PutUint16(rec[12:], 45)
	binary.LittleEndian.PutUint16(rec[14:], 45)
	binary.LittleEndian.PutUint64(rec[24:], lie)
	binary.LittleEndian.PutUint64(rec[32:], lie)
	binary.LittleEndian.PutUint64(rec[40:], cdSize)
	binary.LittleEndian.PutUint64(rec[48:], cdOff)
	loc := make([]byte, 20)
	binary.LittleEndian.PutUint32(loc, sigEnd64Locator)
	binary.LittleEndian.PutUint64(loc[8:], uint64(len(raw)-22))
	binary.LittleEndian.PutUint32(loc[16:], 1)
	binary.LittleEndian.PutUint16(eocd[8:], 0xffff)
	binary.LittleEndian.PutUint16(eocd[10:], 0xffff)
	out := append(append(append(append([]byte(nil), raw[:len(raw)-22]...), rec...), loc...), eocd...)
	return out
}

// A declared entry count never sizes an allocation: a tiny archive that
// claims 2^32+1 entries is refused instead of exhausting memory.
func TestZip64LyingEntryCountIsRefused(t *testing.T) {
	raw := zip64LyingCount(t)
	// archive/zip accepts it and reads its one real entry.
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	require.NoError(t, err)
	require.Len(t, zr.File, 1)
	_, err = localHeaderOffsets(bytes.NewReader(raw), int64(len(raw)), 1)
	require.ErrorIs(t, err, errZipLayout)
	_, err = Open(bytes.NewReader(raw), int64(len(raw)), Options{Purpose: PurposeImport})
	require.ErrorIs(t, err, errZipLayout)
}
