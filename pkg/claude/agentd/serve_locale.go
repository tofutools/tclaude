package agentd

import (
	"log/slog"
	"os"
	"runtime"
	"strings"

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
