package buildversion

import "runtime/debug"

var stampedVersion string

// SetStampedVersion records the release-build version stamped into main.version.
// It returns a restore function for tests.
func SetStampedVersion(v string) func() {
	prev := stampedVersion
	stampedVersion = v
	return func() { stampedVersion = prev }
}

// AppVersion reports the build-time stamped version if present, else falls
// back to the Go module build info (bi.Main.Version), which is populated for
// `go install <module>@version` builds. Builds from an extracted source tree
// with no stamp (e.g. a bare `go build`) report Go's "(devel)" marker, or
// "unknown-(no version)" when even that is absent.
func AppVersion() string {
	if stampedVersion != "" {
		return stampedVersion
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
