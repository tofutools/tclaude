package session

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// The smokes are launched by repo-side entrypoints under scripts/ rather than
// by inline workflow steps, so that adding or extending a smoke is a repo change
// instead of a workflow merge. That moved the evidence discipline — the rule
// that a skipped, renamed or filtered-out smoke is a hard failure — into files
// this repo's own tests can reach.
//
// These tests are that reach. They run the shared self-check and EVERY shard's
// manifest validation on every ordinary `go test ./...`, so a broken evidence
// checker or a manifest that has drifted from its flows is caught by normal CI,
// long before any smoke shard runs and regardless of whether anyone remembers to
// look at it.
//
// Nothing here touches a sandbox, a network namespace or /etc/hosts: only the
// validate-only path is invoked, which exists precisely so the guards can be
// exercised somewhere safe. The smokes themselves must never run locally.

// proxySmokeEntrypoints is the list every guard below runs over. A new shard
// adds its directory here, and is then held to the same discipline as the
// others by construction rather than by whoever remembers to copy the test.
var proxySmokeEntrypoints = []string{
	"filtered-proxy-smoke",
	"proxy-posture-e2e",
}

func proxySmokeScript(t *testing.T, entrypoint, name string) string {
	t.Helper()
	// The entrypoints run only on the Linux CI shards, and they use bash 4
	// features (associative arrays, mapfile). macOS ships bash 3.2, and this
	// test resolves `bash` from PATH rather than through the shebang, so
	// without this guard an ordinary `go test ./...` on a macOS runner would
	// fail with `declare: -A: invalid option` — a red shard for a reason
	// unrelated to anything under test.
	//
	// Skipping is honest rather than lossy: the Linux shards run this on every
	// PR, which is the platform the scripts are for.
	if runtime.GOOS != "linux" {
		t.Skipf("the smoke entrypoints target the Linux CI shards (GOOS=%s)",
			runtime.GOOS)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required to check the smoke entrypoints")
	}
	if !bashSupportsAssociativeArrays(t) {
		t.Skip("the smoke entrypoints require bash 4 or newer")
	}
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	require.NoError(t, err)
	return filepath.Join(root, "scripts", entrypoint, name)
}

// bashSupportsAssociativeArrays probes the interpreter rather than parsing a
// version string: the feature is what matters, and a probe cannot be fooled by
// a vendor version scheme.
func bashSupportsAssociativeArrays(t *testing.T) bool {
	t.Helper()
	return exec.Command("bash", "-c", "declare -A probe").Run() == nil
}

// TestProxySmokeEvidenceCheckerSelfTest runs the shared checker's own suite: the
// synthetic skipped, renamed, zero-test, subtest-only, prefix-collision,
// partial and empty-requirement logs it must refuse, the green log it must
// accept, and the manifest drift guards in both directions.
//
// Every shard's entrypoint runs this too, before it trusts any smoke result.
// Having it here as well means a change that breaks the checker fails an
// ordinary test run rather than waiting for the next smoke shard. It is invoked
// through each shard's own entry point, so a shard that stopped delegating to
// the shared discipline would be caught here rather than reviewed for.
func TestProxySmokeEvidenceCheckerSelfTest(t *testing.T) {
	for _, entrypoint := range proxySmokeEntrypoints {
		t.Run(entrypoint, func(t *testing.T) {
			script := proxySmokeScript(t, entrypoint, "selftest.sh")
			output, err := exec.Command("bash", script).CombinedOutput()
			require.NoErrorf(t, err,
				"the smoke evidence checker failed its own self-test:\n%s", output)
		})
	}
}

// TestProxySmokeManifestMatchesItsFlows checks the other half, for every shard:
// each flow must claim at least one required test, and each claimed test must
// belong to a flow that exists.
//
// Both directions matter. A flow with no manifest entry is a smoke that cannot
// fail; a manifest entry with no flow is evidence claimed for a smoke that no
// longer runs. Either one silently converts a capability cell's backing — or a
// posture's end-to-end verification — into nothing, which is the failure this
// layout is built to prevent.
func TestProxySmokeManifestMatchesItsFlows(t *testing.T) {
	for _, entrypoint := range proxySmokeEntrypoints {
		t.Run(entrypoint, func(t *testing.T) {
			script := proxySmokeScript(t, entrypoint, "run.sh")
			output, err := exec.Command(
				"bash", script, "--validate-only").CombinedOutput()
			require.NoErrorf(t, err,
				"the smoke manifest has drifted from its flows:\n%s", output)
		})
	}
}
