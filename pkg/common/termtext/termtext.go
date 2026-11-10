// Package termtext makes agent- and peer-authored text safe to print to a
// terminal.
//
// Text another agent, a federated peer or a template author wrote (message
// subjects and bodies, display names, group names, drawing board and page titles)
// reaches the operator's terminal through the CLI and the TUIs. Printed raw, a
// control sequence in it is interpreted by the terminal rather than shown: an
// ESC or C1 CSI sequence can rewrite or hide what is on screen, an OSC 8
// sequence can disguise a link, OSC 52 can write the clipboard, and a bidi
// override (U+202E) can make text read differently from what it is.
//
// Everything here drops the same runes, so every printing path agrees on what
// is safe: C0 controls except newline and tab, DEL, C1 controls, bytes that
// are not valid UTF-8 (a raw 0x9b is an 8-bit CSI on some terminals), and the
// bidi embedding, override and isolate controls. Bidi marks (U+200E, U+200F,
// U+061C) stay; they only affect the direction of neutral characters next to
// them.
package termtext

import (
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// UnsafeRune reports whether r must not reach a terminal: C0 but newline and
// tab, DEL, C1, and the bidi embedding, override and isolate controls.
// proto.StripControls uses it on federation ingress, so a peer's text and a
// local printer agree.
func UnsafeRune(r rune) bool {
	switch {
	case r == '\n' || r == '\t':
		return false
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		return true
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	}
	return false
}

// Clean returns s without the runes a terminal would interpret, and without
// bytes that are not valid UTF-8. Newlines and tabs are kept.
func Clean(s string) string {
	if isClean(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r != utf8.RuneError || size != 1) && !UnsafeRune(r) {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func isClean(s string) bool {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if (r == utf8.RuneError && size == 1) || UnsafeRune(r) {
			return false
		}
		i += size
	}
	return true
}

// Line is Clean for a single line of output: newlines and tabs become
// spaces, so the text cannot break a table row or a one-line field.
func Line(s string) string {
	s = Clean(s)
	if !strings.ContainsAny(s, "\n\t") {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		return r
	}, s)
}

// Cell is Clean for a table cell that also collapses every run of
// whitespace into one space and trims the ends.
func Cell(s string) string {
	return strings.Join(strings.Fields(Clean(s)), " ")
}

// Writer cleans everything written through it, as Clean does, before passing
// it on. A UTF-8 sequence split across two writes is held until it completes.
type Writer struct {
	w       io.Writer
	pending []byte
}

// NewWriter returns a Writer that cleans what it passes to w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w} }

// Write cleans p and writes the result. It reports len(p) on success, so
// callers that check for short writes are not confused by the bytes it drops.
func (w *Writer) Write(p []byte) (int, error) {
	data := p
	if len(w.pending) > 0 {
		data = append(w.pending, p...)
		w.pending = nil
	}
	// Hold back an incomplete UTF-8 sequence at the end for the next write.
	cut := len(data)
	for back := 1; back < utf8.UTFMax && back <= len(data); back++ {
		c := data[len(data)-back]
		if c < 0x80 {
			break
		}
		if utf8.RuneStart(c) {
			if !utf8.FullRune(data[len(data)-back:]) {
				cut = len(data) - back
			}
			break
		}
	}
	if cut < len(data) {
		w.pending = append([]byte(nil), data[cut:]...)
	}
	if _, err := io.WriteString(w.w, Clean(string(data[:cut]))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// ForTerminal returns f wrapped in a Writer when f is a terminal, and f
// itself otherwise. Only a terminal interprets control sequences; output
// that is piped or redirected (a --json listing, an export) is data, and is
// passed through byte for byte.
func ForTerminal(f *os.File) io.Writer {
	if f != nil && term.IsTerminal(int(f.Fd())) {
		return NewWriter(f)
	}
	return f
}

// Stdout and Stderr are the process's standard streams for a command that
// prints agent- or peer-authored text: cleaned on a terminal, untouched when
// piped or redirected (ForTerminal). One Writer per stream, so a rune split
// across two writes from different packages still joins.
var (
	Stdout = ForTerminal(os.Stdout)
	Stderr = ForTerminal(os.Stderr)
)
