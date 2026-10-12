package transcript

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// WriteMarkdown renders the complete transcript, unclipped and including any
// readable reasoning, as the file a morph keeps beside the target history.
// It is also what the context-file fallback points the target agent at.
func WriteMarkdown(t *Transcript, w io.Writer) error {
	bw := bufio.NewWriter(w)
	fmt.Fprintf(bw, "# Transcript carried over by tclaude morph\n\n")
	fmt.Fprintf(bw, "- Source harness: %s\n- Source conversation: %s\n", t.Source.Harness, t.Source.ConvID)
	if t.Source.Model != "" {
		fmt.Fprintf(bw, "- Model: %s\n", t.Source.Model)
	}
	if t.Source.Cwd != "" {
		fmt.Fprintf(bw, "- Working directory: %s\n", t.Source.Cwd)
	}
	if t.Source.Title != "" {
		fmt.Fprintf(bw, "- Title: %s\n", t.Source.Title)
	}
	fmt.Fprintf(bw, "\nThis is the effective conversation context at the time of the morph (active branch, from the last compaction on). Tool records are history.\n")
	writeMarkdownEntries(bw, t, 2)
	return bw.Flush()
}

// WriteMarkdownEntries renders only the transcript's entries, each under a
// heading of the given level with an entry-N anchor, for documents that
// write their own header.
func WriteMarkdownEntries(t *Transcript, w io.Writer, level int) error {
	bw := bufio.NewWriter(w)
	writeMarkdownEntries(bw, t, level)
	return bw.Flush()
}

func writeMarkdownEntries(bw *bufio.Writer, t *Transcript, level int) {
	hashes := strings.Repeat("#", max(level, 1))
	for i, e := range t.Entries {
		heading := e.Heading()
		fmt.Fprintf(bw, "\n<a id=\"entry-%d\"></a>\n\n%s %d. %s", i+1, hashes, i+1, heading)
		if !e.Time.IsZero() {
			fmt.Fprintf(bw, " (%s)", e.Time.UTC().Format("2006-01-02 15:04:05Z"))
		}
		_, _ = bw.WriteString("\n")
		for _, b := range e.Blocks {
			writeMarkdownBlock(bw, b)
		}
	}
}

func writeMarkdownBlock(w *bufio.Writer, b Block) {
	switch b.Kind {
	case BlockText:
		fmt.Fprintf(w, "\n%s\n", b.Text)
	case BlockSummary:
		fmt.Fprintf(w, "\n**Compaction summary:**\n\n%s\n", b.Text)
	case BlockThinking:
		if strings.TrimSpace(b.Text) != "" {
			fmt.Fprintf(w, "\n<details><summary>thinking</summary>\n\n%s\n\n</details>\n", b.Text)
		}
	case BlockToolCall:
		input := b.Text
		if !b.Freeform {
			input = compactJSON(b.Input)
		}
		fmt.Fprintf(w, "\n**Tool call `%s`** (%s)\n\n%s\n", b.Name, b.CallID, fence(input))
	case BlockToolResult:
		status := "ok"
		if b.IsError {
			status = "error"
		}
		fmt.Fprintf(w, "\n**Tool result** (%s, %s)\n\n%s\n", b.CallID, status, fence(b.Text))
		for _, m := range b.Media {
			fmt.Fprintf(w, "\n%s\n", describeMedia(m))
		}
	case BlockMedia:
		for _, m := range b.Media {
			fmt.Fprintf(w, "\n%s\n", describeMedia(m))
		}
	}
}

func describeMedia(m Media) string {
	label := m.MediaType
	if m.Name != "" {
		label = m.Name + ", " + m.MediaType
	}
	switch {
	case m.Path != "":
		return fmt.Sprintf("[attachment (%s): %s]", label, m.Path)
	case m.URL != "" && !strings.HasPrefix(m.URL, "data:"):
		return fmt.Sprintf("[attachment (%s): %s]", label, m.URL)
	default:
		return fmt.Sprintf("[inline attachment (%s), %d bytes]", label, len(m.Data))
	}
}

// fence wraps text in a code fence longer than any backtick run inside it.
func fence(s string) string {
	ticks := "```"
	for strings.Contains(s, ticks) {
		ticks += "`"
	}
	return ticks + "\n" + s + "\n" + ticks
}

// Briefing renders the plan as plain text startup context, for the
// context-file fallback when the target cannot install native history.
func (p *Plan) Briefing() string {
	var b strings.Builder
	for _, e := range p.Entries {
		fmt.Fprintf(&b, "\n--- %s ---\n", e.Role)
		for _, blk := range e.Blocks {
			switch blk.Kind {
			case BlockText:
				b.WriteString(blk.Text + "\n")
			case BlockMedia:
				for _, m := range blk.Media {
					b.WriteString(describeMedia(m) + "\n")
				}
			}
		}
	}
	return strings.TrimLeft(b.String(), "\n")
}
