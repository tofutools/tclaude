package sandboxpolicy_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

type canonicalPaths map[string]string

func (p canonicalPaths) ResolveSandboxHostPath(_ context.Context, path string) (string, error) {
	if canonical, ok := p[path]; ok {
		return canonical, nil
	}
	return filepath.Clean(path), nil
}

func TestIncludeCompositionRestoresSharedSiblingIntentAndIntersectsAccess(t *testing.T) {
	r := newRevisions()
	shared := r.add(t, "shared", model.SandboxPolicy{
		Environment:   model.Environment{"VALUE": "shared", "CACHE": "literal"},
		Filesystem:    []model.SandboxFilesystemRule{{HostPath: "/workspace", Access: model.SandboxFilesystemRead, ExpectedKind: "directory"}},
		Network:       &model.SandboxNetwork{Baseline: model.SandboxNetworkDeny, Engine: model.SandboxNetworkPacket, Namespace: "private", Allow: []model.SandboxDestination{{Domain: "example.com", IncludeSubdomains: true}}},
		UnixSockets:   &model.SandboxUnixSockets{Mode: "closed"},
		HarnessConfig: model.SandboxHarnessConfigRead,
		PreLaunch:     []model.SandboxSetupBlock{{Name: "first", Script: "echo shared"}},
	})
	first := r.add(t, "first", model.SandboxPolicy{Includes: []model.SandboxProfileRef{shared}, Environment: model.Environment{"VALUE": "first"}, AgentDirectories: []string{"CACHE"}, Network: &model.SandboxNetwork{Baseline: model.SandboxNetworkAllow, Engine: model.SandboxNetworkProxy}, Filesystem: []model.SandboxFilesystemRule{{HostPath: "/workspace", Access: model.SandboxFilesystemWrite}}, PreLaunch: []model.SandboxSetupBlock{{Name: "first", Script: "echo first"}, {Name: "second", Script: "echo second"}}})
	second := r.add(t, "second", model.SandboxPolicy{Includes: []model.SandboxProfileRef{shared}, Environment: model.Environment{"SECOND": "yes"}, Network: &model.SandboxNetwork{Baseline: model.SandboxNetworkAllow}, UnixSockets: &model.SandboxUnixSockets{Mode: "open"}, HarnessConfig: model.SandboxHarnessConfigWrite})
	root := r.add(t, "root", model.SandboxPolicy{Includes: []model.SandboxProfileRef{first, second}, Resources: model.SandboxResources{Memory: "9007199254740993B"}})
	got, err := sandboxpolicy.ComposeIncludes(context.Background(), root, r, canonicalPaths{})
	require.NoError(t, err)
	require.Equal(t, "shared", got.Values.Environment["VALUE"])
	require.Equal(t, "literal", got.Values.Environment["CACHE"])
	require.Empty(t, got.Values.AgentDirectories)
	require.Equal(t, model.SandboxFilesystemRead, got.Values.Filesystem[0].Access)
	require.Equal(t, "directory", got.Values.Filesystem[0].ExpectedKind)
	require.Equal(t, "echo shared", got.Values.PreLaunch[0].Script)
	require.Equal(t, "second", got.Values.PreLaunch[1].Name)
	require.Equal(t, model.SandboxHarnessConfigRead, got.Values.HarnessConfig)
	require.Equal(t, "9007199254740993B", got.Values.Resources.Memory)
	require.Len(t, got.NetworkAll, 3)
	require.NotNil(t, got.NetworkEngine)
	require.Equal(t, model.SandboxNetworkPacket, got.NetworkEngine.Engine)
	require.Equal(t, shared, got.NetworkEngine.Source)
	require.Equal(t, "private", got.NetworkNamespace)
	require.Equal(t, model.SandboxNetworkDeny, got.NetworkAll[0].Policy.Baseline)
	require.Equal(t, model.SandboxNetworkAllow, got.NetworkAll[1].Policy.Baseline)
	require.Len(t, got.SocketAll, 2)
	require.Equal(t, "closed", got.SocketAll[0].Policy.Mode)
	require.Nil(t, got.Values.Network)
	require.Nil(t, got.Values.UnixSockets)
	require.Empty(t, got.Values.Includes)
	for _, count := range r.reads {
		require.Equal(t, 1, count)
	}
	got.NetworkAll[0].Policy.Allow[0].Domain = "mutated.invalid"
	require.Equal(t, "example.com", r.policies[shared.RevisionID].Network.Allow[0].Domain)
}

func TestIncludeCompositionRefusesConflictingHostAndKindCommitments(t *testing.T) {
	for _, tc := range []struct {
		name, host, kind string
		wantError        bool
	}{
		{"different host", "/other", "directory", true},
		{"same canonical host alias", "/alias", "directory", false},
		{"file cannot become directory", "/workspace", "file", true},
		{"omission preserves kind", "/workspace", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRevisions()
			parent := r.add(t, "parent", model.SandboxPolicy{Filesystem: []model.SandboxFilesystemRule{{HostPath: "/workspace", GuestPath: "/guest", Access: model.SandboxFilesystemRead, ExpectedKind: "directory"}}})
			root := r.add(t, "root", model.SandboxPolicy{Includes: []model.SandboxProfileRef{parent}, Filesystem: []model.SandboxFilesystemRule{{HostPath: tc.host, GuestPath: "/guest", Access: model.SandboxFilesystemWrite, ExpectedKind: tc.kind}}})
			got, err := sandboxpolicy.ComposeIncludes(context.Background(), root, r, canonicalPaths{"/alias": "/workspace"})
			if tc.wantError {
				require.ErrorIs(t, err, sandboxpolicy.ErrInvalidClosure)
				return
			}
			require.NoError(t, err)
			require.Len(t, got.Values.Filesystem, 1)
			require.Equal(t, "directory", got.Values.Filesystem[0].ExpectedKind)
			require.Equal(t, "/workspace", got.Values.Filesystem[0].HostPath)
		})
	}
}

func TestScopeUnionKeepsDenyAndTmpfsCeilingWhileExplicitValuesWin(t *testing.T) {
	r := newRevisions()
	global := r.add(t, "global", model.SandboxPolicy{Filesystem: []model.SandboxFilesystemRule{{HostPath: "/workspace", Access: model.SandboxFilesystemDeny}}, Tmpfs: []model.SandboxTmpfs{{GuestPath: "/scratch", Size: "1MiB"}}, Environment: model.Environment{"VALUE": "global"}, Resources: model.SandboxResources{Memory: "2GiB"}})
	explicit := r.add(t, "explicit", model.SandboxPolicy{Filesystem: []model.SandboxFilesystemRule{{HostPath: "/workspace", Access: model.SandboxFilesystemWrite}}, Tmpfs: []model.SandboxTmpfs{{GuestPath: "/scratch", Size: "2MiB"}}, Environment: model.Environment{"VALUE": "explicit"}, Resources: model.SandboxResources{Memory: "3GiB"}})
	selections := []sandboxpolicy.ScopeSelection{{Scope: sandboxpolicy.ScopeExplicit, Ref: explicit}, {Scope: sandboxpolicy.ScopeGlobal, Ref: global}}
	result, err := sandboxpolicy.ComposeScopes(context.Background(), selections, r, canonicalPaths{})
	require.NoError(t, err)
	require.Equal(t, model.SandboxFilesystemDeny, result.Combined.Values.Filesystem[0].Access)
	require.Equal(t, "1MiB", result.Combined.Values.Tmpfs[0].Size)
	require.Equal(t, "explicit", result.Combined.Values.Environment["VALUE"])
	require.Equal(t, "3GiB", result.Combined.Values.Resources.Memory)
	require.Equal(t, sandboxpolicy.ScopeGlobal, result.Applied[0].Scope)
	require.Empty(t, result.Combined.Root)
	generated := r.add(t, "generated", model.SandboxPolicy{AgentDirectories: []string{"VALUE"}})
	_, err = sandboxpolicy.ComposeScopes(context.Background(), []sandboxpolicy.ScopeSelection{{Scope: sandboxpolicy.ScopeGlobal, Ref: global}, {Scope: sandboxpolicy.ScopeExplicit, Ref: generated}}, r, canonicalPaths{})
	require.ErrorIs(t, err, sandboxpolicy.ErrInvalidClosure)
	_, err = sandboxpolicy.ComposeScopes(context.Background(), []sandboxpolicy.ScopeSelection{{Scope: sandboxpolicy.ScopeGroup, Ref: global}, {Scope: sandboxpolicy.ScopeGroup, Ref: explicit}}, r, canonicalPaths{})
	require.ErrorIs(t, err, sandboxpolicy.ErrInvalidClosure)
}

func TestSandboxCompositionRejectsExactMountCollisionButRetainsNesting(t *testing.T) {
	for _, guest := range []string{"/guest/work", "/guest/work/cache", "/guest"} {
		t.Run(guest, func(t *testing.T) {
			r := newRevisions()
			bind := r.add(t, "bind", model.SandboxPolicy{Filesystem: []model.SandboxFilesystemRule{{HostPath: "/source", GuestPath: "/guest/work", Access: model.SandboxFilesystemRead}}})
			tmp := r.add(t, "tmp", model.SandboxPolicy{Tmpfs: []model.SandboxTmpfs{{GuestPath: guest}}})
			combined := r.add(t, "combined", model.SandboxPolicy{Includes: []model.SandboxProfileRef{bind, tmp}})
			_, includeErr := sandboxpolicy.ComposeIncludes(context.Background(), combined, r, canonicalPaths{})
			_, scopeErr := sandboxpolicy.ComposeScopes(context.Background(), []sandboxpolicy.ScopeSelection{{Scope: sandboxpolicy.ScopeGlobal, Ref: bind}, {Scope: sandboxpolicy.ScopeExplicit, Ref: tmp}}, r, canonicalPaths{})
			if guest == "/guest/work" {
				require.ErrorIs(t, includeErr, sandboxpolicy.ErrInvalidClosure)
				require.ErrorIs(t, scopeErr, sandboxpolicy.ErrInvalidClosure)
			} else {
				require.NoError(t, includeErr)
				require.NoError(t, scopeErr)
			}
		})
	}
}

func TestSandboxCompositionChecksAggregateEnvironmentAndGeneratedDirectoryLimit(t *testing.T) {
	for _, generated := range []bool{false, true} {
		for _, count := range []int{63, 64} {
			r := newRevisions()
			first := model.SandboxPolicy{Environment: model.Environment{}}
			for n := 0; n < 65; n++ {
				first.Environment[fmt.Sprintf("FIRST_%d", n)] = "value"
			}
			second := model.SandboxPolicy{Environment: model.Environment{}}
			for n := 0; n < count; n++ {
				name := fmt.Sprintf("SECOND_%d", n)
				if generated {
					second.AgentDirectories = append(second.AgentDirectories, name)
				} else {
					second.Environment[name] = "value"
				}
			}
			firstRef := r.add(t, "first", first)
			secondRef := r.add(t, "second", second)
			root := r.add(t, "root", model.SandboxPolicy{Includes: []model.SandboxProfileRef{firstRef, secondRef}})
			_, includeErr := sandboxpolicy.ComposeIncludes(context.Background(), root, r, nil)
			_, scopeErr := sandboxpolicy.ComposeScopes(context.Background(), []sandboxpolicy.ScopeSelection{{Scope: sandboxpolicy.ScopeGlobal, Ref: firstRef}, {Scope: sandboxpolicy.ScopeGroup, Ref: secondRef}}, r, nil)
			if count == 64 {
				require.ErrorIs(t, includeErr, sandboxpolicy.ErrInvalidClosure)
				require.ErrorIs(t, scopeErr, sandboxpolicy.ErrInvalidClosure)
			} else {
				require.NoError(t, includeErr)
				require.NoError(t, scopeErr)
			}
		}
	}
}
