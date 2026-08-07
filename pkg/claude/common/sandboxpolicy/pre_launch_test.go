package sandboxpolicy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func block(name, script string, exports ...string) PreLaunchBlock {
	return PreLaunchBlock{Name: name, Script: script, Exports: exports}
}

func blockNames(blocks []PreLaunchBlock) []string {
	out := make([]string, 0, len(blocks))
	for _, b := range blocks {
		out = append(out, b.Name)
	}
	return out
}

func TestNormalizePreLaunchAccepts(t *testing.T) {
	got, err := Normalize(Profile{
		Name: "p",
		PreLaunch: []PreLaunchBlock{
			block("playwright", "export PLAYWRIGHT_CLI_SESSION=x\n", "PLAYWRIGHT_CLI_SESSION"),
			block("second", "true\n"),
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"playwright", "second"}, blockNames(got.PreLaunch),
		"authored order is execution order and must survive normalization unsorted")
	assert.Equal(t, []string{"PLAYWRIGHT_CLI_SESSION"}, got.PreLaunch[0].Exports)
	assert.Nil(t, got.PreLaunch[1].Exports, "an undeclared exports list stays absent, not empty")
}

// The whole reason the feature exists: a block reaches names the declarative
// `environment` field refuses. XDG_CONFIG_HOME is reserved there because agentd
// uses it to isolate per-agent OpenCode state; PATH because the launch contract
// depends on it. A block may still name both.
func TestPreLaunchExportsMayNameReservedVariables(t *testing.T) {
	for _, name := range []string{"XDG_CONFIG_HOME", "PATH", "HOME", "TCLAUDE_SESSION_ID"} {
		_, err := Normalize(Profile{
			Name:      "p",
			PreLaunch: []PreLaunchBlock{block("b", "true\n", name)},
		})
		assert.NoError(t, err, "exports must not apply the reserved-name rule to %q", name)
	}
	// …while the declarative field still refuses them.
	_, err := Normalize(Profile{
		Name:        "p",
		Environment: []EnvironmentEntry{{Name: "XDG_CONFIG_HOME", Value: "/tmp/x"}},
	})
	assert.Error(t, err, "the reserved-name rule on `environment` is unchanged")
}

func TestNormalizePreLaunchRejects(t *testing.T) {
	for _, tc := range []struct {
		name    string
		blocks  []PreLaunchBlock
		wantErr string
	}{
		{"no name", []PreLaunchBlock{block("", "true")}, "needs a name"},
		{"bad name", []PreLaunchBlock{block("has space", "true")}, "is invalid"},
		{"leading dash", []PreLaunchBlock{block("-x", "true")}, "is invalid"},
		{"duplicate", []PreLaunchBlock{block("a", "true"), block("a", "false")}, "duplicate block name"},
		{"empty script", []PreLaunchBlock{block("a", "   \n")}, "empty script"},
		{"nul in script", []PreLaunchBlock{block("a", "true\x00")}, "NUL byte"},
		{"bad export", []PreLaunchBlock{block("a", "true", "not-a-var")}, "not a valid environment-variable name"},
		{
			"oversized script",
			[]PreLaunchBlock{block("a", strings.Repeat("x", MaxPreLaunchScriptBytes+1))},
			"maximum",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Normalize(Profile{Name: "p", PreLaunch: tc.blocks})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}

	many := make([]PreLaunchBlock, 0, MaxPreLaunchBlocks+1)
	for i := range MaxPreLaunchBlocks + 1 {
		many = append(many, block(string(rune('a'+i%26))+string(rune('0'+i/26)), "true"))
	}
	_, err := Normalize(Profile{Name: "p", PreLaunch: many})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too many blocks")
}

// Whitespace, quotes, backslashes and shell metacharacters are ordinary
// content in a shell script. Validation must not develop opinions about the
// operator's code — only the launch path's quoting is responsible for carrying
// it intact.
func TestNormalizePreLaunchKeepsScriptTextVerbatim(t *testing.T) {
	script := "set -e\nx='a b'; y=\"$x\"\nmkdir -p \"$HOME\"/{a,b}\n# comment `tick` $(sub)\n"
	got, err := Normalize(Profile{Name: "p", PreLaunch: []PreLaunchBlock{block("b", script)}})
	require.NoError(t, err)
	assert.Equal(t, script, got.PreLaunch[0].Script)
}

func TestMergePreLaunchOverridesInPlaceAndAppends(t *testing.T) {
	base := []PreLaunchBlock{block("first", "1"), block("second", "2"), block("third", "3")}

	// An override must not move the block it replaces. These are sequential
	// statements: if "second" jumped to the end, an include that merely retunes
	// it would silently change what runs before "third".
	got := mergePreLaunch(base, []PreLaunchBlock{block("second", "override")})
	assert.Equal(t, []string{"first", "second", "third"}, blockNames(got))
	assert.Equal(t, "override", got[1].Script)

	got = mergePreLaunch(base, []PreLaunchBlock{block("fourth", "4")})
	assert.Equal(t, []string{"first", "second", "third", "fourth"}, blockNames(got))

	// The accumulator must not be aliased by the result.
	assert.Equal(t, "2", base[1].Script, "merge must not mutate its input")
}

func TestFlattenComposesPreLaunchIncludesFirst(t *testing.T) {
	registry := map[string]Profile{
		"lower": {Name: "lower", PreLaunch: []PreLaunchBlock{block("shared", "from-lower"), block("only-lower", "l")}},
		"upper": {Name: "upper", PreLaunch: []PreLaunchBlock{block("only-upper", "u")}},
	}
	lookup := func(name string) (*Profile, error) {
		p, ok := registry[name]
		if !ok {
			return nil, nil
		}
		return &p, nil
	}
	got, err := Flatten(Profile{
		Name:      "root",
		Includes:  []string{"lower", "upper"},
		PreLaunch: []PreLaunchBlock{block("shared", "from-root"), block("own", "o")},
	}, lookup)
	require.NoError(t, err)
	assert.Equal(t, []string{"shared", "only-lower", "only-upper", "own"}, blockNames(got.PreLaunch),
		"included blocks run first in include order, the profile's own blocks last")
	assert.Equal(t, "from-root", got.PreLaunch[0].Script,
		"the including profile wins for a same-named block, at the included block's position")
}

func TestResolveComposesPreLaunchAcrossScopes(t *testing.T) {
	global := Profile{Name: "g", PreLaunch: []PreLaunchBlock{block("shared", "g"), block("global-only", "g2")}}
	explicit := Profile{Name: "e", PreLaunch: []PreLaunchBlock{block("shared", "e"), block("explicit-only", "e2")}}
	got, err := Resolve(Scopes{Global: &global, Explicit: &explicit})
	require.NoError(t, err)
	assert.Equal(t, []string{"shared", "global-only", "explicit-only"}, blockNames(got.PreLaunch))
	assert.Equal(t, "e", got.PreLaunch[0].Script, "the more specific scope wins")
}

// A snapshot is what resume and reincarnate replay, so blocks must survive the
// round trip byte-for-byte — and a legacy snapshot with no blocks must stay
// byte-compatible, or every stored snapshot would be rewritten by an upgrade.
func TestSnapshotRoundTripsPreLaunch(t *testing.T) {
	effective := EffectiveProfile{
		Filesystem:       []FilesystemGrant{},
		Environment:      []EnvironmentEntry{},
		AgentDirectories: []string{},
		PreLaunch:        []PreLaunchBlock{block("b", "export X=1\n", "X")},
	}
	snapshot := NewSnapshot(effective, nil)
	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)

	var decoded Snapshot
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	assert.Equal(t, effective.PreLaunch, decoded.Effective.PreLaunch)

	// NewSnapshot clones, so a later caller mutation cannot reach the frozen
	// launch authority.
	effective.PreLaunch[0].Script = "mutated"
	assert.Equal(t, "export X=1\n", snapshot.Effective.PreLaunch[0].Script)

	legacy := NewSnapshot(EffectiveProfile{
		Filesystem: []FilesystemGrant{}, Environment: []EnvironmentEntry{}, AgentDirectories: []string{},
	}, nil)
	encodedLegacy, err := json.Marshal(legacy)
	require.NoError(t, err)
	assert.NotContains(t, string(encodedLegacy), "pre_launch",
		"a profile with no blocks must encode exactly as it did before the field existed")
}

// Operator decision (TCL-1039): pre-launch blocks take NO part in lineage
// containment. A block runs inside the child's own wall, after the sandbox
// exists, so it is setup performed with already-checked authority rather than
// authority itself — every axis RequireContained guards is host access the
// child inherits, and a block grants none.
//
// This is pinned deliberately so it reads as a decision rather than surviving
// as an oversight someone later "fixes".
func TestRequireContainedIgnoresPreLaunchBlocks(t *testing.T) {
	parent := NewSnapshot(EffectiveProfile{
		Filesystem: []FilesystemGrant{}, Environment: []EnvironmentEntry{}, AgentDirectories: []string{},
		PreLaunch: []PreLaunchBlock{block("inherited", "parent")},
	}, nil)

	for _, tc := range []struct {
		name   string
		blocks []PreLaunchBlock
	}{
		{"a brand new block", []PreLaunchBlock{block("inherited", "parent"), block("added", "child")}},
		{"a changed block", []PreLaunchBlock{block("inherited", "rewritten-by-child")}},
		{"no blocks at all", nil},
		{"only new blocks", []PreLaunchBlock{block("added", "child")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			child := NewSnapshot(EffectiveProfile{
				Filesystem: []FilesystemGrant{}, Environment: []EnvironmentEntry{}, AgentDirectories: []string{},
				PreLaunch: tc.blocks,
			}, nil)
			assert.NoError(t, RequireContained(parent, child))
		})
	}
}

func TestPreLaunchExportsUnion(t *testing.T) {
	assert.Empty(t, PreLaunchExports(nil))
	assert.Equal(t, []string{"A", "B", "C"}, PreLaunchExports([]PreLaunchBlock{
		block("one", "x", "B", "A"),
		block("two", "y", "C", "A"),
		block("three", "z"),
	}))
}

// A structurally compatible predecessor left out of NormalizeSnapshotVersion's
// accept list strands EVERY live agent on upgrade: their frozen snapshot stops
// revalidating, so resume, reincarnate and status all fail. That is exactly
// what a version bump is most likely to get wrong, and it is invisible unless
// something asserts the whole range rather than the current value.
func TestEverySnapshotVersionUpToCurrentIsAccepted(t *testing.T) {
	for version := 1; version <= SnapshotVersion; version++ {
		got, err := NormalizeSnapshotVersion(Snapshot{Version: version})
		require.NoError(t, err, "snapshot version %d must still be accepted", version)
		assert.Equal(t, SnapshotVersion, got.Version, "version %d must upgrade in place", version)
	}
	_, err := NormalizeSnapshotVersion(Snapshot{Version: SnapshotVersion + 1})
	assert.Error(t, err, "a FUTURE version must still be refused rather than reinterpreted")
}

// PreLaunch is the only frozen field that becomes executed shell text, so the
// stored payload is revalidated rather than trusted.
func TestRevalidateSnapshotChecksPreLaunch(t *testing.T) {
	good := NewSnapshot(EffectiveProfile{
		Filesystem: []FilesystemGrant{}, Environment: []EnvironmentEntry{}, AgentDirectories: []string{},
		PreLaunch: []PreLaunchBlock{block("ok", "true\n")},
	}, nil)
	revalidated, err := RevalidateSnapshot(good)
	require.NoError(t, err)
	assert.Equal(t, good.Effective.PreLaunch, revalidated.Effective.PreLaunch)

	tampered := NewSnapshot(EffectiveProfile{
		Filesystem: []FilesystemGrant{}, Environment: []EnvironmentEntry{}, AgentDirectories: []string{},
		PreLaunch: []PreLaunchBlock{{Name: "not a valid name", Script: "true\n"}},
	}, nil)
	_, err = RevalidateSnapshot(tampered)
	assert.Error(t, err, "a snapshot edited after resolution must not revalidate")
}

// The fail-closed marker means "no profile tier applied at all", so it must not
// be able to carry blocks either.
func TestProfilesOmittedSnapshotRejectsPreLaunch(t *testing.T) {
	snapshot := NewSnapshot(EffectiveProfile{
		Filesystem: []FilesystemGrant{}, Environment: []EnvironmentEntry{}, AgentDirectories: []string{},
		PreLaunch: []PreLaunchBlock{block("b", "true\n")},
	}, nil)
	snapshot.ProfilesOmitted = true
	_, err := RevalidateSnapshot(snapshot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "omitted sandbox-profile snapshot contains profile values")
}
