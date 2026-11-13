package tmuxcmd

import "strings"

// OptionScope says which tmux option table a SetOption, UnsetOption, SetHook
// or UnsetHook call addresses, and whose.
type OptionScope struct {
	set, unset []string
	// named is set by the constructors for a scope whose target is not
	// empty. The zero OptionScope is not a scope: sent as it is, tmux would
	// apply it to whatever session is current. See ErrInvalidTarget.
	named bool
}

// PaneScope addresses one pane's own options and hooks (-p). A pane-local
// value overrides what the pane would inherit from its window or the server.
func PaneScope(p PaneTarget) OptionScope {
	return OptionScope{set: []string{"-p", "-t", p.spec}, unset: []string{"-p", "-u", "-t", p.spec}, named: p.spec != ""}
}

// WindowScope addresses one window's options (-w).
func WindowScope(w WindowTarget) OptionScope {
	return OptionScope{set: []string{"-w", "-t", w.spec}, unset: []string{"-wu", "-t", w.spec}, named: w.spec != ""}
}

// SessionScope addresses a session, leaving it to tmux to pick the table the
// option belongs to: a session option (mouse, set-titles) is set on the
// session, a window option (allow-passthrough) on its current window.
func SessionScope(s SessionTarget) OptionScope {
	t := s.ActivePane().spec
	return OptionScope{set: []string{"-t", t}, unset: []string{"-u", "-t", t}, named: t != ""}
}

// GlobalScope addresses the server-wide defaults (-g).
func GlobalScope() OptionScope {
	return OptionScope{set: []string{"-g"}, unset: []string{"-gu"}, named: true}
}

// SetOption sets a tmux option in the scope.
func (c Client) SetOption(scope OptionScope, name, value string) error {
	if !scope.named {
		return named("set-option", "")
	}
	_, err := c.run(join("set-option", scope.set, name, value)...)
	return err
}

// UnsetOption removes the scope's own value for an option, so the scope
// inherits it again.
func (c Client) UnsetOption(scope OptionScope, name string) error {
	if !scope.named {
		return named("set-option", "")
	}
	_, err := c.run(join("set-option", scope.unset, name)...)
	return err
}

// ShowWindowOption reads the window's own value for an option. set is false
// when the window does not set the option itself (it then inherits one, which
// this does not report).
func (c Client) ShowWindowOption(w WindowTarget, name string) (value string, set bool, err error) {
	// Without -v tmux prints "name value" only when the window sets the
	// option, which is the one way to tell "unset" from "set to empty".
	if err := named("show-options", w.spec); err != nil {
		return "", false, err
	}
	out, err := c.run("show-options", "-w", "-q", "-t", w.spec, name)
	if err != nil {
		return "", false, err
	}
	if strings.TrimSuffix(string(out), "\n") == "" {
		return "", false, nil
	}
	// -v returns the exact value, without tmux's display quoting.
	val, err := c.run("show-options", "-w", "-qv", "-t", w.spec, name)
	return strings.TrimSuffix(string(val), "\n"), true, err
}

// SetHook makes tmux run a tmux command when the named hook fires in the
// scope. command is tmux command text, not a shell command.
func (c Client) SetHook(scope OptionScope, hook, command string) error {
	if !scope.named {
		return named("set-hook", "")
	}
	_, err := c.run(join("set-hook", scope.set, hook, command)...)
	return err
}

// UnsetHook removes the scope's own handler for a hook.
func (c Client) UnsetHook(scope OptionScope, hook string) error {
	if !scope.named {
		return named("set-hook", "")
	}
	_, err := c.run(join("set-hook", scope.unset, hook)...)
	return err
}

// join builds an argv from a command word and argument groups, each group a
// string or a []string.
func join(command string, groups ...any) []string {
	args := []string{command}
	for _, g := range groups {
		switch v := g.(type) {
		case string:
			args = append(args, v)
		case []string:
			args = append(args, v...)
		}
	}
	return args
}
