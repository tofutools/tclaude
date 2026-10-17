package terminal

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestKeyboardFixedNamesLiteralTextAndPartialEscapes(t *testing.T) {
	var k Keyboard
	keys, dropped := k.Feed([]byte("hello;\r\n\t\x7f\x03\x1b[A\x1b[1;5D\x1bOP\x1b[24~"))
	require.Zero(t, dropped)
	require.Equal(t, []Key{{Literal: "hello;"}, {Name: "Enter"}, {Name: "C-j"}, {Name: "Tab"}, {Name: "BSpace"}, {Name: "C-c"}, {Name: "Up"}, {Name: "C-Left"}, {Name: "F1"}, {Name: "F12"}}, keys)
	keys, _ = k.Feed([]byte("\x1b[1;"))
	require.Empty(t, keys)
	keys, dropped = k.Feed([]byte("2B"))
	require.Zero(t, dropped)
	require.Equal(t, []Key{{Name: "S-Down"}}, keys)
	keys, _ = k.Feed([]byte("\x1b"))
	require.Empty(t, keys)
	keys, dropped = k.Flush()
	require.Zero(t, dropped)
	require.Equal(t, []Key{{Name: "Escape"}}, keys)
	keys, dropped = k.Feed([]byte("\x1b[999~"))
	require.Empty(t, keys)
	require.Equal(t, 1, dropped)
	keys, dropped = k.Feed([]byte("\x1b[123"))
	require.Empty(t, keys)
	require.Zero(t, dropped)
	keys, dropped = k.Flush()
	require.Empty(t, keys)
	require.Equal(t, 1, dropped)
	require.Equal(t, "hello\\;", TmuxLiteralArg("hello;"))
	require.Equal(t, "\\\\;", TmuxLiteralArg("\\;"))
	require.Equal(t, "; kill-server", TmuxLiteralArg("; kill-server"))
	_, err := Encode(Frame{Kind: Report, Data: []byte("\x1b[?1c")})
	require.ErrorIs(t, err, ErrProtocol, "terminal reports cannot be wire frames")
}
func TestKeyboardPasteIsLiteralAndBoundedAcrossFrames(t *testing.T) {
	var k Keyboard
	a, dropped := k.Feed([]byte("\x1b[200~yes\r\n; run-shell bad\x1b[20"))
	require.Zero(t, dropped)
	b, dropped := k.Feed([]byte("1~\r"))
	require.Zero(t, dropped)
	all := append(a, b...)
	text := ""
	for _, key := range all[:len(all)-1] {
		require.Empty(t, key.Name)
		text += key.Literal
	}
	require.Equal(t, "yes\r\n; run-shell bad", text)
	require.Equal(t, Key{Name: "Enter"}, all[len(all)-1])
	for i := 0; i < 1000; i++ {
		_, _ = k.Feed([]byte("\x1b[200~aaaaaaaaaaaaaaaaaaaaaaaaaaaa"))
	}
	require.LessOrEqual(t, len(k.pending), 5)
}

func TestKeyboardMouseReportsAreParsedNotForwarded(t *testing.T) {
	var k Keyboard
	keys, dropped := k.Feed([]byte("\x1b[<64;3;4M\x1b[<81;1;2M\x1b[<0;3;4M\x1b[<0;3;4m\x1b[<34;5;6M\x1b[<66;3;4M\x1b[<64;3;4m\x1b[<128;3;4M"))
	require.Equal(t, []Key{
		{Mouse: &Mouse{Button: 64, X: 3, Y: 4}},
		{Mouse: &Mouse{Button: 81, X: 1, Y: 2}},
		{Mouse: &Mouse{Button: 0, X: 3, Y: 4}},
		{Mouse: &Mouse{Button: 0, X: 3, Y: 4, Release: true}},
		{Mouse: &Mouse{Button: 34, X: 5, Y: 6}},
	}, keys)
	require.Equal(t, 3, dropped, "horizontal wheel, wheel releases and extra buttons are not forwarded")
	require.True(t, keys[0].Mouse.Wheel() && keys[0].Mouse.Up())
	require.True(t, keys[1].Mouse.Wheel() && !keys[1].Mouse.Up())
	require.False(t, keys[2].Mouse.Wheel())
	require.True(t, keys[4].Mouse.Motion())
	// A legacy X10 report's raw coordinate bytes never leak as literal text.
	keys, dropped = k.Feed([]byte("\x1b[M`!"))
	require.Empty(t, keys)
	require.Zero(t, dropped)
	keys, dropped = k.Feed([]byte("\"x"))
	require.Zero(t, dropped)
	require.Equal(t, []Key{{Mouse: &Mouse{Button: 64, X: 1, Y: 2}}, {Literal: "x"}}, keys)
	keys, dropped = k.Feed([]byte("\x1b[M \x21\x21"))
	require.Equal(t, []Key{{Mouse: &Mouse{Button: 0, X: 1, Y: 1}}}, keys)
	require.Zero(t, dropped)
}
