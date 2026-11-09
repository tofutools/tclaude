package agentd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A local permission denial is a 403 with code "permission": the CLI keys its
// hints on that code, and every 403 exits 5 (awb tcl-cezwhu). A generic
// synonym ("forbidden", "denied", "permission_denied") on a new 403 is drift;
// use "permission", or a specific code that names the refusal
// ("operator_only", "sandbox_restricted", ...).
//
// The sites below keep a generic code on purpose, counted so that a new one
// in the same file still fails:
//   - the federation and peer-facing endpoints answer peers and the dashboard
//     with "permission_denied", which the dashboard keys on (fleet-harness-
//     model.js);
//   - spawn failures carry their code into the outcome kind a managed worker's
//     run records (managedIdentityFailureKind).
//
// The guard matches the one-line form a handler writes a 403 in
// (writeError(w, http.StatusForbidden, "code", ...)). A 403 built another way
// (a keyed spawnFailure literal, a code in a variable, a split argument list)
// is not seen; it is a drift tripwire, not a proof.
var forbiddenGenericCodeSites = map[string]int{
	"federation_terminal.go":        4,
	"federation_terminal_upload.go": 5,
	"harness_availability.go":       2,
	"harness_credentials.go":        3,
	"harness_operations.go":         2,
	"node_run.go":                   2,
	"node_service.go":               3,
	"node_service_launch.go":        1,
	"node_update.go":                1,
	"node_update_settings.go":       2,
	"node_update_scripts.go":        1,
	"managed_instructions.go":       2,
	"launch_identity.go":            3,
	"lifecycle_spawn_request.go":    1,
	"spawn_relaunch_overrides.go":   1,
}

func TestForbiddenResponsesDoNotUseAGenericCode(t *testing.T) {
	generic := regexp.MustCompile(`(http\.StatusForbidden|\b403), "(forbidden|denied|permission_denied)"`)
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	got := map[string]int{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		require.NoError(t, err)
		if n := len(generic.FindAllIndex(data, -1)); n > 0 {
			got[file] = n
		}
	}
	for file, n := range got {
		assert.Equal(t, forbiddenGenericCodeSites[file], n,
			`%s: a 403 with a generic code ("forbidden", "denied", "permission_denied"); a local permission denial uses "permission"`, file)
	}
	for file, n := range forbiddenGenericCodeSites {
		assert.Equal(t, n, got[file], "%s: the allowlist count is stale; lower it", file)
	}
}
