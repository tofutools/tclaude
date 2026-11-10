//go:build !linux && !darwin

package agent

import "errors"

func startDetachedServe([]string, []string, string) error {
	return errors.New("starting agentd from the app launcher is supported on Linux and macOS")
}

func appendLauncherLog(string, string) {}
