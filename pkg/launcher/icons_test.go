package launcher

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"image"
	"image/png"
	"math"
	"testing"
)

//go:embed assets/tclaude.png
var launcherIconPNG []byte

// icnsSizes maps each PNG-backed .icns entry type to its pixel size.
var icnsSizes = map[string]int{
	"ic07": 128, "ic08": 256, "ic09": 512, "ic10": 1024,
	"ic11": 32, "ic12": 64, "ic13": 256, "ic14": 512, // ic11..ic14 are the @2x of 16..256
}

// TestLauncherICNSEntriesComplete guards the macOS bundle icon: every
// entry must be a PNG of the size its type code names, carrying the whole
// logo. Spotlight shows the 128 px entry, and an earlier asset had a
// truncated render there (only the top of the circle drawn).
func TestLauncherICNSEntriesComplete(t *testing.T) {
	entries := parseICNS(t, launcherIconICNS)
	for code, size := range icnsSizes {
		data, ok := entries[code]
		if !ok {
			t.Errorf("%s: missing", code)
			continue
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Errorf("%s: not a PNG: %v", code, err)
			continue
		}
		if b := img.Bounds(); b.Dx() != size || b.Dy() != size {
			t.Errorf("%s: %dx%d, want %dx%d", code, b.Dx(), b.Dy(), size, size)
			continue
		}
		// The SVG (r=10 circle in a 22-unit viewBox) fills the middle 80%
		// of the canvas, a 10% margin per side.
		r := 0.8 * 10 / 22
		checkLogo(t, code, img, math.Pi*r*r)
	}
	if len(entries) != len(icnsSizes) {
		t.Errorf("icns has %d entries, want %d", len(entries), len(icnsSizes))
	}
}

// TestLauncherPNGComplete guards the 256 px PNG, rendered without a margin.
func TestLauncherPNGComplete(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(launcherIconPNG))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 256 || b.Dy() != 256 {
		t.Fatalf("tclaude.png is %dx%d, want 256x256", b.Dx(), b.Dy())
	}
	// The SVG's circle has r=10 in a 22-unit viewBox.
	checkLogo(t, "tclaude.png", img, math.Pi*(10.0/22)*(10.0/22))
}

// checkLogo checks that img holds the whole round logo: its opaque area is
// close to wantFrac of the canvas, split evenly between the top and bottom
// halves and between the left and right halves, and the logo's centre pixel
// is opaque.
func checkLogo(t *testing.T, name string, img image.Image, wantFrac float64) {
	t.Helper()
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	var total, top, bottom, left, right float64
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			_, _, _, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			alpha := float64(a) / 0xffff
			total += alpha
			if y < h/2 {
				top += alpha
			} else {
				bottom += alpha
			}
			if x < w/2 {
				left += alpha
			} else {
				right += alpha
			}
		}
	}
	frac := total / float64(w*h)
	if math.Abs(frac-wantFrac) > 0.05 {
		t.Errorf("%s: opaque area %.3f of the canvas, want about %.3f", name, frac, wantFrac)
	}
	if top == 0 || bottom == 0 || math.Abs(top-bottom)/(top+bottom) > 0.05 {
		t.Errorf("%s: top/bottom opaque area %.0f/%.0f, want about equal", name, top, bottom)
	}
	if left == 0 || right == 0 || math.Abs(left-right)/(left+right) > 0.05 {
		t.Errorf("%s: left/right opaque area %.0f/%.0f, want about equal", name, left, right)
	}
	if _, _, _, a := img.At(b.Min.X+w/2, b.Min.Y+h/2).RGBA(); a < 0xf000 {
		t.Errorf("%s: centre pixel alpha %#x, want opaque", name, a)
	}
}

// parseICNS splits an .icns file into its entries by OSType.
func parseICNS(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	if len(data) < 8 || string(data[:4]) != "icns" {
		t.Fatal("not an icns file")
	}
	if n := binary.BigEndian.Uint32(data[4:8]); int(n) != len(data) {
		t.Fatalf("icns header length %d, file is %d bytes", n, len(data))
	}
	entries := map[string][]byte{}
	for off := 8; off < len(data); {
		if off+8 > len(data) {
			t.Fatalf("truncated entry header at %d", off)
		}
		code := string(data[off : off+4])
		n := int(binary.BigEndian.Uint32(data[off+4 : off+8]))
		if n < 8 || off+n > len(data) {
			t.Fatalf("%s: bad length %d at %d", code, n, off)
		}
		if _, dup := entries[code]; dup {
			t.Errorf("%s: duplicate entry", code)
		}
		entries[code] = data[off+8 : off+n]
		off += n
	}
	return entries
}
