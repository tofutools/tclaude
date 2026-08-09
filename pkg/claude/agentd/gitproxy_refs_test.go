package agentd

import (
	"context"
	"errors"
	"fmt"
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

	refs := parseGitRefLines(splitNonEmptyLines(
		sha + " refs/heads/keep\n" +
			sha + " refs/remotes/origin/HEAD refs/remotes/origin/main\n" + // symref
			"nothex refs/heads/bad-oid\n" +
			"1111 refs/heads/abbreviated\n" +
			sha + " heads/not-fully-qualified\n" +
			sha + " refs/heads/has..dots\n" +
			sha + " refs/heads/tilde~1\n" +
			sha + " refs/heads/x.lock\n" +
			sha + " refs/heads/a.lock/b\n" + // .lock on a MIDDLE component
			sha + " refs/heads/-dashy\n" + // fine as a ref, and never a flag: it is not argv-leading
			sha + " refs/heads/mid./end\n" + // legal: only a LEADING dot is barred per component
			sha + " refs/heads/deep/name\n"))

	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name)
		assert.Equal(t, sha, ref.OID)
	}
	assert.Equal(t, []string{
		"refs/heads/keep", "refs/heads/-dashy", "refs/heads/mid./end", "refs/heads/deep/name",
	}, names)
}

// TestValidImportableRefName_RejectsTheShapesGitWouldRefuse pins the rules that
// keep an imported name inside refs/ and inside check-ref-format.
func TestValidImportableRefName_RejectsTheShapesGitWouldRefuse(t *testing.T) {
	for _, name := range []string{
		"refs/heads/main",
		"refs/remotes/origin/feat/thing",
		"refs/tags/v1.2.3",
		// Legal, and it must stay legal: git lists and accepts a component that
		// ENDS in a dot, so refusing it would silently drop the ref from the
		// mirror rather than protect anything.
		"refs/heads/mid./end",
	} {
		assert.Truef(t, validImportableRefName(name), "%q must be importable", name)
	}
	for _, name := range []string{
		"",
		"HEAD",                    // never a ref this proxy writes
		"heads/main",              // not fully qualified
		"refs/heads/main/",        // trailing slash
		"refs/heads/main.lock",    // git's own lock suffix
		"refs/heads/a.lock/b",     // ...on a middle component, which git also refuses
		"refs/heads/x/.hidden",    // a component starting with a dot, at any depth
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

// TestListRefs_MirrorRefusesOverflowAndNegotiationDoesNot pins the asymmetry
// between the two listings, which is easy to collapse into one and wrong to.
//
// A MIRRORED namespace cannot survive truncation: a ref that exists in the
// agent's repository but fell off the listing is imported as a creation, and
// the transaction fails with a lock error naming nothing an agent could act on.
// The NEGOTIATION namespace can: a missing "have" costs bandwidth, so refusing
// there would mean a repository with a few thousand local branches could not
// fetch through the proxy at all.
func TestListRefs_MirrorRefusesOverflowAndNegotiationDoesNot(t *testing.T) {
	const sha = "1111111111111111111111111111111111111111"
	refLines := func(n int) string {
		var buf strings.Builder
		for i := range n {
			fmt.Fprintf(&buf, "%s refs/remotes/origin/b%d\n", sha, i)
		}
		return buf.String()
	}
	// runner answers with a canned listing and records the argv it was given.
	runner := func(out string, truncated bool, seen *[]string) gitRunner {
		return func(_ context.Context, _ gitOpts, args ...string) (ProxyResult, error) {
			*seen = args
			return ProxyResult{Stdout: out, Truncated: truncated}, nil
		}
	}

	t.Run("mirror asks for one more than it accepts", func(t *testing.T) {
		var args []string
		refs, fault := listMirroredRefs(context.Background(),
			runner(refLines(3), false, &args), "refs/remotes/origin")
		require.Nil(t, fault, "%+v", fault)
		assert.Len(t, refs, 3)
		assert.Contains(t, args, fmt.Sprintf("--count=%d", maxGitProxyMirrorRefs+1),
			"one over the limit is how an overflow becomes visible at all")
	})

	t.Run("mirror refuses an overflowing listing", func(t *testing.T) {
		var args []string
		_, fault := listMirroredRefs(context.Background(),
			runner(refLines(maxGitProxyMirrorRefs+1), false, &args), "refs/remotes/origin")
		require.NotNil(t, fault)
		assert.Equal(t, "too_many_refs", fault.Code)
	})

	t.Run("mirror refuses a truncated listing", func(t *testing.T) {
		var args []string
		_, fault := listMirroredRefs(context.Background(),
			runner(refLines(3), true, &args), "refs/remotes/origin")
		require.NotNil(t, fault, "the tail is kept, so truncation drops the FIRST refs")
		assert.Equal(t, "too_many_refs", fault.Code)
	})

	t.Run("negotiation caps and carries on", func(t *testing.T) {
		var args []string
		refs, fault := listNegotiationRefs(context.Background(),
			runner(refLines(maxGitProxyHaveRefs), true, &args), "refs/heads")
		require.Nil(t, fault, "a repository with many branches must still be able to fetch")
		assert.Len(t, refs, maxGitProxyHaveRefs)
		assert.Contains(t, args, fmt.Sprintf("--count=%d", maxGitProxyHaveRefs),
			"the cap is applied by git rather than by refusing afterwards")
	})

	t.Run("a probe that could not run is a refusal, not an empty answer", func(t *testing.T) {
		failing := func(_ context.Context, _ gitOpts, _ ...string) (ProxyResult, error) {
			return ProxyResult{}, errors.New("git exploded")
		}
		_, fault := listMirroredRefs(context.Background(), failing, "refs/remotes/origin")
		require.NotNil(t, fault)
		assert.Equal(t, "ref_probe_failed", fault.Code)
		_, fault = listNegotiationRefs(context.Background(), failing, "refs/heads")
		require.NotNil(t, fault, "even the soft cap must not read a failed probe as \"no refs\"")
	})
}

// TestRefImportNote_SaysWhatLanded — an agent reading a fetch summary should be
// able to see that the refs arrived, rather than inferring it from silence.
func TestRefImportNote_SaysWhatLanded(t *testing.T) {
	assert.Equal(t, "", refImport{}.note(), "nothing to say when nothing changed")
	assert.Contains(t, refImport{Updated: 3}.note(), "3 updated")
	assert.Contains(t, refImport{Updated: 1, Deleted: 2}.note(), "1 updated, 2 pruned")
	assert.Contains(t, refImport{Deleted: 2}.note(), "2 pruned")
}
