package tmuxcmd

import (
	"fmt"
	"strconv"
	"strings"
)

// PaneFromSpec turns a pane target written as text into a PaneTarget that
// resolves exactly. A pane id ("%12") is already exact and passes through;
// anything else is a session-shaped target ("name:0.0") and gets its session
// name pinned, so it cannot fall back onto a namesake session. A leading '='
// is accepted and means the same.
//
// It is for targets that arrive as text (a stored target, a lock identity)
// and name a pane by id or by session name. A session id ("$3") is not
// understood here. New code should build a target from a session name or
// pane id instead.
func PaneFromSpec(spec string) PaneTarget {
	spec = strings.TrimPrefix(spec, "=")
	if spec == "" || strings.HasPrefix(spec, "%") {
		return PaneTarget{spec: spec}
	}
	return PaneTarget{spec: "=" + spec}
}

// SendKeys presses keys in the pane, by tmux key name ("Enter", "C-c",
// "Escape"). A name tmux does not know is typed as text, so text the caller
// does not control must go through SendLiteral instead.
func (c Client) SendKeys(p PaneTarget, keys ...string) error {
	if err := named("send-keys", p.spec); err != nil {
		return err
	}
	_, err := c.run(join("send-keys", "-t", p.spec, keys)...)
	return err
}

// SendLiteral types text into the pane exactly as written: nothing in it is
// read as a key name, so "Enter" types the word, and nothing in it is read as
// tmux syntax, so text may start with '-' or end with ';'.
func (c Client) SendLiteral(p PaneTarget, text string) error {
	if err := named("send-keys", p.spec); err != nil {
		return err
	}
	_, err := c.run("send-keys", "-l", "-t", p.spec, "--", literalArg(text))
	return err
}

// SendBytes writes raw bytes to the pane's process, bypassing tmux's key
// table. It is how an already-encoded terminal sequence (a mouse report)
// reaches the application unchanged. No bytes is a no-op.
func (c Client) SendBytes(p PaneTarget, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if err := named("send-keys", p.spec); err != nil {
		return err
	}
	args := make([]string, 0, 4+len(data))
	args = append(args, "send-keys", "-t", p.spec, "-H")
	for _, b := range data {
		args = append(args, fmt.Sprintf("%02x", b))
	}
	_, err := c.run(args...)
	return err
}

// Key is one keystroke for SendKeySequence: either a tmux key name or literal
// text, never both.
type Key struct {
	// Name is a tmux key name. It must come from a fixed table, never from
	// input a user controls.
	Name string
	// Literal is text to type as written, used when Name is empty.
	Literal string
}

// SendKeySequence presses a run of keys in order with a single tmux command,
// so a paste or a fast typist costs one round trip rather than one per key.
// Literal text is escaped so it can never end the command and start another.
// tmux refuses a command over about 16KB; the caller bounds the run.
func (c Client) SendKeySequence(p PaneTarget, keys []Key) error {
	if len(keys) == 0 {
		return nil
	}
	if err := named("send-keys", p.spec); err != nil {
		return err
	}
	var args []string
	for i, key := range keys {
		if i > 0 {
			args = append(args, ";")
		}
		args = append(args, "send-keys", "-t", p.spec)
		if key.Name != "" {
			args = append(args, key.Name)
		} else {
			args = append(args, "-l", "--", literalArg(key.Literal))
		}
	}
	_, err := c.run(args...)
	return err
}

// literalArg escapes the one thing tmux's argv parser treats specially in an
// argument: a trailing ';' ends the command, and tmux drops it. A backslash
// before it keeps it part of the text. The other half of passing text
// verbatim is the "--" its callers put in front: without it tmux reads text
// that starts with '-' as flags and rejects the command.
func literalArg(s string) string {
	if strings.HasSuffix(s, ";") {
		return s[:len(s)-1] + `\;`
	}
	return s
}

// EnterCopyMode puts the pane into tmux's copy mode, where its scrollback can
// be scrolled. The mode ends by itself when scrolled back to the bottom.
func (c Client) EnterCopyMode(p PaneTarget) error {
	if err := named("copy-mode", p.spec); err != nil {
		return err
	}
	_, err := c.run("copy-mode", "-e", "-t", p.spec)
	return err
}

// CopyModeCommand runs a copy-mode command ("scroll-up", "cancel") in the
// pane, repeat times. tmux rejects it when the pane is not in a mode.
func (c Client) CopyModeCommand(p PaneTarget, command string, repeat int) error {
	if err := named("send-keys", p.spec); err != nil {
		return err
	}
	args := []string{"send-keys", "-t", p.spec, "-X"}
	if repeat > 1 {
		args = append(args, "-N", strconv.Itoa(repeat))
	}
	_, err := c.run(append(args, command)...)
	return err
}

// PasteText delivers text to the pane as one bracketed paste, so an
// application that supports it receives the whole text, newlines and tabs
// included, as a single input event rather than as keystrokes.
//
// It stages the text in the named tmux buffer and pastes it; the buffer is
// removed either way. beforePaste, when not nil, runs between the two steps
// and aborts the paste by returning an error: it is where a caller re-checks
// that the pane may still be typed into.
func (c Client) PasteText(p PaneTarget, buffer, text string, beforePaste func() error) error {
	if err := named("paste-buffer", p.spec, buffer); err != nil {
		return err
	}
	if _, err := c.run("set-buffer", "-b", buffer, "--", literalArg(text)); err != nil {
		return fmt.Errorf("set-buffer text: %w", err)
	}
	err := error(nil)
	if beforePaste != nil {
		err = beforePaste()
	}
	if err == nil {
		// -d drops the buffer, -p brackets the paste, -r keeps newlines as
		// they are instead of turning them into carriage returns.
		_, err = c.run("paste-buffer", "-d", "-p", "-r", "-b", buffer, "-t", p.spec)
	}
	if err != nil {
		_, _ = c.run("delete-buffer", "-b", buffer)
		return fmt.Errorf("paste-buffer text: %w", err)
	}
	return nil
}

// CaptureOptions selects what CapturePane returns beyond the visible screen.
type CaptureOptions struct {
	// Escapes keeps the escape sequences for colour and attributes, and the
	// targets of hyperlinks, instead of plain text.
	Escapes bool
	// History includes this many lines of scrollback above the visible
	// screen. 0 captures the visible screen only.
	History int
	// Wrapped keeps a line the terminal soft-wrapped as the separate rows it
	// is displayed as. By default wrapped rows are joined back into the line
	// the application printed.
	Wrapped bool
}

// CapturePane returns the pane's screen as text. It also works on a retained
// dead pane, which is how a failed launch's last output is read.
func (c Client) CapturePane(p PaneTarget, opts CaptureOptions) (string, error) {
	if err := named("capture-pane", p.spec); err != nil {
		return "", err
	}
	args := []string{"capture-pane", "-p"}
	if opts.Escapes {
		args = append(args, "-e")
	}
	if !opts.Wrapped {
		args = append(args, "-J")
	}
	if opts.History > 0 {
		args = append(args, "-S", strconv.Itoa(-opts.History))
	}
	out, err := c.run(append(args, "-t", p.spec)...)
	return string(out), err
}

// ResizeWindow sets the window's size and makes it manual: tmux stops
// deriving it from the attached clients.
func (c Client) ResizeWindow(w WindowTarget, cols, rows int) error {
	if err := named("resize-window", w.spec); err != nil {
		return err
	}
	_, err := c.run("resize-window", "-t", w.spec, "-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows))
	return err
}

// PinWindowSize makes the window's current size manual in one step, so it
// stays as it is when the clients that determined it go away.
func (c Client) PinWindowSize(w WindowTarget) error {
	if err := named("resize-window", w.spec); err != nil {
		return err
	}
	_, err := c.run("resize-window", "-t", w.spec)
	return err
}

// BindRootKey binds a key in tmux's root table, so it acts without the prefix
// key, to a tmux command given as its words.
func (c Client) BindRootKey(key string, command ...string) error {
	_, err := c.run(join("bind-key", "-n", key, command)...)
	return err
}
