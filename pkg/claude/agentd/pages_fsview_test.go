package agentd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestPageFSViewTheLastCoveringEntryDecides(t *testing.T) {
	root := testutil.CanonicalTempDir(t)
	j := func(parts ...string) string { return filepath.Join(append([]string{root}, parts...)...) }
	entry := func(p string, hidden bool) pageFSEntry { return pageFSEntry{paths: pageFSSpellings(p), hidden: hidden} }
	v := &pageFSView{class: pageFSGuarded, entries: []pageFSEntry{
		entry(j("wt"), false),
		entry(j("wt", "secret"), true),
		entry(j("wt", "secret", "shared"), false), // bound again on top of the hide
		entry(j("wt", "remapped"), true),          // a remapped entry counts as hidden
	}, always: []string{j("wt", "protected")}}

	assert.False(t, v.hidden(j("wt", "index.html")))
	assert.True(t, v.hidden(j("wt", "secret")))
	assert.True(t, v.hidden(j("wt", "secret", "a", "b.html")))
	assert.False(t, v.hidden(j("wt", "secret", "shared", "c.html")), "a later bind below a hide shows its files")
	assert.True(t, v.hidden(j("wt", "remapped", "x.html")))
	assert.True(t, v.hidden(j("wt", "protected", "x")))
	assert.False(t, v.hidden(j("wt", "secretive.html")), "a sibling sharing a prefix is not under it")
	assert.False(t, v.hidden(j("elsewhere")), "a host-inherited root shows unbound paths")

	v.constructed = true
	assert.True(t, v.hidden(j("elsewhere")), "a constructed root leaves unbound paths out")
	assert.False(t, v.hidden(j("wt", "index.html")))

	unknown := &pageFSView{always: []string{j("wt", "protected")}}
	assert.False(t, unknown.hidden(j("wt", "secret", "x")), "an unknown view hides only the protected roots")
	assert.True(t, unknown.hidden(j("wt", "protected", "x")))
	assert.False(t, unknown.readBack())

	// An exact view leaves path rules to the sandbox itself.
	exact := &pageFSView{class: pageFSSandboxRoot, entries: v.entries, always: v.always}
	assert.False(t, exact.hidden(j("wt", "secret", "x")))
	assert.True(t, exact.hidden(j("wt", "protected", "x")))
	assert.True(t, exact.readBack())
	assert.False(t, v.readBack(), "a guarded view is never read back")
}

func TestPageFSViewMatchesAPlanSpelledThroughASymlink(t *testing.T) {
	real := testutil.CanonicalTempDir(t)
	require.NoError(t, os.MkdirAll(filepath.Join(real, "wt", "secret"), 0o755))
	alias := filepath.Join(testutil.CanonicalTempDir(t), "home-link")
	require.NoError(t, os.Symlink(real, alias))
	v := &pageFSView{class: pageFSGuarded, entries: []pageFSEntry{
		{paths: pageFSSpellings(filepath.Join(alias, "wt")), hidden: false},
		{paths: pageFSSpellings(filepath.Join(alias, "wt", "secret")), hidden: true},
	}}
	// The stage walks canonical paths; the plan named the alias.
	assert.True(t, v.hidden(filepath.Join(real, "wt", "secret", "key.html")))
	assert.False(t, v.hidden(filepath.Join(real, "wt", "index.html")))
}
