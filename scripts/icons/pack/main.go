// Command pack builds the launcher's raster icons from one large render of
// the logo: it downscales a square PNG to every size the macOS .icns needs
// and packs them, and writes a downscaled PNG. scripts/icons/
// make-launcher-icons.sh drives it; it is not a production tool.
//
// Every size comes from the one source image so a single bad render cannot
// truncate one size only (headless Chrome at small window sizes did that to
// the 128 px entry Spotlight shows).
//
// Usage:
//
//	pack -icns <src.png> <out.icns>
//	pack -png <size> <src.png> <out.png>
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/png"
	"os"
	"strconv"

	"golang.org/x/image/draw"
)

// icnsEntries lists the PNG-backed .icns entry types and their pixel sizes,
// in the order iconutil writes them. ic11..ic14 are the @2x variants of
// 16, 32, 128 and 256.
var icnsEntries = []struct {
	code string
	size int
}{
	{"ic07", 128}, {"ic08", 256}, {"ic09", 512}, {"ic10", 1024},
	{"ic11", 32}, {"ic12", 64}, {"ic13", 256}, {"ic14", 512},
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pack:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	switch {
	case len(args) == 3 && args[0] == "-icns":
		src, err := load(args[1])
		if err != nil {
			return err
		}
		return writeICNS(src, args[2])
	case len(args) == 4 && args[0] == "-png":
		size, err := strconv.Atoi(args[1])
		if err != nil || size <= 0 {
			return fmt.Errorf("bad size %q", args[1])
		}
		src, err := load(args[2])
		if err != nil {
			return err
		}
		data, err := encode(src, size)
		if err != nil {
			return err
		}
		return os.WriteFile(args[3], data, 0o644)
	}
	return fmt.Errorf("usage: pack -icns <src.png> <out.icns> | pack -png <size> <src.png> <out.png>")
}

func load(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if b := img.Bounds(); b.Dx() != b.Dy() {
		return nil, fmt.Errorf("%s: %dx%d is not square", path, b.Dx(), b.Dy())
	}
	return img, nil
}

// encode returns src scaled to size x size as a PNG.
func encode(src image.Image, size int) ([]byte, error) {
	dst := image.NewNRGBA(image.Rect(0, 0, size, size))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Src, nil)
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	if err := enc.Encode(&buf, dst); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeICNS(src image.Image, path string) error {
	var body bytes.Buffer
	for _, e := range icnsEntries {
		data, err := encode(src, e.size)
		if err != nil {
			return err
		}
		body.WriteString(e.code)
		_ = binary.Write(&body, binary.BigEndian, uint32(len(data)+8))
		body.Write(data)
	}
	var out bytes.Buffer
	out.WriteString("icns")
	_ = binary.Write(&out, binary.BigEndian, uint32(body.Len()+8))
	out.Write(body.Bytes())
	return os.WriteFile(path, out.Bytes(), 0o644)
}
