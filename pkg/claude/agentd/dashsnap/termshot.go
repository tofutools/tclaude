package dashsnap

import (
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
)

// TermPage renders ANSI terminal output (SGR colors and attributes) as a
// self-contained HTML page styled like a dark terminal window, for docs
// screenshots of tclaude's TUIs and CLI output. Pair it with Capture and
// ReadySelector ".term" to turn it into a PNG. title is shown in the window's
// title bar. Non-SGR escape sequences are dropped.
func TermPage(title, ansi string) string {
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8"><style>
html,body{margin:0;background:#1b1d23}
body{padding:18px;display:inline-block}
.win{background:#0f1115;border:1px solid #2c303a;border-radius:9px;box-shadow:0 8px 28px rgba(0,0,0,.45);overflow:hidden}
.bar{height:30px;background:#20232b;display:flex;align-items:center;padding:0 12px;gap:7px;color:#9aa0ab;font:12px system-ui,sans-serif}
.dot{width:11px;height:11px;border-radius:50%}
.title{margin-left:10px}
.term{margin:0;padding:12px 16px 16px;color:#d6d9df;font:13.5px/1.42 "DejaVu Sans Mono","JetBrains Mono",Menlo,monospace;white-space:pre}
.b{font-weight:bold}.dim{opacity:.6}.i{font-style:italic}.u{text-decoration:underline}
</style></head><body><div class="win"><div class="bar">
<span class="dot" style="background:#ff5f57"></span><span class="dot" style="background:#febc2e"></span><span class="dot" style="background:#28c840"></span>
<span class="title">`)
	b.WriteString(html.EscapeString(title))
	b.WriteString(`</span></div><pre class="term">`)
	b.WriteString(ansiToHTML(ansi))
	b.WriteString(`</pre></div></body></html>`)
	return b.String()
}

type sgrState struct {
	fg, bg                      string
	bold, dim, ital, ul, invert bool
}

func (s sgrState) plain() bool { return s == sgrState{} }

func (s sgrState) open() string {
	fg, bg := s.fg, s.bg
	if s.invert {
		fg, bg = bg, fg
		if fg == "" {
			fg = "#0f1115"
		}
		if bg == "" {
			bg = "#d6d9df"
		}
	}
	var cls []string
	if s.bold {
		cls = append(cls, "b")
	}
	if s.dim {
		cls = append(cls, "dim")
	}
	if s.ital {
		cls = append(cls, "i")
	}
	if s.ul {
		cls = append(cls, "u")
	}
	var style []string
	if fg != "" {
		style = append(style, "color:"+fg)
	}
	if bg != "" {
		style = append(style, "background:"+bg)
	}
	return fmt.Sprintf(`<span class="%s" style="%s">`, strings.Join(cls, " "), strings.Join(style, ";"))
}

func ansiToHTML(in string) string {
	var out strings.Builder
	var st sgrState
	spanOpen := false
	flush := func() {
		if spanOpen {
			out.WriteString("</span>")
			spanOpen = false
		}
		if !st.plain() {
			out.WriteString(st.open())
			spanOpen = true
		}
	}
	for i := 0; i < len(in); {
		c := in[i]
		if c == 0x1b && i+1 < len(in) && in[i+1] == '[' {
			j := i + 2
			for j < len(in) && (in[j] < 0x40 || in[j] > 0x7e) {
				j++
			}
			if j >= len(in) {
				break
			}
			if in[j] == 'm' {
				st = applySGR(st, in[i+2:j])
				flush()
			}
			i = j + 1
			continue
		}
		if c == 0x1b { // other escapes (OSC etc.): skip to BEL or ST
			j := i + 1
			for j < len(in) && in[j] != 0x07 && !(in[j] == '\\' && in[j-1] == 0x1b) {
				j++
			}
			i = j + 1
			continue
		}
		if c == '\r' {
			i++
			continue
		}
		// Copy one UTF-8 rune's bytes, escaping HTML.
		j := i + 1
		for j < len(in) && in[j]&0xc0 == 0x80 {
			j++
		}
		out.WriteString(html.EscapeString(in[i:j]))
		i = j
	}
	if spanOpen {
		out.WriteString("</span>")
	}
	return out.String()
}

func applySGR(st sgrState, params string) sgrState {
	if params == "" {
		return sgrState{}
	}
	ps := strings.FieldsFunc(params, func(r rune) bool { return r == ';' || r == ':' })
	n := func(k int) int {
		if k >= len(ps) {
			return 0
		}
		v, _ := strconv.Atoi(ps[k])
		return v
	}
	for k := 0; k < len(ps); k++ {
		switch p := n(k); {
		case p == 0:
			st = sgrState{}
		case p == 1:
			st.bold = true
		case p == 2:
			st.dim = true
		case p == 3:
			st.ital = true
		case p == 4:
			st.ul = true
		case p == 7:
			st.invert = true
		case p == 22:
			st.bold, st.dim = false, false
		case p == 23:
			st.ital = false
		case p == 24:
			st.ul = false
		case p == 27:
			st.invert = false
		case p >= 30 && p <= 37:
			st.fg = xterm256(p - 30)
		case p >= 90 && p <= 97:
			st.fg = xterm256(p - 90 + 8)
		case p == 39:
			st.fg = ""
		case p >= 40 && p <= 47:
			st.bg = xterm256(p - 40)
		case p >= 100 && p <= 107:
			st.bg = xterm256(p - 100 + 8)
		case p == 49:
			st.bg = ""
		case p == 38 || p == 48:
			var col string
			switch n(k + 1) {
			case 5:
				col = xterm256(n(k + 2))
				k += 2
			case 2:
				col = fmt.Sprintf("#%02x%02x%02x", n(k+2), n(k+3), n(k+4))
				k += 4
			}
			if p == 38 {
				st.fg = col
			} else {
				st.bg = col
			}
		}
	}
	return st
}

// xterm256 maps an xterm 256-color index to a hex color, with the 16 base
// colors tuned for a dark background.
func xterm256(i int) string {
	base := [16]string{"#3b3f4a", "#e06c75", "#98c379", "#e5c07b", "#61afef", "#c678dd", "#56b6c2", "#d6d9df",
		"#5c6370", "#ff7b86", "#b5e890", "#ffd580", "#82c4ff", "#de9cf0", "#7fd4df", "#ffffff"}
	switch {
	case i < 0:
		return ""
	case i < 16:
		return base[i]
	case i < 232:
		i -= 16
		lv := func(v int) int {
			if v == 0 {
				return 0
			}
			return 55 + v*40
		}
		return fmt.Sprintf("#%02x%02x%02x", lv(i/36), lv(i/6%6), lv(i%6))
	case i < 256:
		g := 8 + (i-232)*10
		return fmt.Sprintf("#%02x%02x%02x", g, g, g)
	}
	return ""
}

// CaptureTerm screenshots ansi as a terminal window (see TermPage) into
// outDir/<key>.png at the given viewport size and returns the file path.
// It returns ErrBrowserUnavailable when no Chrome is found.
func CaptureTerm(outDir, key, title, ansi string, width, height int) (string, error) {
	page := TermPage(title, ansi)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()
	shots, err := Capture(Config{
		BaseURL: srv.URL, OutDir: outDir, ReadySelector: ".term", LoadSettleMS: 100,
		States: []State{{Key: key, Title: title, Width: width, Height: height}},
	})
	if err != nil {
		return "", err
	}
	if len(shots) != 1 || shots[0].Err != "" {
		return "", fmt.Errorf("capture %s: %v", key, shots)
	}
	return filepath.Join(outDir, shots[0].File), nil
}
