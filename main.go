package main

import (
	"log/slog"
	"os"
	"runtime/debug"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/claude"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
)

func main() {
	common.SetupLogging(slog.LevelInfo)
	exitCode := run()
	db.Close()
	os.Exit(exitCode)
}

func run() int {
	cmd := claude.Cmd()
	cmd.Use = "tclaude"
	cmd.Version = appVersion()
	if err := boa.Execute(cmd); err != nil {
		return 1
	}
	return 0
}

// appVersion reports the version from the Go module build info
// (bi.Main.Version), which is populated for `go install <module>@version`
// builds. Builds from an extracted source tree (e.g. the Homebrew
// build-from-source formula) carry no module version, so they report
// "unknown-(no version)".
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
