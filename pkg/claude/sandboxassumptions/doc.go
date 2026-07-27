// Package sandboxassumptions contains executable specifications of operating
// system sandbox behavior that tclaude relies on.
//
// The behavioral tests in this package are deliberately independent of
// tclaude's sandbox renderers. They invoke bubblewrap or Seatbelt directly so a
// production rendering bug cannot make both the implementation and its oracle
// agree. Each assumption names the production code that relies on it.
//
// These are not end-to-end launch smokes. Smokes prove that tclaude composes a
// complete boundary correctly; assumption tests prove the lower-level platform
// behavior on which that composition rests. Behavioral assumptions are
// environment-gated in ordinary go test runs and are hard-gated in the
// platform smoke jobs, where a skip or missing explicit PASS is a failure.
package sandboxassumptions
