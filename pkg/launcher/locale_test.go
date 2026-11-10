package launcher

import (
	"errors"
	"slices"
	"testing"
)

func TestChooseLocale(t *testing.T) {
	macInstalled := []string{"C", "POSIX", "en_GB.UTF-8", "en_US.UTF-8", "sv_SE.UTF-8"}
	glibcInstalled := []string{"C", "C.utf8", "POSIX", "en_GB.utf8", "en_US.utf8"}
	mac := func(apple string, appleErr error, installed []string) LocaleSources {
		return LocaleSources{
			GOOS:        "darwin",
			AppleLocale: func() (string, error) { return apple, appleErr },
			Installed:   func() ([]string, error) { return installed, nil },
		}
	}
	linux := func(system string, installed []string) LocaleSources {
		return LocaleSources{
			GOOS:       "linux",
			SystemLANG: func() string { return system },
			Installed:  func() ([]string, error) { return installed, nil },
		}
	}
	for _, tc := range []struct {
		name          string
		env, login    []string
		src           LocaleSources
		value, source string
	}{
		{"env UTF-8 kept", []string{"LANG=sv_SE.UTF-8"}, []string{"LANG=en_GB.UTF-8"}, mac("en_GB", nil, macInstalled), "", LocaleFromEnvironment},
		{"env LC_CTYPE UTF-8 kept", []string{"LANG=C", "LC_CTYPE=UTF-8"}, nil, mac("en_GB", nil, macInstalled), "", LocaleFromEnvironment},
		{"explicit LC_ALL=C left alone", []string{"LC_ALL=C"}, nil, mac("en_GB", nil, macInstalled), "", LocaleFromEnvironment},
		{"login shell", nil, []string{"LANG=sv_SE.UTF-8"}, mac("en_GB", nil, macInstalled), "sv_SE.UTF-8", LocaleFromLoginShell},
		{"login shell not UTF-8 skipped", nil, []string{"LANG=C"}, mac("en_GB", nil, macInstalled), "en_GB.UTF-8", LocaleFromAppleLocale},
		{"AppleLocale", nil, nil, mac("en_GB", nil, macInstalled), "en_GB.UTF-8", LocaleFromAppleLocale},
		{"AppleLocale modifier stripped", []string{"LANG=C"}, nil, mac("en_GB@rg=sezzzz", nil, macInstalled), "en_GB.UTF-8", LocaleFromAppleLocale},
		{"AppleLocale not installed", nil, nil, mac("en_SE", nil, macInstalled), "en_US.UTF-8", LocaleFromFallback},
		{"AppleLocale unreadable", nil, nil, mac("", errors.New("no such key"), macInstalled), "en_US.UTF-8", LocaleFromFallback},
		{"linux system LANG", nil, nil, linux("en_GB.UTF-8", glibcInstalled), "en_GB.UTF-8", LocaleFromSystem},
		{"linux system LANG not UTF-8", nil, nil, linux("C", glibcInstalled), "C.UTF-8", LocaleFromFallback},
		{"linux no C.UTF-8", nil, nil, linux("", []string{"C", "en_US.utf8"}), "en_US.UTF-8", LocaleFromFallback},
		{"linux nothing installed", nil, nil, linux("", []string{"C", "POSIX"}), "", ""},
	} {
		v, src := ChooseLocale(tc.env, tc.login, tc.src)
		if v != tc.value || src != tc.source {
			t.Errorf("%s: got %q from %q, want %q from %q", tc.name, v, src, tc.value, tc.source)
		}
	}

	// `locale -a` failing does not block the choice.
	s := mac("en_GB", nil, nil)
	s.Installed = func() ([]string, error) { return nil, errors.New("no locale") }
	if v, src := ChooseLocale(nil, nil, s); v != "en_GB.UTF-8" || src != LocaleFromAppleLocale {
		t.Errorf("unlisted: %q %q", v, src)
	}
}

func TestWithLocale(t *testing.T) {
	if got := WithLocale([]string{"PATH=/bin"}, "en_GB.UTF-8"); !slices.Equal(got, []string{"PATH=/bin", "LANG=en_GB.UTF-8"}) {
		t.Errorf("no LANG: %v", got)
	}
	if got := WithLocale([]string{"LANG=C"}, "en_GB.UTF-8"); !slices.Equal(got, []string{"LANG=C", "LC_CTYPE=en_GB.UTF-8"}) {
		t.Errorf("LANG=C: %v", got)
	}
	if got := WithLocale([]string{"LANG=C"}, ""); !slices.Equal(got, []string{"LANG=C"}) {
		t.Errorf("blank: %v", got)
	}
}

func TestLangFromLocaleFile(t *testing.T) {
	if v := langFromLocaleFile([]byte("# comment\nLANG=\"en_GB.UTF-8\"\nLC_TIME=sv_SE.UTF-8\n")); v != "en_GB.UTF-8" {
		t.Errorf("got %q", v)
	}
	if v := langFromLocaleFile([]byte("LC_ALL=C\n")); v != "" {
		t.Errorf("got %q", v)
	}
}
