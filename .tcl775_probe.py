#!/usr/bin/env python3
import fcntl
import os
import signal
import struct
import sys
import termios
import time


def terminal_state(label):
    rows, cols, _, _ = struct.unpack("HHHH", fcntl.ioctl(0, termios.TIOCGWINSZ, b"\0" * 8))
    try:
        foreground_pgrp = os.tcgetpgrp(0)
    except OSError as exc:
        foreground_pgrp = f"error:{exc.errno}"
    print(
        f"{label} rows={rows} cols={cols} pid={os.getpid()} sid={os.getsid(0)} "
        f"pgrp={os.getpgrp()} tty_fg_pgrp={foreground_pgrp}",
        flush=True,
    )


def on_winch(_signum, _frame):
    terminal_state("SIGWINCH")


signal.signal(signal.SIGWINCH, on_winch)
terminal_state("START")
deadline = time.monotonic() + 4
last = None
while time.monotonic() < deadline:
    state = fcntl.ioctl(0, termios.TIOCGWINSZ, b"\0" * 8)
    if state != last:
        rows, cols, _, _ = struct.unpack("HHHH", state)
        print(f"POLL rows={rows} cols={cols}", flush=True)
        last = state
    time.sleep(0.05)
terminal_state("END")
sys.exit(0)
