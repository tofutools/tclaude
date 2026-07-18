package dashsnap

import (
	"testing"

	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
)

func TestConfigureScrollbarVisibility(t *testing.T) {
	hideScrollbars := flags.Flag("hide-scrollbars")

	defaultLauncher := launcher.New()
	configureScrollbarVisibility(defaultLauncher, false)
	if !defaultLauncher.Has(hideScrollbars) {
		t.Fatal("default captures must retain Chrome's --hide-scrollbars flag")
	}

	visibleLauncher := launcher.New()
	configureScrollbarVisibility(visibleLauncher, true)
	if visibleLauncher.Has(hideScrollbars) {
		t.Fatal("ShowScrollbars captures must omit Chrome's --hide-scrollbars flag")
	}
}
