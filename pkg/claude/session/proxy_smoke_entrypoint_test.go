package session

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// The filtered-proxy smokes are launched by scripts/filtered-proxy-smoke/run.sh
// rather than by inline workflow steps, so that adding or extending a smoke is
// a repo change instead of a workflow merge. That moved the evidence discipline
// — the rule that a skipped, renamed or filtered-out smoke is a hard failure —
// into files this repo's own tests can reach.
//
// These tests are that reach. They run the entrypoint's self-check on every
// ordinary `go test ./...`, so a broken evidence checker or a manifest that has
// drifted from the flows is caught by normal CI, long before the smoke shard
// runs and regardless of whether anyone remembers to look at it.
//
// Neither test touches a sandbox, a network namespace or /etc/hosts: they
// invoke only the validate-only path, which exists precisely so the guards can
// be exercised somewhere safe. The smokes themselves must never run locally.

func proxySmokeScript(t *testing.T, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the smoke entrypoint is a POSIX shell script")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required to check the smoke entrypoint")
	}
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	require.NoError(t, err)
	return filepath.Join(root, "scripts", "filtered-proxy-smoke", name)
}

// TestProxySmokeEvidenceCheckerSelfTest runs the checker's own suite: the
// synthetic skipped, renamed, zero-test, subtest-only, prefix-collision,
// partial and empty-requirement logs it must refuse, and the green log it must
// accept.
//
// The entrypoint runs this too, before it trusts any smoke result. Having it
// here as well means a change that breaks the checker fails an ordinary test
// run rather than waiting for the next smoke shard.
func TestProxySmokeEvidenceCheckerSelfTest(t *testing.T) {
	script := proxySmokeScript(t, "selftest.sh")
	output, err := exec.Command("bash", script).CombinedOutput()
	require.NoErrorf(t, err,
		"the smoke evidence checker failed its own self-test:\n%s", output)
}

// TestProxySmokeManifestMatchesItsFlows checks the other half: every flow must
// claim at least one required test, and every claimed test must belong to a
// flow that exists.
//
// Both directions matter. A flow with no manifest entry is a smoke that cannot
// fail; a manifest entry with no flow is evidence claimed for a smoke that no
// longer runs. Either one silently converts a capability cell's backing into
// nothing, which is the failure this whole layout is built to prevent.
func TestProxySmokeManifestMatchesItsFlows(t *testing.T) {
	script := proxySmokeScript(t, "run.sh")
	output, err := exec.Command("bash", script, "--validate-only").CombinedOutput()
	require.NoErrorf(t, err,
		"the smoke manifest has drifted from its flows:\n%s", output)
}
