package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The likely-cause pointer is a disclosure, so it may only appear when BOTH
// halves agree: the probe's own output says the inner namespace could not be
// created, and the host still carries the enforcing policy. Either half alone
// would be tclaude guessing at the operator's failure.
func TestStackedAppArmorLikelyCauseNeedsBothHalves(t *testing.T) {
	t.Run("both halves", func(t *testing.T) {
		t.Cleanup(setLikelyAppArmorNestedBwrapForTest(true))
		hint := stackedAppArmorLikelyCause(
			"bwrap: No permissions to create a new namespace, likely because " +
				"the kernel does not allow non-privileged user namespaces")
		require.NotEmpty(t, hint)
		assert.Contains(t, hint, "likely cause")
		assert.Contains(t, hint, "bwrap-userns-restrict")
		assert.True(t, strings.HasSuffix(hint, stackedAppArmorDocURL),
			"the line must end with the docs URL so the operator can follow it, got %q", hint)
		assert.Contains(t, stackedAppArmorDocURL,
			"docs/sandboxing.md#stacked-refuses-on-apparmor-restricted-hosts")
	})

	t.Run("unrelated failure on a restricted host", func(t *testing.T) {
		t.Cleanup(setLikelyAppArmorNestedBwrapForTest(true))
		assert.Empty(t, stackedAppArmorLikelyCause("claude: command not found"),
			"an unrelated failure must not be blamed on the policy")
	})

	t.Run("namespace failure on an unrestricted host", func(t *testing.T) {
		t.Cleanup(setLikelyAppArmorNestedBwrapForTest(false))
		assert.Empty(t,
			stackedAppArmorLikelyCause("bwrap: Creating new namespace failed: Operation not permitted"),
			"a host without the enforcing policy must not be told it has one")
	})
}

// The refusal's literal shape is a contract of its own: the hint extends the
// detail, it never displaces the capability name or the trailing clause.
func TestStackedAppArmorLikelyCauseKeepsRefusalShape(t *testing.T) {
	t.Cleanup(setLikelyAppArmorNestedBwrapForTest(true))
	detail := "Claude SRT bwrap/seccomp round-trip failed: exit status 1: " +
		"bwrap: setting up uid map: Permission denied"
	err := stackedSandboxRefusal(
		"stacked_claude_inner_policy",
		detail+stackedAppArmorLikelyCause(detail),
	)
	assert.Contains(t, err.Error(),
		"stacked requested — refused: missing capability stacked_claude_inner_policy: ")
	assert.Contains(t, err.Error(),
		"; refusing rather than falling back to tclaude-layer or harness-builtin")
	assert.Contains(t, err.Error(), stackedAppArmorDocURL)
}

// A refusal that sends the operator to a heading which no longer exists is
// worse than one that says nothing, so the anchor is pinned to the real
// heading rather than trusted to survive an edit.
func TestStackedAppArmorDocAnchorResolves(t *testing.T) {
	path, anchor, ok := strings.Cut(stackedAppArmorDocPath, "#")
	require.True(t, ok, "the doc path must carry the anchor the refusal promises")

	root, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, statErr := os.Stat(filepath.Join(root, "go.mod")); statErr == nil {
			break
		}
		parent := filepath.Dir(root)
		require.NotEqual(t, root, parent, "no go.mod above %s", root)
		root = parent
	}
	doc, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	require.NoError(t, err, "the refusal points at %s", path)

	var found bool
	for _, line := range strings.Split(string(doc), "\n") {
		heading, isHeading := strings.CutPrefix(line, "### ")
		if !isHeading {
			continue
		}
		if githubHeadingAnchor(heading) == anchor {
			found = true
			break
		}
	}
	assert.True(t, found, "no heading in %s slugifies to %q", path, anchor)
}

// githubHeadingAnchor reproduces enough of GitHub's heading slug rules for the
// one heading this package links to: lowercase, drop punctuation, spaces to
// dashes.
func githubHeadingAnchor(heading string) string {
	var out strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(heading)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			out.WriteRune(r)
		case r == ' ':
			out.WriteByte('-')
		}
	}
	return out.String()
}

func setLikelyAppArmorNestedBwrapForTest(likely bool) func() {
	prev := likelyAppArmorNestedBwrap
	likelyAppArmorNestedBwrap = func() bool { return likely }
	return func() { likelyAppArmorNestedBwrap = prev }
}
