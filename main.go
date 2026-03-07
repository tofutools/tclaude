package main

import (
	"os"
	"runtime/debug"

	"github.com/tofutools/tclaude/pkg/claude"
)

func main() {
	cmd := claude.Cmd()
	cmd.Use = "tclaude"
	cmd.Version = appVersion()
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func appVersion() string {
	bi, hasBuilInfo := debug.ReadBuildInfo()
	if !hasBuilInfo {
		return "unknown-(no build info)"
	}

	versionString := bi.Main.Version
	if versionString == "" {
		versionString = "unknown-(no version)"
	}

	return versionString
}
