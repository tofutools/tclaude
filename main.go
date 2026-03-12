package main

import (
	"os"
	"runtime/debug"

	"github.com/tofutools/tclaude/pkg/claude"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
)

func main() {
	common.SetupLogging()
	exitCode := run()
	db.Close()
	os.Exit(exitCode)
}

func run() int {
	cmd := claude.Cmd()
	cmd.Use = "tclaude"
	cmd.Version = appVersion()
	if err := cmd.Execute(); err != nil {
		return 1
	}
	return 0
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
