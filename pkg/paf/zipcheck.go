package paf

import (
	"archive/zip"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// ZIP record signatures.
const (
	sigLocal          = 0x04034b50
	sigCentral        = 0x02014b50
	sigEnd            = 0x06054b50
	sigEnd64          = 0x06064b50
	sigEnd64Locator   = 0x07064b50
	sigDataDescriptor = 0x08074b50
)

var errZipLayout = errors.New("paf: ZIP central directory cannot be read")

// localHeaderOffsets parses the central directory itself and returns each
// entry's local header offset. archive/zip does not expose these offsets,
// and the local header must be read at its real position, never searched
// for: a search can be steered to a forged header inside an extra field.
func localHeaderOffsets(ra io.ReaderAt, size int64) (map[string]int64, error) {
	tailLen := min(size, 22+65535)
	tail := make([]byte, tailLen)
	if _, err := ra.ReadAt(tail, size-tailLen); err != nil {
		return nil, errZipLayout
	}
	end := -1
	for p := len(tail) - 22; p >= 0; p-- {
		if binary.LittleEndian.Uint32(tail[p:]) == sigEnd {
			end = p
			break
		}
	}
	if end < 0 {
		return nil, errZipLayout
	}
	eocd := tail[end:]
	count := uint64(binary.LittleEndian.Uint16(eocd[10:]))
	cdSize := uint64(binary.LittleEndian.Uint32(eocd[12:]))
	cdOff := uint64(binary.LittleEndian.Uint32(eocd[16:]))
	if count == 0xffff || cdSize == 0xffffffff || cdOff == 0xffffffff {
		// ZIP64: the locator sits immediately before the end record.
		locAt := size - tailLen + int64(end) - 20
		if locAt < 0 {
			return nil, errZipLayout
		}
		loc := make([]byte, 20)
		if _, err := ra.ReadAt(loc, locAt); err != nil || binary.LittleEndian.Uint32(loc) != sigEnd64Locator {
			return nil, errZipLayout
		}
		recAt := int64(binary.LittleEndian.Uint64(loc[8:]))
		rec := make([]byte, 56)
		if recAt < 0 || recAt > size-56 {
			return nil, errZipLayout
		}
		if _, err := ra.ReadAt(rec, recAt); err != nil || binary.LittleEndian.Uint32(rec) != sigEnd64 {
			return nil, errZipLayout
		}
		count = binary.LittleEndian.Uint64(rec[32:])
		cdSize = binary.LittleEndian.Uint64(rec[40:])
		cdOff = binary.LittleEndian.Uint64(rec[48:])
	}
	if cdOff+cdSize > uint64(size) || cdSize > 1<<30 {
		return nil, errZipLayout
	}
	cd := make([]byte, cdSize)
	if _, err := ra.ReadAt(cd, int64(cdOff)); err != nil {
		return nil, errZipLayout
	}
	out := make(map[string]int64, count)
	p := 0
	for i := uint64(0); i < count; i++ {
		if p+46 > len(cd) || binary.LittleEndian.Uint32(cd[p:]) != sigCentral {
			return nil, errZipLayout
		}
		h := cd[p:]
		nameLen := int(binary.LittleEndian.Uint16(h[28:]))
		extraLen := int(binary.LittleEndian.Uint16(h[30:]))
		commentLen := int(binary.LittleEndian.Uint16(h[32:]))
		if p+46+nameLen+extraLen+commentLen > len(cd) {
			return nil, errZipLayout
		}
		name := string(h[46 : 46+nameLen])
		usize := uint64(binary.LittleEndian.Uint32(h[24:]))
		csize := uint64(binary.LittleEndian.Uint32(h[20:]))
		off := uint64(binary.LittleEndian.Uint32(h[42:]))
		if off == 0xffffffff {
			extra := h[46+nameLen : 46+nameLen+extraLen]
			for len(extra) >= 4 {
				id := binary.LittleEndian.Uint16(extra)
				n := int(binary.LittleEndian.Uint16(extra[2:]))
				if 4+n > len(extra) {
					return nil, errZipLayout
				}
				if id == 0x0001 {
					f := extra[4 : 4+n]
					for _, v := range []uint64{usize, csize} {
						if v == 0xffffffff {
							if len(f) < 8 {
								return nil, errZipLayout
							}
							f = f[8:]
						}
					}
					if len(f) < 8 {
						return nil, errZipLayout
					}
					off = binary.LittleEndian.Uint64(f)
				}
				extra = extra[4+n:]
			}
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("paf: duplicate entry %q", name)
		}
		out[name] = int64(off)
		p += 46 + nameLen + extraLen + commentLen
	}
	return out, nil
}

// checkLocalHeader refuses an entry whose local file header, read at its
// real offset, disagrees with the central directory, so readers that stream
// local headers see the same archive this one does.
func checkLocalHeader(ra io.ReaderAt, size int64, f *zip.File, offset int64) error {
	bad := fmt.Errorf("paf: entry %q: local header does not match the central directory", f.Name)
	if offset < 0 || offset > size-30 {
		return bad
	}
	hdr := make([]byte, 30+len(f.Name))
	if _, err := ra.ReadAt(hdr, offset); err != nil {
		return bad
	}
	if binary.LittleEndian.Uint32(hdr) != sigLocal {
		return bad
	}
	flags := binary.LittleEndian.Uint16(hdr[6:])
	method := binary.LittleEndian.Uint16(hdr[8:])
	nameLen := int(binary.LittleEndian.Uint16(hdr[26:]))
	extraLen := int64(binary.LittleEndian.Uint16(hdr[28:]))
	if nameLen != len(f.Name) || string(hdr[30:]) != f.Name || method != f.Method {
		return bad
	}
	data, err := f.DataOffset()
	if err != nil || data != offset+30+int64(nameLen)+extraLen {
		return bad
	}
	if flags&0x8 == 0 {
		crc := binary.LittleEndian.Uint32(hdr[14:])
		csize := binary.LittleEndian.Uint32(hdr[18:])
		usize := binary.LittleEndian.Uint32(hdr[22:])
		if crc != f.CRC32 || (csize != 0xffffffff && uint64(csize) != f.CompressedSize64) || (usize != 0xffffffff && uint64(usize) != f.UncompressedSize64) {
			return bad
		}
		return nil
	}
	// A data descriptor follows the data. It must sit exactly where the
	// central directory says the data ends and agree with it, so a
	// streaming reader finds the same boundary and sizes.
	at := data + int64(f.CompressedSize64)
	dd := make([]byte, 24)
	n, _ := ra.ReadAt(dd, at)
	dd = dd[:n]
	if len(dd) >= 4 && binary.LittleEndian.Uint32(dd) == sigDataDescriptor {
		dd = dd[4:]
	}
	if len(dd) < 12 || binary.LittleEndian.Uint32(dd) != f.CRC32 {
		return bad
	}
	if uint64(binary.LittleEndian.Uint32(dd[4:])) == f.CompressedSize64 && uint64(binary.LittleEndian.Uint32(dd[8:])) == f.UncompressedSize64 {
		return nil
	}
	if len(dd) >= 20 && binary.LittleEndian.Uint64(dd[4:]) == f.CompressedSize64 && binary.LittleEndian.Uint64(dd[12:]) == f.UncompressedSize64 {
		return nil
	}
	return bad
}
