//go:build !darwin

package dashsnap

import "github.com/go-rod/rod/lib/launcher"

const platformLaunchHint = ""

func configurePlatformChrome(_ *launcher.Launcher, _ string) {}
