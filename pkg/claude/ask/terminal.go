package ask

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// TerminalKey returns a stable identifier for "the terminal `tclaude ask` was
// invoked from", salted by the machine's boot id. Together with the cwd it
// keys the ask-thread map (db.ask_threads), so repeated questions from the
// same terminal+directory continue one conversation.
//
// The terminal id is taken from the first terminal-emulator env var that is
// set (the approach proven in github.com/GiGurra/ai), falling back to the
// parent process id — i.e. the shell that ran `tclaude ask`, which is stable
// for the life of that shell. The boot id salt means a recycled pid after a
// reboot can't be mistaken for the same terminal.
func TerminalKey() string {
	return terminalID() + "." + bootID()
}

// terminalID identifies the calling terminal. Terminal emulators expose a
// per-window/-tab session id in the environment; we prefer those (they're
// stable across the shell's child processes and survive `exec`). TCLAUDE_ASK_TERM
// is an explicit override (handy for scripting and for tests). The final
// fallback is the parent pid — the shell process — which is stable for that
// shell's lifetime.
func terminalID() string {
	for _, k := range []string{
		"TCLAUDE_ASK_TERM", // explicit override / tests
		"WT_SESSION",       // Windows Terminal
		"TERM_SESSION_ID",  // macOS Terminal.app / some others
		"ITERM_SESSION_ID", // iTerm2
		"TMUX_PANE",        // inside tmux: per-pane id
	} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return "ppid-" + strconv.Itoa(os.Getppid())
}

// bootID returns a per-boot constant so a pid reused after a reboot doesn't
// collide with a pre-reboot terminal id. Linux/WSL expose it directly;
// macOS derives one from the kernel boot time; anything else falls back to a
// fixed token (the terminal id alone then keys the thread, which is still
// correct within a single boot — only cross-reboot pid reuse is unguarded).
func bootID() string {
	if b, err := os.ReadFile("/proc/sys/kernel/random/boot_id"); err == nil {
		return strings.TrimSpace(string(b))
	}
	if out, err := exec.Command("sysctl", "-n", "kern.boottime").Output(); err == nil {
		sum := sha256.Sum256([]byte(strings.TrimSpace(string(out))))
		return hex.EncodeToString(sum[:8])
	}
	return "noboot"
}
