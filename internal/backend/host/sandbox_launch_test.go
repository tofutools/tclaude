//go:build linux || darwin

package host

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

type launchPolicyReader struct {
	ref    model.SandboxProfileRef
	policy model.SandboxPolicy
}

func (r launchPolicyReader) ReadSandboxRevision(_ context.Context, ref model.SandboxProfileRef) (model.SandboxPolicy, error) {
	if ref != r.ref {
		return model.SandboxPolicy{}, fmt.Errorf("unexpected policy revision")
	}
	return r.policy, nil
}
func materializedLaunchPolicy(t *testing.T, paths *SandboxPathInspector, policy model.SandboxPolicy) (model.SandboxSelection, sandboxpolicy.PolicyMaterialization) {
	t.Helper()
	hash, err := sandboxpolicy.ContentHash(policy)
	require.NoError(t, err)
	ref := model.SandboxProfileRef{ProfileID: "policy", RevisionID: "revision", ContentHash: hash}
	materialized, err := sandboxpolicy.MaterializeScopes(context.Background(), []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: ref}}, launchPolicyReader{ref, policy}, paths)
	require.NoError(t, err)
	selected, err := materialized.LaunchSelection()
	require.NoError(t, err)
	return selected, materialized
}

func TestShellSandboxPreparationRetainsExactArtifactAndAbortRemovesOnlyPreparation(t *testing.T) {
	private, err := os.MkdirTemp("/tmp", "sb-shell-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(private)) })
	workspace := t.TempDir()
	inspector, err := NewSandboxPathInspector([]string{private})
	require.NoError(t, err)
	// Preparation does not execute the wrapper; use a real executable on either OS.
	planner, err := NewSandboxLaunchPreparer(SandboxLaunchConfig{Inspector: inspector, Wrapper: "/bin/sh", Bootstrap: "/bin/sh", Artifacts: private})
	require.NoError(t, err)
	selected, materialized := materializedLaunchPolicy(t, inspector, model.SandboxPolicy{DarwinAllowMachRegister: true, FilesystemRoot: model.SandboxRootSeparate, Filesystem: []model.SandboxFilesystemRule{{HostPath: workspace, Access: model.SandboxFilesystemWrite}}, Environment: model.Environment{"LITERAL": "$HOME stays literal"}, Network: &model.SandboxNetwork{Baseline: model.SandboxNetworkDeny}})
	shell, err := NewShellHost(ShellConfig{HostSandbox: planner, Terminal: TerminalHost{PrivateRoot: filepath.Join(private, "terminal")}, Executable: "/bin/sh"})
	require.NoError(t, err)
	request := ports.ShellPreparationRequest{ExecutionID: "shell", Attempt: 1, WorkspaceID: "workspace", WorkingDirectory: workspace, Sandbox: model.SandboxUnconfined, HostSandbox: &selected, HostSandboxPolicy: &materialized}
	prepared, err := shell.PrepareShell(context.Background(), request)
	require.NoError(t, err)
	description := prepared.Describe()
	require.Equal(t, selected.PolicyHash, description.HostSandboxPolicyHash)
	evidence, err := decodeShellEvidence(description.Evidence)
	require.NoError(t, err)
	require.NotNil(t, evidence.HostSandbox)
	require.NoError(t, VerifySandboxChild(context.Background(), *evidence.HostSandbox))
	input, _, err := readSandboxChild(*evidence.HostSandbox)
	require.NoError(t, err)
	require.Contains(t, input.Environment, "LITERAL=$HOME stays literal")
	require.True(t, input.PrivateNetwork)
	require.True(t, input.DarwinAllowMachRegister)
	require.NoFileExists(t, evidence.HostSandbox.Path+".started", "preparation never starts the command")
	require.NoError(t, prepared.Abort(context.Background()))
	require.NoDirExists(t, filepath.Dir(evidence.HostSandbox.Path))
	require.DirExists(t, workspace)

	changed := selected.Clone()
	changed.PolicyHash = strings.Repeat("c", 64)
	request.HostSandbox = &changed
	_, err = shell.PrepareShell(context.Background(), request)
	require.ErrorContains(t, err, "does not match")
}

func TestSandboxArtifactsCannotBePlacedOutsideProtectedState(t *testing.T) {
	protected := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.Chmod(outside, 0700))
	inspector, err := NewSandboxPathInspector([]string{protected})
	require.NoError(t, err)
	_, err = NewSandboxLaunchPreparer(SandboxLaunchConfig{Inspector: inspector, Wrapper: "/bin/sh", Bootstrap: "/bin/sh", Artifacts: outside})
	require.ErrorContains(t, err, "protected host root")
}

// Native platform journeys use the same materialization-to-artifact preparer
// as shell/provider composition; their child probes still exercise real OS IO.
func prepareNativeLaunchPolicy(t *testing.T, inspector *SandboxPathInspector, private, wrapper string, child ProcessSpec, bound *SandboxMountBindings, allowMachRegister ...bool) (SandboxChildArtifact, error) {
	t.Helper()
	rules := []model.SandboxFilesystemRule{}
	for _, pin := range bound.Pins() {
		rules = append(rules, model.SandboxFilesystemRule{HostPath: pin.Source, GuestPath: pin.Guest, Access: pin.Access, ExpectedKind: pin.Kind})
	}
	selected, materialized := materializedLaunchPolicy(t, inspector, model.SandboxPolicy{DarwinAllowMachRegister: len(allowMachRegister) > 0 && allowMachRegister[0], FilesystemRoot: model.SandboxRootSeparate, Filesystem: rules, Network: &model.SandboxNetwork{Baseline: model.SandboxNetworkDeny}})
	bootstrap, err := os.Executable()
	require.NoError(t, err)
	planner, err := NewSandboxLaunchPreparer(SandboxLaunchConfig{Inspector: inspector, Wrapper: wrapper, Bootstrap: bootstrap, Artifacts: private})
	require.NoError(t, err)
	return planner.Prepare(context.Background(), selected, materialized, child)
}
