package copilotfixture

// PinnedCLIVersion is the exact `@github/copilot` release the GOLDENS were
// captured against, and is the only version they describe. It also names their
// directory: testdata/<PinnedCLIVersion>/.
//
// Scope, since TCL-1046 split this package in two: the pin belongs to LAB mode
// only. Lab scenarios run `copilot --version` and FAIL when the binary does
// not report this string, because a byte-level golden is meaningless against
// an unknown release. The per-PR regression scenarios assert behaviour rather
// than bytes, so they neither read this constant nor care which release is
// installed — which is what lets ci.yml pin an npm spec the way every other
// harness's job does instead of the Go package being welded to one release.
//
// Drift mechanics, deliberately manual: bumping is a two-step, reviewable act
// — change this constant, then re-record goldens with -update (which implies
// lab mode) and read the diff. That diff is the compatibility evidence TCL-970
// exists to produce, so it must never be absorbed silently by a floating
// install or an auto-updating CLI (hence COPILOT_AUTO_UPDATE=false in the
// runner environment).
const PinnedCLIVersion = "1.0.77"

// PinnedCLISpec is the npm spec the LAB workflow installs, and the spelling
// the skip messages suggest to anyone running the lab locally.
//
// It is not what ci.yml's per-PR smoke installs. That job pins its own spec in
// the workflow, deliberately: its scenarios are release-agnostic, so coupling
// them to this constant would turn an upstream publish into a red PR. The two
// pins are allowed to differ, and the lab is what reconciles them — a change
// to ci.yml triggers it.
const PinnedCLISpec = "@github/copilot@" + PinnedCLIVersion

// VersionBanner is the prefix of `copilot --version` output for the pinned
// release. The CLI prints "GitHub Copilot CLI <version>." followed by an
// update hint, so the check is a contains rather than an equality.
const VersionBanner = "GitHub Copilot CLI " + PinnedCLIVersion
