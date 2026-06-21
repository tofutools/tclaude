package ask

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
// The terminal id is resolved by resolveTerminalID (see there for the full
// chain): an explicit override, then the calling emulator's own per-tab session
// id, then the controlling tty, then the parent shell pid. The boot-id salt
// means a recycled pid/pts after a reboot can't be mistaken for the same
// terminal.
func TerminalKey() string {
	id, _ := resolveTerminalID()
	return id + "." + bootID()
}

// termSource is one way to identify the calling terminal. Vars lists the env
// var(s) that carry the id; the value is their contents joined with "/", and is
// used only when EVERY listed var is non-empty — so a partial multi-var id
// (e.g. Konsole's SERVICE without its SESSION) is skipped rather than forming
// an unstable half-key. Label names the source, surfaced by `tclaude ask
// --where` so the resolved bucket is observable on each emulator.
type termSource struct {
	Label string
	Vars  []string
}

// perTabSources are per-tab/pane session ids — the gold standard, as granular
// as the existing TMUX_PANE choice (one ask thread per tab). The first four are
// the original cross-platform set (Windows Terminal, macOS Terminal.app/iTerm2,
// tmux), kept in their original order so existing threads keep their key; the
// rest are the Linux desktop emulators added for JOH-251. In practice exactly
// one of these is set (you are inside one emulator), so the order rarely bites —
// the exception is a multiplexer nested in an emulator (tmux inside Windows
// Terminal), where the outer WT_SESSION matches first and all the tab's tmux
// panes share one bucket; cwd usually re-separates them, and a deliberate
// "innermost pane wins" reorder of the cross-platform four is left as its own
// decision rather than smuggled in here.
//
// NOTE (JOH-251 step 1): the Linux env var names below are from documentation,
// not a live capture on each emulator — verify on a real GNOME/KDE/kitty/…
// session (`tclaude ask --where`) before treating coverage as confirmed.
var perTabSources = []termSource{
	{"windows-terminal", []string{"WT_SESSION"}},          // Windows Terminal
	{"macos-terminal", []string{"TERM_SESSION_ID"}},       // macOS Terminal.app / some others
	{"iterm2", []string{"ITERM_SESSION_ID"}},              // iTerm2
	{"tmux", []string{"TMUX_PANE"}},                       // tmux: per-pane id
	{"gnome-terminal", []string{"GNOME_TERMINAL_SCREEN"}}, // GNOME Terminal (VTE): per-tab D-Bus path, Wayland-safe
	// KDE Konsole: needs the SERVICE+SESSION pair — KONSOLE_DBUS_SESSION alone
	// ("/Sessions/1") resets per process, so it's unstable on its own.
	{"konsole", []string{"KONSOLE_DBUS_SERVICE", "KONSOLE_DBUS_SESSION"}},
	{"wezterm", []string{"WEZTERM_PANE"}},       // WezTerm: per-pane (clean, like TMUX_PANE)
	{"tilix", []string{"TILIX_ID"}},             // Tilix: per-terminal UUID
	{"terminator", []string{"TERMINATOR_UUID"}}, // Terminator: per-terminal urn:uuid:…
}

// perWindowSources are per-OS-window ids — coarser than per-tab (all tabs in
// one window share a single ask thread). They sit BELOW the controlling-tty
// fallback on purpose: the controlling tty gives per-tab granularity and is
// terminal-agnostic, so it is the better default here. These only get a look-in
// where the controlling tty can't be read (no /proc), and even then they're
// still better than the bare ppid (stable across wrapper scripts / subshells
// within the window).
var perWindowSources = []termSource{
	{"kitty", []string{"KITTY_WINDOW_ID"}},         // kitty: per OS window
	{"alacritty", []string{"ALACRITTY_WINDOW_ID"}}, // Alacritty: per window (newer versions)
	{"x11-window", []string{"WINDOWID"}},           // xterm/urxvt: per window (X11 only, not Wayland)
}

// resolveTerminalID identifies the calling terminal and the source that won,
// in descending order of fidelity:
//
//  1. TCLAUDE_ASK_TERM — explicit override (scripting / tests).
//  2. perTabSources    — the emulator's own per-tab/pane session id.
//  3. controlling tty  — the terminal LINE (/dev/pts/N). Terminal-agnostic and,
//     unlike the env vars, present on a bare native Linux terminal; unlike the
//     ppid fallback it survives wrapper scripts and fresh subshells (same line,
//     same id) and tolerates fd redirection (pipes), so it's the universal
//     fallback. Linux/WSL only.
//  4. perWindowSources — coarser per-window ids, below the tty fallback.
//  5. ppid             — the parent shell's pid, the last resort.
func resolveTerminalID() (id, source string) {
	if v := strings.TrimSpace(os.Getenv("TCLAUDE_ASK_TERM")); v != "" {
		return v, "override (TCLAUDE_ASK_TERM)"
	}
	if id, src := firstEnvSource(perTabSources); id != "" {
		return id, src
	}
	if v := controllingTTYFn(); v != "" {
		return v, "controlling-tty"
	}
	if id, src := firstEnvSource(perWindowSources); id != "" {
		return id, src
	}
	return "ppid-" + strconv.Itoa(os.Getppid()), "ppid"
}

// firstEnvSource returns the id + label of the first source whose env vars are
// all set, or ("", "") if none match. A source's id is its vars' values joined
// with "/" (for single-var sources that's just the raw value, preserving the
// original keys for WT_SESSION/TERM_SESSION_ID/ITERM_SESSION_ID/TMUX_PANE).
func firstEnvSource(sources []termSource) (id, label string) {
	for _, s := range sources {
		parts := make([]string, 0, len(s.Vars))
		complete := true
		for _, k := range s.Vars {
			v := strings.TrimSpace(os.Getenv(k))
			if v == "" {
				complete = false
				break
			}
			parts = append(parts, v)
		}
		if complete {
			return strings.Join(parts, "/"), s.Label
		}
	}
	return "", ""
}

// controllingTTYFn is the controlling-terminal probe, swappable in tests so the
// resolveTerminalID chain can be exercised without depending on the test
// runner's real (or absent) tty.
var controllingTTYFn = controllingTTY

// controllingTTY returns a stable id for the process's controlling terminal
// line (e.g. "pts/3"), or "" if there is none. It reads the controlling tty
// from /proc/self/stat (the tty_nr field), so — unlike inspecting stdin — it
// survives `git diff | tclaude ask` and `x=$(tclaude ask …)` (fd redirection
// doesn't change the session's controlling terminal), and — unlike the ppid
// fallback — it names the terminal LINE not the shell PROCESS, so it stays
// constant across wrapper scripts and fresh subshells. Linux/WSL only; returns
// "" elsewhere (no /proc), where the env vars and ppid still apply.
func controllingTTY() string {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return ""
	}
	// The comm (2nd) field is wrapped in parens and may itself contain spaces
	// and ')', so split on the LAST ')': everything after it is space-separated
	// numeric fields starting with state. tty_nr is the 5th of those
	// (state, ppid, pgrp, session, tty_nr → index 4).
	s := string(b)
	rparen := strings.LastIndexByte(s, ')')
	if rparen < 0 {
		return ""
	}
	fields := strings.Fields(s[rparen+1:])
	if len(fields) < 5 {
		return ""
	}
	ttyNr, err := strconv.Atoi(fields[4])
	if err != nil {
		return ""
	}
	return ttyName(ttyNr)
}

// ttyName renders a Linux tty_nr (the packed dev_t from /proc/.../stat) as a
// stable id. The packing is the kernel's new_encode_dev: the major is bits
// 19–8; the minor is bits 31–20 (high) plus 7–0 (low). (`man 5 proc` describes
// the major as bits 15–8 — the common case; we mask the full 12 bits to match
// the kernel exactly, though no real tty major exceeds 255.) tty_nr 0 means "no
// controlling terminal" → "". UNIX98 pts slaves (major 136–143) render as
// "pts/N" for readability; anything else (real serial/console lines) as
// "ttyMAJOR-MINOR". Either way the string is unique per terminal line, which is
// all the key needs.
func ttyName(ttyNr int) string {
	if ttyNr == 0 {
		return ""
	}
	major := (ttyNr >> 8) & 0xfff
	minor := (ttyNr & 0xff) | (((ttyNr >> 20) & 0xfff) << 8)
	if major >= 136 && major <= 143 {
		return "pts/" + strconv.Itoa((major-136)*256+minor)
	}
	return fmt.Sprintf("tty%d-%d", major, minor)
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
