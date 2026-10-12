package claude

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// TestLearnDocsCommandsResolve keeps the Learn tutorials honest: every
// `tclaude …` command in a fenced bash block under docs/learn/ must name a
// real subcommand and only real flags. Nothing is executed — each line is
// resolved against the live Cobra tree — so a renamed command or flag fails
// here instead of in a new user's terminal.
func TestLearnDocsCommandsResolve(t *testing.T) {
	pages, err := filepath.Glob(filepath.Join("..", "..", "docs", "learn", "*.md"))
	if err != nil || len(pages) == 0 {
		t.Fatalf("no Learn pages found: %v", err)
	}
	root := Cmd()
	checked := 0
	for _, page := range pages {
		for _, c := range learnDocCommands(t, page) {
			checked++
			if msg := checkLearnCommand(root, c.args); msg != "" {
				t.Errorf("%s:%d: %s\n    %s", filepath.Base(page), c.line, msg, c.text)
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no tclaude commands in docs/learn; is the parser broken?")
	}
}

type learnDocCommand struct {
	line int
	text string
	args []string // without the leading "tclaude"
}

// learnDocCommands extracts the tclaude invocations from a page's ```bash
// blocks, joining backslash continuations and splitting pipelines.
func learnDocCommands(t *testing.T, path string) []learnDocCommand {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	var out []learnDocCommand
	inBash := false
	pending, pendingLine := "", 0
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inBash = !inBash && (trimmed == "```bash" || trimmed == "```sh" || trimmed == "```shell")
			continue
		}
		if !inBash {
			continue
		}
		if pending == "" {
			pendingLine = n
		}
		if strings.HasSuffix(trimmed, "\\") {
			pending += strings.TrimSuffix(trimmed, "\\") + " "
			continue
		}
		full := pending + trimmed
		pending = ""
		for _, segment := range shellWords(full) {
			if len(segment) > 0 && segment[0] == "tclaude" {
				out = append(out, learnDocCommand{line: pendingLine, text: full, args: segment[1:]})
			}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// shellWords splits a command line into words, honoring quotes, stopping at
// an unquoted comment, and starting a new segment at each unquoted | ; &&.
func shellWords(s string) [][]string {
	var segments [][]string
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	endWord := func() {
		if inWord {
			words = append(words, cur.String())
			cur.Reset()
			inWord = false
		}
	}
	endSegment := func() {
		endWord()
		if len(words) > 0 {
			segments = append(segments, words)
		}
		words = nil
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '"' || r == '\'':
			quote, inWord = r, true
		case r == '#' && !inWord:
			endSegment()
			return segments
		case r == '|' || r == ';' || r == '&':
			endSegment()
		case r == ' ' || r == '\t':
			endWord()
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	endSegment()
	return segments
}

// checkLearnCommand resolves args against root and returns a problem, or "".
func checkLearnCommand(root *cobra.Command, args []string) string {
	cmd, rest, err := root.Find(args)
	if err != nil {
		return "unknown command: " + err.Error()
	}
	// root.Find leaves an unknown subcommand as a positional arg of its parent.
	// A parent with subcommands whose usage line declares no arguments (no
	// "<arg>" or "[arg]") does not take one, so that word is a typo.
	if cmd != root && cmd.HasSubCommands() && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") &&
		!strings.ContainsAny(cmd.Use, "<[") {
		return "unknown subcommand " + rest[0] + " of " + cmd.CommandPath()
	}
	for _, a := range rest {
		if a == "--" {
			break
		}
		switch {
		case strings.HasPrefix(a, "--"):
			name, _, _ := strings.Cut(strings.TrimPrefix(a, "--"), "=")
			if cmd.Flags().Lookup(name) == nil && cmd.InheritedFlags().Lookup(name) == nil {
				return "unknown flag --" + name + " for " + cmd.CommandPath()
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for _, short := range strings.TrimPrefix(a, "-") {
				if cmd.Flags().ShorthandLookup(string(short)) == nil && cmd.InheritedFlags().ShorthandLookup(string(short)) == nil {
					return "unknown flag -" + string(short) + " for " + cmd.CommandPath()
				}
			}
		}
	}
	return ""
}
