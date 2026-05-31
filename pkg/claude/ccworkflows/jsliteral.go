package ccworkflows

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// jsLexer is a tolerant recursive-descent parser for the subset of JavaScript
// value syntax that a Workflow script's `meta` literal is guaranteed to use.
//
// The Workflow tool contract requires `meta` to be a PURE literal — no
// variables, function calls, spreads, or template interpolation — so a static
// parse is well-defined and we never need (nor want) a JS engine. The lexer
// understands: objects with bare-identifier or quoted keys, arrays, single-,
// double- and backtick-quoted strings (with the usual escapes; backticks are
// treated as plain strings since interpolation is disallowed), numbers,
// booleans, null, trailing commas, and `//` / `/* */` comments.
//
// It is deliberately lenient: anything it does not recognise as a value is an
// error, but it tolerates the cosmetic noise (comments, trailing commas) that
// real saved scripts contain.
type jsLexer struct {
	s string
	i int
}

// parseJSValue parses a single JS literal value from src starting at offset.
// It returns the decoded Go value (map[string]any, []any, string, float64,
// bool, or nil) and the offset just past the consumed value.
func parseJSValue(src string, offset int) (any, int, error) {
	l := &jsLexer{s: src, i: offset}
	v, err := l.value()
	if err != nil {
		return nil, l.i, err
	}
	return v, l.i, nil
}

func (l *jsLexer) value() (any, error) {
	l.skipTrivia()
	if l.i >= len(l.s) {
		return nil, fmt.Errorf("unexpected end of input")
	}
	switch c := l.s[l.i]; {
	case c == '{':
		return l.object()
	case c == '[':
		return l.array()
	case c == '\'' || c == '"' || c == '`':
		return l.string()
	case c == '-' || c == '+' || c == '.' || (c >= '0' && c <= '9'):
		return l.number()
	default:
		return l.keyword()
	}
}

func (l *jsLexer) object() (any, error) {
	l.i++ // consume '{'
	obj := map[string]any{}
	for {
		l.skipTrivia()
		if l.i >= len(l.s) {
			return nil, fmt.Errorf("unterminated object")
		}
		if l.s[l.i] == '}' {
			l.i++
			return obj, nil
		}
		key, err := l.key()
		if err != nil {
			return nil, err
		}
		l.skipTrivia()
		if l.i >= len(l.s) || l.s[l.i] != ':' {
			return nil, fmt.Errorf("expected ':' after key %q", key)
		}
		l.i++ // consume ':'
		val, err := l.value()
		if err != nil {
			return nil, err
		}
		obj[key] = val
		l.skipTrivia()
		if l.i < len(l.s) && l.s[l.i] == ',' {
			l.i++ // tolerate (and require for multi-entry) trailing comma
		}
	}
}

func (l *jsLexer) array() (any, error) {
	l.i++ // consume '['
	arr := []any{}
	for {
		l.skipTrivia()
		if l.i >= len(l.s) {
			return nil, fmt.Errorf("unterminated array")
		}
		if l.s[l.i] == ']' {
			l.i++
			return arr, nil
		}
		val, err := l.value()
		if err != nil {
			return nil, err
		}
		arr = append(arr, val)
		l.skipTrivia()
		if l.i < len(l.s) && l.s[l.i] == ',' {
			l.i++
		}
	}
}

// key parses an object key: a quoted string or a bare JS identifier.
func (l *jsLexer) key() (string, error) {
	l.skipTrivia()
	if l.i >= len(l.s) {
		return "", fmt.Errorf("expected object key")
	}
	if c := l.s[l.i]; c == '\'' || c == '"' || c == '`' {
		v, err := l.string()
		if err != nil {
			return "", err
		}
		return v.(string), nil
	}
	start := l.i
	for l.i < len(l.s) && isIdentChar(l.s[l.i]) {
		l.i++
	}
	if l.i == start {
		return "", fmt.Errorf("expected object key at offset %d", l.i)
	}
	return l.s[start:l.i], nil
}

func (l *jsLexer) string() (any, error) {
	quote := l.s[l.i]
	l.i++ // consume opening quote
	var b strings.Builder
	for l.i < len(l.s) {
		c := l.s[l.i]
		switch c {
		case quote:
			l.i++
			return b.String(), nil
		case '\\':
			l.i++
			if l.i >= len(l.s) {
				return nil, fmt.Errorf("unterminated escape in string")
			}
			esc := l.s[l.i]
			switch esc {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case '/':
				b.WriteByte('/')
			case '\\':
				b.WriteByte('\\')
			case '\'':
				b.WriteByte('\'')
			case '"':
				b.WriteByte('"')
			case '`':
				b.WriteByte('`')
			case 'u':
				r, n, err := decodeUnicodeEscape(l.s[l.i+1:])
				if err != nil {
					return nil, err
				}
				// Combine a UTF-16 surrogate pair (e.g. 😀 → 😀): a
				// lone high surrogate written via WriteRune would become U+FFFD.
				if utf16.IsSurrogate(r) {
					if rest := l.s[l.i+1+n:]; strings.HasPrefix(rest, "\\u") {
						if r2, n2, err2 := decodeUnicodeEscape(rest[2:]); err2 == nil {
							if combined := utf16.DecodeRune(r, r2); combined != utf8.RuneError {
								r = combined
								n += 2 + n2
							}
						}
					}
				}
				if !utf8.ValidRune(r) {
					r = utf8.RuneError // out-of-range \u{...} or unpaired surrogate
				}
				b.WriteRune(r)
				l.i += n
			default:
				// Unknown escape: keep the character verbatim (lenient).
				b.WriteByte(esc)
			}
			l.i++
		default:
			// Copy raw bytes (handles multibyte UTF-8 transparently).
			b.WriteByte(c)
			l.i++
		}
	}
	return nil, fmt.Errorf("unterminated string")
}

// decodeUnicodeEscape decodes the bytes following a `\u` escape. It supports
// both the 4-hex-digit form (\uXXXX) and the brace form (\u{XXXXX}). It returns
// the rune and the number of source bytes consumed after the `u`.
func decodeUnicodeEscape(s string) (rune, int, error) {
	if strings.HasPrefix(s, "{") {
		end := strings.IndexByte(s, '}')
		if end < 0 {
			return 0, 0, fmt.Errorf("unterminated \\u{...} escape")
		}
		n, err := strconv.ParseInt(s[1:end], 16, 32)
		if err != nil {
			return 0, 0, fmt.Errorf("bad \\u{...} escape: %w", err)
		}
		return rune(n), end + 1, nil
	}
	if len(s) < 4 {
		return 0, 0, fmt.Errorf("truncated \\u escape")
	}
	n, err := strconv.ParseInt(s[:4], 16, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("bad \\u escape: %w", err)
	}
	return rune(n), 4, nil
}

func (l *jsLexer) number() (any, error) {
	start := l.i
	for l.i < len(l.s) {
		c := l.s[l.i]
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' ||
			c == 'e' || c == 'E' || c == 'x' || c == 'X' ||
			(c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			l.i++
			continue
		}
		break
	}
	tok := l.s[start:l.i]
	if v, err := strconv.ParseFloat(tok, 64); err == nil {
		return v, nil
	}
	if v, err := strconv.ParseInt(tok, 0, 64); err == nil {
		return float64(v), nil
	}
	return nil, fmt.Errorf("invalid number literal %q", tok)
}

// keyword parses true/false/null/undefined, requiring an identifier boundary
// so e.g. `nullable` is not mis-read as `null` followed by `able`.
func (l *jsLexer) keyword() (any, error) {
	kws := []struct {
		word string
		val  any
	}{
		{"true", true}, {"false", false}, {"null", nil}, {"undefined", nil},
	}
	for _, kw := range kws {
		if !strings.HasPrefix(l.s[l.i:], kw.word) {
			continue
		}
		end := l.i + len(kw.word)
		if end < len(l.s) && isIdentChar(l.s[end]) {
			continue // longer identifier, not this keyword
		}
		l.i = end
		return kw.val, nil
	}
	return nil, fmt.Errorf("unexpected token at offset %d: %.10q", l.i, l.s[l.i:])
}

// skipTrivia advances past whitespace and `//` / `/* */` comments.
func (l *jsLexer) skipTrivia() {
	for l.i < len(l.s) {
		c := l.s[l.i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			l.i++
		case c == '/' && l.i+1 < len(l.s) && l.s[l.i+1] == '/':
			l.i += 2
			for l.i < len(l.s) && l.s[l.i] != '\n' {
				l.i++
			}
		case c == '/' && l.i+1 < len(l.s) && l.s[l.i+1] == '*':
			l.i += 2
			for l.i+1 < len(l.s) && (l.s[l.i] != '*' || l.s[l.i+1] != '/') {
				l.i++
			}
			l.i += 2 // consume closing */ (clamped below)
			if l.i > len(l.s) {
				l.i = len(l.s)
			}
		default:
			return
		}
	}
}

func isIdentChar(c byte) bool {
	return c == '_' || c == '$' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// asString coerces a parsed JS value to a string, returning ok=false otherwise.
func asString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}
