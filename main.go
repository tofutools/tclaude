package main

import (
	"log/slog"
	"os"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/tofutools/tclaude/pkg/claude"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
)

func main() {
	common.SetupLogging(slog.LevelInfo)
	exitCode := run()
	db.Close()
	os.Exit(exitCode)
}

func run() int {
	buildversion.SetStampedVersion(version)
	cmd := claude.Cmd()
	cmd.Use = "tclaude"
	cmd.Version = buildversion.AppVersion()
	if err := boa.Execute(cmd); err != nil {
		return 1
	}
	return 0
}

// version, when non-empty, is the version stamped at build time via
// -ldflags "-X main.version=...". Both the GoReleaser release builds and the
// Homebrew formula inject it. It is empty for a plain `go build`.
var version string
