package agentd

import (
	"log/slog"
	"os"
	"runtime"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/tmuxcmd"
	"github.com/tofutools/tclaude/pkg/launcher"
)

// localeDeps is what ensureUTF8Locale reaches outside the process; tests
// swap it.
type localeDeps struct {
	Environ []string
	Sources launcher.LocaleSources
	Setenv  func(string, string) error
}

// ensureUTF8Locale gives agentd a UTF-8 locale when its environment names
// none, so every child it starts (tmux, harnesses, git, hooks) runs as it
// would in a terminal. agentd started by launchd, systemd or a desktop
// launcher often has no locale at all. launcher.ChooseLocale picks the
// user's own (macOS AppleLocale, the system locale file) before a fallback;
// it is set as LANG when there is none, else as LC_CTYPE.
func ensureUTF8Locale(d localeDeps) {
	value, source := launcher.ChooseLocale(d.Environ, nil, d.Sources)
	if value == "" {
		if source == "" {
			slog.Info("agentd: no UTF-8 locale set, and none installed to set")
		}
		return
	}
	env := launcher.WithLocale(d.Environ, value)
	key, _, _ := strings.Cut(env[len(env)-1], "=")
	if err := d.Setenv(key, value); err != nil {
		slog.Info("agentd: could not set the locale", "key", key, "error", err)
		return
	}
	slog.Info("agentd: no UTF-8 locale in the environment; set one for agentd and everything it starts",
		"key", key, "value", value, "source", source)
}

func productionLocaleDeps() localeDeps {
	return localeDeps{
		Environ: os.Environ(),
		Sources: launcher.SystemLocaleSources(runtime.GOOS),
		Setenv:  os.Setenv,
	}
}

// syncTmuxServerLocale gives a running tclaude tmux server agentd's UTF-8
// locale when the server's global environment has none. Panes take their
// environment from the server, not from agentd, and the server outlives
// agentd: one an earlier, locale-less agentd started (or one an external
// unit owns) would otherwise start every new agent without a locale. No
// server running: nothing to do, the one agentd starts inherits its env.
func syncTmuxServerLocale(environ []string, tmux tmuxcmd.Client) {
	value, key := launcherEffectiveLocale(environ)
	if value == "" || !strings.Contains(strings.ToLower(strings.ReplaceAll(value, "-", "")), "utf8") {
		return
	}
	server, err := tmux.GlobalEnvironment()
	if err != nil {
		return // no server
	}
	if v, _ := launcherEffectiveLocale(server); v != "" {
		return // the server has a locale; it was chosen for it
	}
	if err := tmux.SetGlobalEnvironment(key, value); err != nil {
		slog.Info("agentd: could not give the tmux server a locale", "error", err)
		return
	}
	slog.Info("agentd: gave the running tmux server a UTF-8 locale for new agents", "key", key, "value", value)
}

// launcherEffectiveLocale returns the first set of LC_ALL, LC_CTYPE and
// LANG in environ, and its name (LANG when LC_ALL decides it, as LC_ALL
// is not one to plant in a server).
func launcherEffectiveLocale(environ []string) (value, key string) {
	get := func(k string) string {
		for i := len(environ) - 1; i >= 0; i-- {
			if kk, v, ok := strings.Cut(environ[i], "="); ok && kk == k {
				return v
			}
		}
		return ""
	}
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := get(k); v != "" {
			if k == "LC_ALL" {
				k = "LANG"
			}
			return v, k
		}
	}
	return "", ""
}
