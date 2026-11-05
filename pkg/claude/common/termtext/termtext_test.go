package termtext

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The hostile shapes an agent- or peer-authored string can carry.
var hostile = []struct {
	name, in, want string
}{
	{"ESC CSI clears the screen", "a\x1b[2Jb", "a[2Jb"},
	{"OSC 8 disguises a link", "\x1b]8;;https://evil.example\x1b\\docs\x1b]8;;\x1b\\", "]8;;https://evil.example\\docs]8;;\\"},
	{"OSC 52 writes the clipboard", "\x1b]52;c;cm0gLXJmIH4=\x07", "]52;c;cm0gLXJmIH4="},
	{"raw 8-bit C1 CSI", "a\x9b2Jb", "a2Jb"},
	{"UTF-8 encoded C1 CSI", "a\u009b2Jb", "a2Jb"},
	{"bidi override", "invoice\u202egpj.exe", "invoicegpj.exe"},
	{"bidi isolate", "a\u2066b\u2069c", "abc"},
	{"carriage return overwrites the line", "safe\rEVIL", "safeEVIL"},
	{"backspace and DEL", "ab\b\x7fc", "abc"},
}

func TestCleanDropsTerminalControls(t *testing.T) {
	for _, tc := range hostile {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, Clean(tc.in))
		})
	}
}

func TestCleanKeepsOrdinaryText(t *testing.T) {
	for _, s := range []string{
		"plain", "line one\nline two\ttabbed", "↳ reply", "smörgåsbord", "日本語",
		"שלום \u200fעולם", "emoji 🙂", "",
	} {
		assert.Equal(t, s, Clean(s))
	}
}

func TestLineAndCellAreSingleLine(t *testing.T) {
	assert.Equal(t, "a b c", Line("a\nb\tc"))
	assert.Equal(t, "a b c", Cell("  a \n\n b\tc  "))
	assert.Equal(t, "x [31my", Cell("x \x1b[31my"))
}

func TestWriterCleansAcrossWrites(t *testing.T) {
	var out bytes.Buffer
	w := NewWriter(&out)
	for _, tc := range hostile {
		n, err := w.Write([]byte(tc.in))
		require.NoError(t, err)
		assert.Equal(t, len(tc.in), n)
	}
	var want string
	for _, tc := range hostile {
		want += tc.want
	}
	assert.Equal(t, want, out.String())

	// A multi-byte rune, or an encoded C1 control, split across writes.
	out.Reset()
	for _, s := range []string{"a\u202e"[:2], "\u202e"[1:] + "é"[:1], "é"[1:], "\u009b"[:1], "\u009b"[1:] + "z"} {
		_, err := w.Write([]byte(s))
		require.NoError(t, err)
	}
	assert.Equal(t, "aéz", out.String())
}
