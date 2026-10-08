package terminal

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

// Key is either literal application text or a name from our fixed tmux table.
// User text never becomes a tmux key name, flag, command or format string.
type Key struct {
	Literal string
	Name    string
}
type Keyboard struct {
	pending []byte
	paste   bool
	discard bool
}

var controls = [...]string{"C-@", "C-a", "C-b", "C-c", "C-d", "C-e", "C-f", "C-g", "BSpace", "Tab", "C-j", "C-k", "C-l", "Enter", "C-n", "C-o", "C-p", "C-q", "C-r", "C-s", "C-t", "C-u", "C-v", "C-w", "C-x", "C-y", "C-z", "Escape", "C-\\", "C-]", "C-^", "C-_"}
var escapeKeys = func() map[string]string {
	m := map[string]string{"[A": "Up", "[B": "Down", "[C": "Right", "[D": "Left", "[H": "Home", "[F": "End", "OH": "Home", "OF": "End", "OA": "Up", "OB": "Down", "OC": "Right", "OD": "Left", "OP": "F1", "OQ": "F2", "OR": "F3", "OS": "F4", "[Z": "BTab"}
	tilde := map[string]string{"1": "Home", "2": "IC", "3": "DC", "4": "End", "5": "PPage", "6": "NPage", "7": "Home", "8": "End", "11": "F1", "12": "F2", "13": "F3", "14": "F4", "15": "F5", "17": "F6", "18": "F7", "19": "F8", "20": "F9", "21": "F10", "23": "F11", "24": "F12"}
	for n, k := range tilde {
		m["["+n+"~"] = k
	}
	modifiers := []string{"", "", "S-", "M-", "M-S-", "C-", "C-S-", "C-M-", "C-M-S-"}
	digits := []string{"", "", "2", "3", "4", "5", "6", "7", "8"}
	for i := 2; i <= 8; i++ {
		for code, k := range map[string]string{"A": "Up", "B": "Down", "C": "Right", "D": "Left", "H": "Home", "F": "End", "P": "F1", "Q": "F2", "R": "F3", "S": "F4"} {
			m["[1;"+digits[i]+code] = modifiers[i] + k
		}
		for n, k := range tilde {
			m["["+n+";"+digits[i]+"~"] = modifiers[i] + k
		}
	}
	return m
}()

func (k *Keyboard) Pending() bool { return len(k.pending) > 0 }

func (k *Keyboard) Feed(data []byte) ([]Key, int) {
	k.pending = append(k.pending, data...)
	return k.parse(false)
}

// Flush resolves a standalone Escape after a brief input idle interval. Other
// incomplete sequences are discarded, never forwarded raw to the pane.
func (k *Keyboard) Flush() ([]Key, int) { return k.parse(true) }
func (k *Keyboard) parse(flush bool) (out []Key, dropped int) {
	literal := func(p []byte) {
		if len(p) > 0 {
			out = append(out, Key{Literal: string(p)})
		}
	}
	for len(k.pending) > 0 {
		p := k.pending
		if k.discard {
			end := bytes.IndexByte(p, 7)
			if st := bytes.Index(p, []byte("\x1b\\")); st >= 0 && (end < 0 || st < end) {
				end = st + 1
			}
			if end >= 0 {
				k.pending = p[end+1:]
				k.discard = false
				continue
			}
			// Retain only a possible split ST terminator, never an unbounded string.
			if p[len(p)-1] == 27 {
				k.pending = p[len(p)-1:]
			} else {
				k.pending = nil
			}
			break
		}
		if k.paste {
			if end := bytes.Index(p, []byte("\x1b[201~")); end >= 0 {
				literal(p[:end])
				k.pending = p[end+6:]
				k.paste = false
				continue
			}
			// Keep enough tail to recognize a closing delimiter split across frames.
			n := len(p) - 5

			if n <= 0 {
				break
			}
			literal(p[:n])
			k.pending = p[n:]
			continue
		}
		if p[0] == 27 {
			if len(p) == 1 {
				if flush {
					out = append(out, Key{Name: "Escape"})
					k.pending = nil
				}
				break
			}
			if p[1] == ']' || p[1] == 'P' || p[1] == '^' || p[1] == '_' {
				k.discard = true
				k.pending = p[2:]
				dropped++
				continue
			}
			if p[1] < 32 {
				out = append(out, Key{Name: "Escape"})
				k.pending = p[1:]
				continue
			}
			n := 2
			if p[1] == '[' {
				for n < len(p) && (p[n] < 0x40 || p[n] > 0x7e) && n < 128 {
					n++
				}
				if n == len(p) {
					if flush {
						dropped++
						k.pending = nil
					}
					break
				}
				n++
			} else if p[1] == 'O' {
				if len(p) < 3 {
					if flush {
						dropped++
						k.pending = nil
					}
					break
				}
				n = 3
			}
			if n > len(p) {
				n = len(p)
			}
			seq := string(p[1:n])
			k.pending = p[n:]
			if seq == "[200~" {
				k.paste = true
				continue
			}
			if name, ok := escapeKeys[seq]; ok {
				out = append(out, Key{Name: name})
			} else {
				dropped++
			}
			continue
		}
		if p[0] < 32 {
			out = append(out, Key{Name: controls[p[0]]})
			k.pending = p[1:]
			continue
		}
		if p[0] == 127 {
			out = append(out, Key{Name: "BSpace"})
			k.pending = p[1:]
			continue
		}
		n := 0
		for n < len(p) && p[n] >= 32 && p[n] != 127 {
			if !utf8.FullRune(p[n:]) {
				break
			}
			r, size := utf8.DecodeRune(p[n:])
			if r == utf8.RuneError && size == 1 {
				break
			}
			n += size
		}
		if n > 0 {
			literal(p[:n])
			k.pending = p[n:]
			continue
		}
		if !flush && !utf8.FullRune(p) {
			break
		}
		dropped++
		k.pending = p[1:]
	}
	return
}

// TmuxLiteralArg escapes only tmux's argv-level command separator syntax.
// tmux removes this one escape before delivering the literal argument.
func TmuxLiteralArg(s string) string {
	if strings.HasSuffix(s, ";") {
		return s[:len(s)-1] + "\\;"
	}
	return s
}
