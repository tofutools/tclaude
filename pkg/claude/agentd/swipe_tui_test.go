package agentd

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"golang.org/x/term"
)

// swipeTUIEnv makes this test binary a stand-in for Claude Code's fullscreen
// view instead of running tests (TestMain checks it first): the view a phone
// swipe is measured over (TestMeasureTerminalSwipe, the sandbox's
// TCLAUDE_SANDBOX_SWIPE_TUI). Like Claude Code it takes the alternate screen
// and SGR mouse reporting, keeps its own transcript, and scrolls it on each
// wheel notch, redrawing every row in colour, the frame bracketed in
// synchronized output (DEC 2026); a notch past either end draws nothing.
const swipeTUIEnv = "TCLAUDE_SWIPE_TUI_CHILD"

// SwipeTUIEnvForTest is swipeTUIEnv, for TestMain.
const SwipeTUIEnvForTest = swipeTUIEnv

// swipeTUICommand is the shell command that runs this binary as the view.
func swipeTUICommand() string {
	return swipeTUIEnv + "=1 exec " + shellSingleQuote(os.Args[0])
}

// SwipeTUICommandForTest is swipeTUICommand, for the sandbox.
func SwipeTUICommandForTest() string { return swipeTUICommand() }

var (
	swipeWords = strings.Fields("Read Edit Bash Grep the dashboard terminal swipe frame socket tmux " +
		"pane render notch scroll phone cellular compress coalesce latency")
	swipeKinds = []string{"\x1b[38;5;114m⏺\x1b[0m ", "  \x1b[2m⎿\x1b[0m  ", "  ", "\x1b[1m>\x1b[0m "}
	swipeWheel = regexp.MustCompile(`\x1b\[<(\d+);\d+;\d+[Mm]`)
)

func swipeLine(i, width int) string {
	var words []string
	n, length := i*7+3, 0
	for length < width-14 {
		w := swipeWords[n%len(swipeWords)]
		words = append(words, w)
		length += len(w) + 1
		n = (n*31 + 17) % 9973
	}
	text := strings.Join(words, " ")
	if len(text) > width-14 {
		text = text[:max(0, width-14)]
	}
	return fmt.Sprintf("%s\x1b[38;5;%dm%s\x1b[0m %d", swipeKinds[i%len(swipeKinds)], 250-i%6, text, i)
}

// RunSwipeTUIForTest runs the view on this process's terminal until it reads
// q or Ctrl-C, or its input ends.
func RunSwipeTUIForTest() {
	in := int(os.Stdin.Fd())
	if saved, err := term.MakeRaw(in); err == nil {
		defer func() { _ = term.Restore(in, saved) }()
	}
	out := os.Stdout
	const total = 2000
	top := total
	_, _ = out.WriteString("\x1b[?1049h\x1b[?1000h\x1b[?1006h\x1b[?25l")
	defer func() { _, _ = out.WriteString("\x1b[?1000l\x1b[?1006l\x1b[?25h\x1b[?1049l") }()
	draw := func() {
		cols, rows, err := term.GetSize(in)
		if err != nil || cols <= 0 || rows <= 0 {
			cols, rows = 80, 24
		}
		first := max(0, min(top, total)-rows)
		var frame strings.Builder
		frame.WriteString("\x1b[?2026h")
		for r := range rows {
			fmt.Fprintf(&frame, "\x1b[%d;1H%s\x1b[K", r+1, swipeLine(first+r, cols))
		}
		frame.WriteString("\x1b[?2026l")
		_, _ = out.WriteString(frame.String())
	}
	draw()
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n == 0 || err != nil {
			return
		}
		data := buf[:n]
		if strings.ContainsAny(string(data), "q\x03") {
			return
		}
		was := top
		wheel := swipeWheel.FindAllSubmatch(data, -1)
		for _, m := range wheel {
			switch string(m[1]) {
			case "64":
				top = max(40, top-3)
			case "65":
				top = min(total, top+3)
			}
		}
		// Like Claude Code, a notch that scrolls nothing (past either end)
		// draws nothing.
		if len(wheel) > 0 && top == was {
			continue
		}
		draw()
	}
}
