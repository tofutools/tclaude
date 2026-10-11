package render

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/paf"
)

// maxEmbeddedImage bounds one image embedded in an HTML document.
const maxEmbeddedImage = 8 << 20

// embeddableImages are raster types safe to embed as data URIs. SVG is left
// out because it can carry script.
var embeddableImages = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

// HTML writes the archive as one self-contained HTML document. Every value
// from the archive is escaped, the page loads nothing and runs no script,
// and its content security policy enforces both.
func HTML(w io.Writer, a *paf.Archive, source string, opts Options) error {
	s := Summarize(a, source)
	bw := bufio.NewWriter(w)
	e := html.EscapeString
	fmt.Fprintf(bw, `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src data:; style-src 'unsafe-inline'">
<title>%s</title>
<style>%s</style>
</head><body><main>
<h1>%s</h1>
<p class="note">%s</p>
<section><h2>Archive</h2>%s</section>
`, e(documentTitle(s)), htmlStyle, e(documentTitle(s)), e(provenanceNote(s)), factsHTML(archiveFacts(s)))
	for _, ag := range s.Agents {
		fmt.Fprintf(bw, "<section><h2>Agent: %s</h2>%s", e(ag.Name), factsHTML(agentFacts(ag)))
		for _, f := range definitionTexts(ag.Definition) {
			fmt.Fprintf(bw, "<h3>%s</h3><pre class=\"text\">%s</pre>", e(f[0]), e(f[1]))
		}
		if ag.LandingRung != "" || len(ag.Losses) > 0 {
			fmt.Fprintf(bw, "<h3>Losses</h3>")
			if ag.LandingRung != "" {
				fmt.Fprintf(bw, "<p>Landing rung when written: %s</p>", e(ag.LandingRung))
			}
			fmt.Fprintf(bw, "<ul>")
			for _, l := range ag.Losses {
				fmt.Fprintf(bw, "<li>%s</li>", e(lossLine(l)))
			}
			fmt.Fprintf(bw, "</ul>")
		}
		t, from, err := Transcript(a, ag.Key, opts)
		if err != nil {
			fmt.Fprintf(bw, "<h3>Conversation</h3><p class=\"note\">Not shown: %s.</p></section>\n", e(err.Error()))
			continue
		}
		fmt.Fprintf(bw, "<h3>Conversation</h3><p class=\"note\">From the %s, %d entries.</p>\n", e(from), len(t.Entries))
		for i, en := range t.Entries {
			writeEntryHTML(bw, i+1, en)
		}
		fmt.Fprintf(bw, "</section>\n")
	}
	fmt.Fprintf(bw, "</main></body></html>\n")
	return bw.Flush()
}

func factsHTML(facts [][2]string) string {
	var b strings.Builder
	b.WriteString("<dl>")
	for _, f := range facts {
		fmt.Fprintf(&b, "<dt>%s</dt><dd>%s</dd>", html.EscapeString(f[0]), html.EscapeString(f[1]))
	}
	b.WriteString("</dl>")
	return b.String()
}

func writeEntryHTML(w *bufio.Writer, n int, en transcript.Entry) {
	e := html.EscapeString
	heading := string(en.Role)
	if en.Origin == transcript.OriginTool {
		heading = "tool results"
	}
	if en.Author != "" {
		heading += " — " + en.Author
	}
	class := "entry " + e(string(en.Role))
	fmt.Fprintf(w, "<article class=\"%s\" id=\"entry-%d\"><h4>%d. %s", class, n, n, e(heading))
	if !en.Time.IsZero() {
		fmt.Fprintf(w, " <time>%s</time>", e(en.Time.UTC().Format(time.RFC3339)))
	}
	fmt.Fprintf(w, "</h4>")
	for _, b := range en.Blocks {
		switch b.Kind {
		case transcript.BlockText:
			fmt.Fprintf(w, "<pre class=\"text\">%s</pre>", e(b.Text))
		case transcript.BlockSummary:
			fmt.Fprintf(w, "<p class=\"label\">Compaction summary</p><pre class=\"text\">%s</pre>", e(b.Text))
		case transcript.BlockThinking:
			if strings.TrimSpace(b.Text) != "" {
				fmt.Fprintf(w, "<details><summary>thinking</summary><pre class=\"text\">%s</pre></details>", e(b.Text))
			}
		case transcript.BlockToolCall:
			input := b.Text
			if !b.Freeform {
				input = indentJSON(b.Input)
			}
			fmt.Fprintf(w, "<details><summary>tool call <code>%s</code></summary><pre>%s</pre></details>", e(b.Name), e(input))
		case transcript.BlockToolResult:
			status := "ok"
			if b.IsError {
				status = "error"
			}
			fmt.Fprintf(w, "<details><summary>tool result (%s)</summary><pre>%s</pre></details>", status, e(b.Text))
		}
		for _, m := range b.Media {
			writeMediaHTML(w, m)
		}
	}
	fmt.Fprintf(w, "</article>\n")
}

func writeMediaHTML(w *bufio.Writer, m transcript.Media) {
	e := html.EscapeString
	label := m.MediaType
	if m.Name != "" {
		label = m.Name + ", " + m.MediaType
	}
	switch {
	case len(m.Data) > 0 && len(m.Data) <= maxEmbeddedImage && embeddableImages[m.MediaType]:
		fmt.Fprintf(w, "<figure><img alt=\"%s\" src=\"data:%s;base64,%s\"><figcaption>%s</figcaption></figure>",
			e(label), m.MediaType, base64.StdEncoding.EncodeToString(m.Data), e(label))
	case m.Path != "":
		fmt.Fprintf(w, "<p class=\"label\">attachment (%s): %s</p>", e(label), e(m.Path))
	case m.URL != "" && !strings.HasPrefix(m.URL, "data:"):
		// Shown as text, never linked: the page makes no requests.
		fmt.Fprintf(w, "<p class=\"label\">attachment (%s): %s</p>", e(label), e(m.URL))
	default:
		fmt.Fprintf(w, "<p class=\"label\">attachment (%s), %d bytes, not shown</p>", e(label), len(m.Data))
	}
}

func indentJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Indent(&b, raw, "", "  ") != nil {
		return string(raw)
	}
	return b.String()
}

const htmlStyle = `
:root{--bg:#fbfaf7;--fg:#1d1d1b;--muted:#6b6862;--line:#e3e0d8;--user:#eef3fb;--assistant:#ffffff;--code:#f3f1ec}
@media (prefers-color-scheme: dark){:root{--bg:#161615;--fg:#e8e6e1;--muted:#9a978f;--line:#33322f;--user:#1c2433;--assistant:#1e1e1c;--code:#232321}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.55 system-ui,-apple-system,"Segoe UI",sans-serif}
main{max-width:920px;margin:0 auto;padding:24px 16px 64px}
h1{font-size:1.6rem;margin:0 0 4px}h2{font-size:1.25rem;margin:32px 0 8px;border-bottom:1px solid var(--line);padding-bottom:4px}
h3{font-size:1.05rem;margin:20px 0 6px}h4{font-size:.95rem;margin:0 0 6px}
.note,.label,time{color:var(--muted);font-size:.9rem}
dl{display:grid;grid-template-columns:max-content 1fr;gap:2px 16px;margin:8px 0}dt{color:var(--muted)}dd{margin:0;overflow-wrap:anywhere}
pre{background:var(--code);padding:8px 10px;border-radius:6px;overflow-x:auto;font:13px/1.45 ui-monospace,SFMono-Regular,Menlo,monospace;margin:6px 0}
pre.text{white-space:pre-wrap;overflow-wrap:anywhere;background:none;padding:0;font:inherit}
.entry{border:1px solid var(--line);border-radius:8px;padding:10px 12px;margin:10px 0;background:var(--assistant)}
.entry.user{background:var(--user)}
details summary{cursor:pointer;color:var(--muted)}
img{max-width:100%;height:auto;border-radius:4px}figure{margin:8px 0}figcaption{color:var(--muted);font-size:.85rem}
`
