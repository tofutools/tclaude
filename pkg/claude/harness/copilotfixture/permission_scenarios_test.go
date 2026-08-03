package copilotfixture_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/harness/copilotfixture"
)

// Scenario names are registered here, at package initialization, so the
// contract test can check that every proven or disproven entry still names a
// scenario that EXISTS.
//
// Registration on declaration rather than on execution is a deliberate choice.
// `go test -run` is how anyone iterates on a single row, and a registry keyed
// on execution would make the contract check fail for every partial run —
// which teaches people to ignore it. Split across two mechanisms instead:
//
//   - This registry answers "is the contract entry still backed by a scenario
//     that exists", and fails the build when a scenario is deleted or renamed
//     while its status stays proven.
//   - The CI job answers "did that scenario actually run", by grepping for each
//     name's explicit PASS line. Note the subtest indentation: Go indents
//     subtest results, so a gate anchored at column 0 would match the parent
//     only — and a parent whose subtests all skipped still prints PASS.
//
// Neither mechanism can answer the other's question, which is why there are
// two of them.
var permissionScenarios = struct {
	FolderTrustBlocks   string
	TrustBypass         map[string]string
	ToolApproval        map[string]string
	URLGate             map[string]string
	AmbientAllowAll     map[string]string
	DenyToolGrammar     map[string]string
	NoAskUser           string
	HeadlessNotEvidence string
}{
	NoAskUser: copilotfixture.RegisterScenario(
		"TestCopilotPermissionNoAskUserRemovesTheTool"),
	FolderTrustBlocks: copilotfixture.RegisterScenario(
		"TestCopilotPermissionFolderTrustBlocksFirst"),
	TrustBypass: registerRows("TestCopilotPermissionTrustBypassSurface",
		"allow-all-tools", "allow-all", "allow-all-paths", "add-dir-workdir",
		"config-json-trustedFolders", "settings-json-trustedFolders"),
	ToolApproval: registerRows("TestCopilotPermissionToolApprovalGate",
		"unsafe-command/no-flags", "unsafe-command/allow-all-tools", "safe-command/no-flags"),
	URLGate: registerRows("TestCopilotPermissionURLGateUnderToolApproval",
		"no-flags", "allow-all-tools"),
	AmbientAllowAll: registerRows("TestCopilotPermissionAmbientAllowAllPromotes",
		"true", "TRUE", "one", "false", "empty"),
	DenyToolGrammar: registerRows("TestCopilotPermissionDenyToolGrammar",
		"url()", "shell()", "write()", "*", "url", "url(*)", "url(example.com)",
		"shell(*)", "write(/tmp)"),
	HeadlessNotEvidence: copilotfixture.RegisterScenario(
		"TestCopilotPermissionHeadlessIsNotEvidence"),
}

// registerRows registers one scenario per table row and returns the row->name
// map, so a test's table and the registry cannot drift apart.
func registerRows(test string, rows ...string) map[string]string {
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		// Go replaces spaces with underscores in subtest names; the rows here
		// deliberately contain none, so the registered name is what `--- PASS`
		// prints and what CI greps for.
		out[row] = copilotfixture.RegisterScenario(test + "/" + goSubtestName(row))
	}
	return out
}

func goSubtestName(row string) string {
	return strings.ReplaceAll(row, " ", "_")
}

// assertScenarioRowsMatchRegistry fails when a test's table and the registry
// disagree, which is the case the registry cannot otherwise catch: a row added
// to a table but never registered would run without any contract entry able to
// cite it.
func assertScenarioRowsMatchRegistry(t *testing.T, registered map[string]string, rows []string) {
	t.Helper()
	if len(registered) != len(rows) {
		t.Fatalf("scenario table has %d rows but %d are registered; "+
			"add the new row to permissionScenarios so the contract can cite it",
			len(rows), len(registered))
	}
	for _, row := range rows {
		if _, ok := registered[row]; !ok {
			t.Fatalf("table row %q is not registered in permissionScenarios", row)
		}
	}
}

// seedTrustedFolders writes a trustedFolders grant into the named file under
// COPILOT_HOME, so a scenario can measure WHICH file 1.0.77 honours.
//
// TrustFolder is the helper other scenarios use; this one exists only for the
// bypass matrix, which has to be able to write the wrong file on purpose.
func seedTrustedFolders(t *testing.T, home, dir, file string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", home, err)
	}
	enc, err := json.Marshal(map[string]any{"trustedFolders": []string{dir}})
	if err != nil {
		t.Fatalf("marshal trustedFolders: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, file), enc, 0o600); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
}

func contains(haystack, needle string) bool {
	return strings.Contains(haystack, needle)
}
