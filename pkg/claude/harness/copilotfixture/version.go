package copilotfixture

// PinnedCLIVersion is the exact `@github/copilot` release this fixture suite
// was captured against and is the only version its goldens describe.
//
// Drift mechanics, deliberately manual: the smoke test runs `copilot
// --version` and FAILS when the binary does not report this string. Bumping
// the pin is therefore a two-step, reviewable act — change this constant, then
// re-record goldens with -update and read the diff. That diff is the
// compatibility evidence TCL-970 exists to produce, so it must never be
// absorbed silently by a floating install or an auto-updating CLI (hence
// COPILOT_AUTO_UPDATE=false in the runner environment).
const PinnedCLIVersion = "1.0.77"

// PinnedCLISpec is the npm spec CI installs. Kept beside the version so a bump
// cannot update one and forget the other.
const PinnedCLISpec = "@github/copilot@" + PinnedCLIVersion

// VersionBanner is the prefix of `copilot --version` output for the pinned
// release. The CLI prints "GitHub Copilot CLI <version>." followed by an
// update hint, so the check is a contains rather than an equality.
const VersionBanner = "GitHub Copilot CLI " + PinnedCLIVersion
