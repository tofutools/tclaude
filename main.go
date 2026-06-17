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

// version, when non-empty, is the version stamped at build time via
// -ldflags "-X main.version=...". Both the GoReleaser release builds and the
// Homebrew formula inject it. It is empty for a plain `go build`.
var version string

// appVersion reports the build-time stamped version if present, else falls
// back to the Go module build info (bi.Main.Version), which is populated for
// `go install <module>@version` builds. Builds from an extracted source tree
// with no stamp (e.g. a bare `go build`) report Go's "(devel)" marker, or
// "unknown-(no version)" when even that is absent.
func appVersion() string {
	if version != "" {
		return version
	}

	bi, hasBuildInfo := debug.ReadBuildInfo()
	if !hasBuildInfo {
		return "unknown-(no build info)"
	}

	versionString := bi.Main.Version
	if versionString == "" {
		versionString = "unknown-(no version)"
	}

	return versionString
}
