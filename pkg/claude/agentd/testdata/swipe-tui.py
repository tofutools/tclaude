#!/usr/bin/env python3
# swipe-tui.py: a stand-in for Claude Code's fullscreen view, for measuring a
# phone swipe over a real tmux (dashboard_term_swipe_measure_test.go and the
# sandbox's TCLAUDE_SANDBOX_SWIPE_TUI). Like Claude Code it takes the
# alternate screen and SGR mouse reporting, keeps its own transcript and
# scrolls it on each wheel notch, redrawing every row in colour, the frame
# bracketed in synchronized output (DEC 2026).
import os, re, shutil, sys, termios, tty

WORDS = ('Read Edit Bash Grep the dashboard terminal swipe frame socket tmux '
         'pane render notch scroll phone cellular compress coalesce latency').split()
KINDS = ['\x1b[38;5;114m⏺\x1b[0m ', '  \x1b[2m⎿\x1b[0m  ', '  ', '\x1b[1m>\x1b[0m ']


def line(i, width):
    words, n = [], i * 7 + 3
    while sum(len(w) + 1 for w in words) < width - 8:
        words.append(WORDS[n % len(WORDS)])
        n = (n * 31 + 17) % 9973
    text = ' '.join(words)[: width - 8]
    return f'{KINDS[i % len(KINDS)]}\x1b[38;5;{250 - i % 6}m{text}\x1b[0m {i}'


def main():
    out = sys.stdout.buffer
    fd = sys.stdin.fileno()
    saved = termios.tcgetattr(fd)
    tty.setraw(fd)
    total, top = 2000, 2000
    try:
        out.write(b'\x1b[?1049h\x1b[?1000h\x1b[?1006h\x1b[?25l')

        def draw():
            cols, rows = shutil.get_terminal_size()
            first = max(0, min(top, total) - rows)
            frame = ['\x1b[?2026h']
            for r in range(rows):
                frame.append(f'\x1b[{r + 1};1H{line(first + r, cols)}\x1b[K')
            frame.append('\x1b[?2026l')
            out.write(''.join(frame).encode())
            out.flush()

        draw()
        buf = b''
        while True:
            data = os.read(fd, 4096)
            if not data or b'\x03' in data or b'q' in data:
                return
            buf += data
            for m in re.finditer(rb'\x1b\[<(\d+);\d+;\d+[Mm]', buf):
                if m.group(1) == b'64':
                    top = max(40, top - 3)
                elif m.group(1) == b'65':
                    top = min(total, top + 3)
            buf = buf[buf.rfind(b'\x1b'):] if buf.endswith(b'\x1b') else b''
            draw()
    finally:
        out.write(b'\x1b[?1000l\x1b[?1006l\x1b[?25h\x1b[?1049l')
        out.flush()
        termios.tcsetattr(fd, termios.TCSADRAIN, saved)


main()
