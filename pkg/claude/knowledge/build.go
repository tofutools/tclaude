// Package knowledge is the Steward's knowledge base: the docs and source of the
// build that is actually running, so its advice matches what the operator has.
//
// The docs are embedded, so they are always there and always the ones this
// build shipped with. The source is too large to embed, and `go install` has no
// step that could generate it, so it is fetched once by commit into the
// daemon's private store under ~/.tclaude/data, which no sandbox reopens. The
// only way to it is the read-only daemon API, so a reader cannot change it and
// reaching it needs no new filesystem access.
package knowledge

import (
	"runtime/debug"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// BuildState says how far a build can be tied to a commit.
type BuildState string

const (
	// StateExact is a clean build of a known commit or release tag.
	StateExact BuildState = "exact"
	// StateDirty is a build of a known commit plus uncommitted changes.
	StateDirty BuildState = "dirty"
	// StateUnknown is a build that names no commit or release.
	StateUnknown BuildState = "unknown"
)

// Build pins the knowledge base to what is running.
type Build struct {
	Version string     `json:"version"`
	State   BuildState `json:"state"`
	// Commit is the full hash, or the 12 digits a pseudo-version carries.
	// Empty for a tag until its source has been fetched.
	Commit string `json:"commit,omitempty"`
	// Ref is what the source is fetched by: the commit or the release tag.
	Ref string `json:"ref,omitempty"`
}

// Label is the one line a reader must see before trusting an answer.
func (b Build) Label() string {
	id := b.Commit
	if id == "" {
		id = b.Ref
	}
	switch b.State {
	case StateExact:
		return "exact: docs and source match " + shortRef(id) + ", the running build"
	case StateDirty:
		return "dirty: built from " + shortRef(id) + " with uncommitted changes. Docs are as built; source is that commit without the changes, so it may differ from what is running"
	}
	return "unknown: this build names no commit. Docs are as built, there is no source, and neither can be tied to a release"
}

func shortRef(ref string) string {
	if len(ref) >= 40 {
		return ref[:12]
	}
	return ref
}

// CurrentBuild identifies the running binary.
func CurrentBuild(version string) Build {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return Build{Version: version, State: StateUnknown}
	}
	return identify(bi, version)
}

// identify reads what the Go toolchain stamped. A build from a checkout carries
// VCS settings; `go install module@version` carries only the module version, a
// tag or a pseudo-version with the commit in it. Anything else is unknown: a
// guess at `main` would present another build's code as this one's.
func identify(bi *debug.BuildInfo, version string) Build {
	var revision, modified string
	for _, setting := range bi.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}
	build := Build{Version: version, State: StateUnknown}
	if revision != "" {
		build.Commit, build.Ref, build.State = revision, revision, StateDirty
		if modified == "false" {
			build.State = StateExact
		}
		return build
	}
	moduleVersion, dirty := strings.CutSuffix(bi.Main.Version, "+dirty")
	if !semver.IsValid(moduleVersion) {
		return build
	}
	build.State = StateExact
	if dirty {
		build.State = StateDirty
	}
	build.Ref = moduleVersion
	if module.IsPseudoVersion(moduleVersion) {
		rev, err := module.PseudoVersionRev(moduleVersion)
		if err != nil {
			return Build{Version: version, State: StateUnknown}
		}
		build.Commit, build.Ref = rev, rev
	}
	return build
}
