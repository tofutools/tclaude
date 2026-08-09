package agentd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitproxy_refs_test.go covers the pure half of the fetch ref transfer: what is
// accepted out of a ref listing, and what goes into an update-ref transaction.
// The behaviour that needs a real git — packed-refs, symrefs, prune, the
// isolation itself — is in gitproxy_realgit_test.go.

// TestParseGitRefLines_DropsWhatMustNotReachATransaction is the input gate on
// data that is only mostly trustworthy. `.git/packed-refs` is agent-writable,
// and git's reader is more forgiving than its ref-creation path, so a listing
// can carry a name `git update-ref` would never have produced.
func TestParseGitRefLines_DropsWhatMustNotReachATransaction(t *testing.T) {
	const sha = "1111111111111111111111111111111111111111"

	refs := parseGitRefLines(
		sha + " refs/heads/keep\n" +
			sha + " refs/remotes/origin/HEAD refs/remotes/origin/main\n" + // symref
			"nothex refs/heads/bad-oid\n" +
			"1111 refs/heads/abbreviated\n" +
			sha + " heads/not-fully-qualified\n" +
			sha + " refs/heads/has..dots\n" +
			sha + " refs/heads/tilde~1\n" +
			sha + " refs/heads/x.lock\n" +
			sha + " refs/heads/-dashy\n" + // fine as a ref, and never a flag: it is not argv-leading
			sha + " refs/heads/deep/name\n")

	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name)
		assert.Equal(t, sha, ref.OID)
	}
	assert.Equal(t, []string{"refs/heads/keep", "refs/heads/-dashy", "refs/heads/deep/name"}, names)
}

// TestValidImportableRefName_RejectsTheShapesGitWouldRefuse pins the rules that
// keep an imported name inside refs/ and inside check-ref-format.
func TestValidImportableRefName_RejectsTheShapesGitWouldRefuse(t *testing.T) {
	for _, name := range []string{
		"refs/heads/main",
		"refs/remotes/origin/feat/thing",
		"refs/tags/v1.2.3",
	} {
		assert.Truef(t, validImportableRefName(name), "%q must be importable", name)
	}
	for _, name := range []string{
		"",
		"HEAD",                    // never a ref this proxy writes
		"heads/main",              // not fully qualified
		"refs/heads/main/",        // trailing slash
		"refs/heads/main.lock",    // git's own lock suffix
		"refs/heads/a..b",         // revision range syntax
		"refs/heads/a@{0}",        // reflog syntax
		"refs/heads/a//b",         // empty segment
		"refs/heads/.hidden",      // segment starting with a dot
		"refs/heads/a b",          // a space would split the listing's fields
		"refs/heads/a\tb",         // control character
		"refs/heads/star*",        // glob
		"refs/heads/colon:",       // refspec separator
		"refs/heads/back\\slash",  // pathological on any platform
		"refs/heads/caret^",       // revision syntax
		"refs/heads/question?",    // glob
		"refs/heads/bracket[abc]", // glob
	} {
		assert.Falsef(t, validImportableRefName(name), "%q must be refused", name)
	}
}

// TestValidGitObjectID_RequiresAFullName — these values are compared for
// equality and handed to update-ref as EXPECTED values, where an abbreviation
// is not an answer.
func TestValidGitObjectID_RequiresAFullName(t *testing.T) {
	assert.True(t, validGitObjectID("1111111111111111111111111111111111111111"))
	assert.True(t, validGitObjectID(
		"1111111111111111111111111111111111111111111111111111111111111111"), "SHA-256")
	assert.False(t, validGitObjectID("1111111"), "abbreviated")
	assert.False(t, validGitObjectID("111111111111111111111111111111111111111G"))
	assert.False(t, validGitObjectID("1111111111111111111111111111111111111111 "))
	assert.False(t, validGitObjectID(""))
}

// TestAppendRefCommand_MatchesGitsNULLayout pins the wire format of the
// transaction, because getting it subtly wrong is not a parse error — it is a
// ref update aimed at the wrong place. The layout is
// `<command> SP <ref> NUL [<newvalue> NUL] [<oldvalue>] NUL`.
func TestAppendRefCommand_MatchesGitsNULLayout(t *testing.T) {
	t.Run("update against a known value", func(t *testing.T) {
		var buf strings.Builder
		appendRefCommand(&buf, "update", "refs/remotes/origin/main", "aaaa", "bbbb")
		assert.Equal(t, "update refs/remotes/origin/main\x00aaaa\x00bbbb\x00", buf.String())
	})

	t.Run("create asserts absence with an empty expected value", func(t *testing.T) {
		var buf strings.Builder
		appendRefCommand(&buf, "update", "refs/remotes/origin/new", "aaaa", "")
		assert.Equal(t, "update refs/remotes/origin/new\x00aaaa\x00\x00", buf.String(),
			"an empty expected value is git's \"this ref must not already exist\"")
	})

	t.Run("delete carries no new value", func(t *testing.T) {
		var buf strings.Builder
		appendRefCommand(&buf, "delete", "refs/remotes/origin/gone", "", "cccc")
		assert.Equal(t, "delete refs/remotes/origin/gone\x00cccc\x00", buf.String())
	})
}

// TestFetchRefspecs_AreAlwaysTheDaemonsOwn records the deliberate departure
// from `git fetch <remote>`: `remote.<name>.fetch` is agent-writable, is not
// one of the keys the gates inspect, and a value like `+refs/*:refs/*` would
// have a fetch overwrite the agent's own branches.
func TestFetchRefspecs_AreAlwaysTheDaemonsOwn(t *testing.T) {
	assert.Equal(t, []string{"+refs/heads/*:refs/remotes/origin/*"},
		fetchRefspecs("origin", "", false))

	assert.Equal(t, []string{"+refs/heads/feat/thing:refs/remotes/upstream/feat/thing"},
		fetchRefspecs("upstream", "feat/thing", false),
		"a named branch is fully qualified on both sides, so the remote's own "+
			"refspec configuration cannot reinterpret where it lands")

	specs := fetchRefspecs("origin", "", true)
	require.Len(t, specs, 2)
	assert.Equal(t, "refs/tags/*:refs/tags/*", specs[1],
		"unforced, which is `git fetch --tags` exactly: an existing tag is left alone")
}

// TestRefImportNote_SaysWhatLanded — an agent reading a fetch summary should be
// able to see that the refs arrived, rather than inferring it from silence.
func TestRefImportNote_SaysWhatLanded(t *testing.T) {
	assert.Equal(t, "", refImport{}.note(), "nothing to say when nothing changed")
	assert.Contains(t, refImport{Updated: 3}.note(), "3 updated")
	assert.Contains(t, refImport{Updated: 1, Deleted: 2}.note(), "1 updated, 2 pruned")
	assert.Contains(t, refImport{Deleted: 2}.note(), "2 pruned")
}
