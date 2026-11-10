package launcher

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

// A process started by launchd, systemd or a desktop launcher often has no
// locale at all, where a terminal would have given it one: macOS's
// Terminal and iTerm set LANG themselves (it is rarely in the shell
// profile). Its children then run in the C locale: tmux clients escape
// control characters in what they print, and harnesses fall back to ASCII.
// ChooseLocale picks the locale a terminal would most likely have given,
// and WithLocale sets it.

// LocaleSources are where ChooseLocale looks beyond the environment.
// SystemLocaleSources is the production set; tests swap it.
type LocaleSources struct {
	GOOS string
	// AppleLocale is the user's macOS locale (`defaults read -g
	// AppleLocale`), such as "en_GB" or "en_GB@rg=sezzzz".
	AppleLocale func() (string, error)
	// SystemLANG is LANG from /etc/locale.conf or /etc/default/locale.
	SystemLANG func() string
	// Installed lists the installed locales (`locale -a`).
	Installed func() ([]string, error)
}

// Where a chosen locale came from (ChooseLocale's source).
const (
	LocaleFromEnvironment = "environment"
	LocaleFromLoginShell  = "login shell"
	LocaleFromAppleLocale = "macOS AppleLocale"
	LocaleFromSystem      = "system locale file"
	LocaleFromFallback    = "fallback"
)

var localeKeys = []string{"LC_ALL", "LC_CTYPE", "LANG"}

// ChooseLocale returns the UTF-8 locale to give a process whose
// environment is env, and where it came from. value is "" when env already
// has a UTF-8 locale (or an explicit non-UTF-8 LC_ALL, which is the user's
// choice and overrides anything set beside it), or when nothing usable is
// installed. The order:
//  1. a UTF-8 locale already in env: keep it;
//  2. a UTF-8 LC_ALL / LC_CTYPE / LANG from login, the login shell's
//     environment;
//  3. macOS: the user's AppleLocale, its @modifiers dropped, plus .UTF-8;
//     Linux: the system LANG from /etc/locale.conf or /etc/default/locale;
//  4. en_US.UTF-8 on macOS, C.UTF-8 on Linux.
//
// Steps 3 and 4 take a locale only when it is installed.
func ChooseLocale(env, login []string, s LocaleSources) (value, source string) {
	if v, k := effectiveLocale(env); v != "" && (isUTF8Locale(v) || k == "LC_ALL") {
		return "", LocaleFromEnvironment
	}
	if v, _ := effectiveLocale(login); isUTF8Locale(v) {
		return v, LocaleFromLoginShell
	}
	var installed []string
	listed := false
	if s.Installed != nil {
		if l, err := s.Installed(); err == nil {
			installed, listed = l, true
		}
	}
	// Without a list, trust the candidate: a missing locale only means the
	// C locale, which is what the process has now.
	exists := func(name string) bool { return !listed || localeInstalled(installed, name) }
	switch s.GOOS {
	case "darwin":
		if s.AppleLocale != nil {
			if al, err := s.AppleLocale(); err == nil {
				if base, _, _ := strings.Cut(strings.TrimSpace(al), "@"); base != "" {
					if name := base + ".UTF-8"; exists(name) {
						return name, LocaleFromAppleLocale
					}
				}
			}
		}
		if exists("en_US.UTF-8") {
			return "en_US.UTF-8", LocaleFromFallback
		}
	case "linux":
		if s.SystemLANG != nil {
			if v := s.SystemLANG(); isUTF8Locale(v) && exists(v) {
				return v, LocaleFromSystem
			}
		}
		for _, name := range []string{"C.UTF-8", "en_US.UTF-8"} {
			if exists(name) {
				return name, LocaleFromFallback
			}
		}
	}
	return "", ""
}

// WithLocale returns env with value as its locale: LANG when env names no
// locale at all, else LC_CTYPE, which outranks LANG (and replaces a
// non-UTF-8 LC_CTYPE) for the character set. A blank value returns env
// unchanged.
func WithLocale(env []string, value string) []string {
	if value == "" {
		return env
	}
	return append(env, LocaleKey(env)+"="+value)
}

// LocaleKey is the variable WithLocale sets in env.
func LocaleKey(env []string) string {
	if lookup(env, "LANG") != "" || lookup(env, "LC_CTYPE") != "" {
		return "LC_CTYPE"
	}
	return "LANG"
}

// effectiveLocale returns the locale env's character set comes from: the
// first set of LC_ALL, LC_CTYPE and LANG, as setlocale reads them.
func effectiveLocale(env []string) (value, key string) {
	for _, k := range localeKeys {
		if v := lookup(env, k); v != "" {
			return v, k
		}
	}
	return "", ""
}

func isUTF8Locale(v string) bool {
	v = strings.ToLower(v)
	return strings.Contains(v, "utf-8") || strings.Contains(v, "utf8")
}

// localeInstalled matches name against `locale -a`, which may spell the
// charset utf8 (glibc) or UTF-8 (macOS).
func localeInstalled(installed []string, name string) bool {
	norm := func(s string) string { return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), "-", "") }
	want := norm(name)
	for _, l := range installed {
		if norm(l) == want {
			return true
		}
	}
	return false
}

func lookup(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v
		}
	}
	return ""
}

// SystemLocaleSources reads the host's locale settings.
func SystemLocaleSources(goos string) LocaleSources {
	run := func(name string, args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, name, args...).Output()
	}
	return LocaleSources{
		GOOS: goos,
		AppleLocale: func() (string, error) {
			out, err := run("defaults", "read", "-g", "AppleLocale")
			return strings.TrimSpace(string(out)), err
		},
		SystemLANG: func() string {
			for _, f := range []string{"/etc/locale.conf", "/etc/default/locale"} {
				if data, err := os.ReadFile(f); err == nil {
					if v := langFromLocaleFile(data); v != "" {
						return v
					}
				}
			}
			return ""
		},
		Installed: func() ([]string, error) {
			out, err := run("locale", "-a")
			if err != nil {
				return nil, err
			}
			return strings.Split(string(out), "\n"), nil
		},
	}
}

// langFromLocaleFile reads LANG from a locale.conf-style file
// (LANG=en_GB.UTF-8, optionally quoted).
func langFromLocaleFile(data []byte) string {
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if v, ok := strings.CutPrefix(line, "LANG="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}
