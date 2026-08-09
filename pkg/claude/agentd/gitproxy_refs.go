package agentd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// gitproxy_refs.go moves refs between the agent's repository and the
// daemon-owned transfer directory, which is what lets a credentialed FETCH run
// somewhere the agent cannot influence.
//
// Why this file exists
//
// Push and ls-remote were converted to the transfer directory first, and both
// were nearly free: a push only READS objects, so borrowing the agent's store
// through `objects/info/alternates` was enough, and the destination is a URL
// and a resolved SHA the daemon already had.
//
// A fetch is the other shape. It has to LEAVE objects and refs behind in the
// agent's repository, and alternates only point one way. So the transfer
// directory writes into the agent's object store directly (GIT_OBJECT_DIRECTORY
// — see gitProxyXfer), which leaves refs as the only thing to move by hand:
//
//	seedRefs    before the fetch: copy the agent's remote-tracking refs and tags
//	            into the transfer directory verbatim, plus its branches under a
//	            private have-namespace.
//	importRefs  after the fetch: make the agent's refs match what the fetch
//	            produced, in ONE update-ref transaction.
//
// Seeding is not just an optimisation. It is what makes the fetch behave like a
// fetch:
//
//   - Negotiation. Without the agent's refs, the server is told we have nothing
//     and sends the entire history on every fetch.
//   - The summary. Git reports deltas against the refs it can see, so an
//     unseeded transfer directory would report every branch as `[new branch]`.
//   - `--prune`. With the agent's tracking refs present, git prunes them in the
//     transfer directory and the import mirrors the deletion; there is no
//     second guess about which refs went stale.
//   - Tag clobbering. Git refuses to overwrite an existing tag on a
//     non-forced refspec. Seeding the agent's tags is what puts the tags it
//     already has in front of that check, instead of importing a silent
//     overwrite.
//
// The one process that touches agent-writable state is the import, and it holds
// nothing worth stealing: `update-ref` speaks to no network and carries no
// credential. The pins still ride on its argv, so `core.hooksPath` keeps the
// reference-transaction hook — which fires on ref updates — pointed at the
// daemon's empty directory.

// gitRef is one (object id, ref name) pair, as `for-each-ref` reports it.
type gitRef struct{ OID, Name string }

// refTransferPatterns returns the ref namespaces a fetch mirrors: the remote's
// own tracking refs, and tags.
func refTransferPatterns(remoteName string) []string {
	return []string{"refs/remotes/" + remoteName, "refs/tags"}
}

// listMirroredRefs reads the refs of a namespace the fetch MIRRORS, where
// silent truncation is not survivable: a ref that exists in the agent's
// repository but fell off the listing would be imported as a CREATION, and the
// transaction would fail with a lock error that names nothing an agent could
// act on. So it asks for one more than it will accept and refuses at the limit.
func listMirroredRefs(ctx context.Context, run gitRunner, patterns ...string) ([]gitRef, *proxyFault) {
	lines, truncated, fault := runRefListing(ctx, run, maxGitProxyMirrorRefs+1, patterns)
	if fault != nil {
		return nil, fault
	}
	// The count is taken from the LINES, before parsing. Parsing drops symrefs
	// and malformed names, so a listing that really did overflow could come
	// back under the limit as refs and slip through a check made on those.
	if truncated || len(lines) > maxGitProxyMirrorRefs {
		return nil, faultf(http.StatusInternalServerError, "too_many_refs",
			"this repository has more than %d refs under %s; the proxy will not "+
				"mirror a ref namespace that large, and will not act on a partial "+
				"view of it either", maxGitProxyMirrorRefs, strings.Join(patterns, ", "))
	}
	return parseGitRefLines(lines), nil
}

// listNegotiationRefs reads the refs that exist only to be fetch "have"s.
//
// Truncation here costs bandwidth and nothing else — the fetch is still
// correct, it just asks for more than it needed — so this one caps and carries
// on where listMirroredRefs refuses. Refusing would mean a repository with a
// few thousand local branches could not fetch through the proxy at all, which
// is a far worse answer than a slightly larger transfer.
func listNegotiationRefs(ctx context.Context, run gitRunner, patterns ...string) ([]gitRef, *proxyFault) {
	lines, _, fault := runRefListing(ctx, run, maxGitProxyHaveRefs, patterns)
	if fault != nil {
		return nil, fault
	}
	return parseGitRefLines(lines), nil
}

// runRefListing is the probe both listings share. A probe that could not RUN is
// a refusal rather than an empty answer, for the same reason gitProbeStrict
// exists: reading a failed read as "there is nothing here" fails open.
func runRefListing(ctx context.Context, run gitRunner, count int, patterns []string) (
	lines []string, truncated bool, fault *proxyFault,
) {
	args := append([]string{
		"for-each-ref",
		// The trailing %(symref) is what keeps refs/remotes/<name>/HEAD out of
		// the transfer. Every clone has one, it is a SYMBOLIC ref pointing at
		// the remote's default branch, and an ordinary `git fetch` does not
		// touch it. Treating it as a ref like any other produces two updates
		// for the same underlying ref — one direct, one through the symref —
		// and update-ref refuses the whole transaction. A refname cannot
		// contain a space, so a third field is unambiguously the symref target.
		"--format=%(objectname) %(refname) %(symref)",
		fmt.Sprintf("--count=%d", count),
		"--",
	}, patterns...)
	probeCtx, cancel := contextWithProbeTimeout(ctx)
	defer cancel()
	res, err := run(probeCtx, gitOpts{MaxOutputBytes: maxGitProxyRefBytes}, args...)
	if err != nil || res.TimedOut {
		return nil, false, faultf(http.StatusInternalServerError, "ref_probe_failed",
			"could not list the refs under %s; refusing rather than guessing at them",
			strings.Join(patterns, ", "))
	}
	if res.ExitCode != 0 {
		return nil, false, faultf(http.StatusInternalServerError, "ref_probe_failed",
			"listing the refs under %s failed (git exit %d)",
			strings.Join(patterns, ", "), res.ExitCode)
	}
	// proxyTail keeps the TAIL, so an exceeded byte bound drops the FIRST refs
	// rather than the last. The caller decides whether that matters.
	return splitNonEmptyLines(res.Stdout), res.Truncated, nil
}

// parseGitRefLines turns `<oid> <refname> [<symref target>]` lines into pairs,
// dropping symbolic refs and anything malformed.
//
// The strictness is not decoration. `.git/packed-refs` is a file the agent can
// write by hand, and git's own reader is more forgiving than the ref-creation
// path — so a name that never came from `git update-ref` can appear in a
// listing. These names go on to reach argv and an update-ref transaction, so
// each is held to the same shape the rest of this proxy demands.
func parseGitRefLines(lines []string) []gitRef {
	var refs []gitRef
	for _, line := range lines {
		fields := strings.SplitN(line, " ", 3)
		if len(fields) > 2 && strings.TrimSpace(fields[2]) != "" {
			continue // a symbolic ref; see the format comment in listGitRefs
		}
		if len(fields) < 2 {
			continue
		}
		oid, name := fields[0], fields[1]
		if !validGitObjectID(oid) || !validImportableRefName(name) {
			continue
		}
		refs = append(refs, gitRef{OID: oid, Name: name})
	}
	return refs
}

// validGitObjectID accepts a full SHA-1 or SHA-256 object name, lower case as
// git writes it. An abbreviated one is refused: these are compared for equality
// and fed to update-ref as expected values, where a prefix is not an answer.
func validGitObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

// validImportableRefName applies git's check-ref-format rules to a full ref
// name. It is deliberately stricter than git in one direction — only `refs/…`
// names are accepted, so nothing this file writes can land on HEAD or on a bare
// name — and deliberately no stricter anywhere else, because a name this drops
// is a ref the mirror silently skips.
//
// The per-COMPONENT rules are applied per component rather than to the whole
// string. git's own ref-store reader already refuses to list a name like
// `refs/heads/a.lock/b` ("ignoring ref with broken name"), so on git 2.43
// nothing of that shape reaches here even though `.git/packed-refs` is
// agent-writable. Not relying on that is the same call the rest of this proxy
// makes about git behaviour that varies per version: if such a name ever did
// arrive, update-ref would refuse it and take the whole atomic import down
// with it.
func validImportableRefName(name string) bool {
	if !strings.HasPrefix(name, "refs/") || len(name) > maxGitProxyRefLen*4 {
		return false
	}
	// Whole-name rules: a ref may not end with "/" or ".", contain two
	// consecutive dots, or contain the reflog syntax "@{".
	if strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") ||
		strings.Contains(name, "..") || strings.Contains(name, "@{") {
		return false
	}
	// Per-component rules. Note that a component ENDING in a dot is legal —
	// `refs/heads/mid./end` is a name git both lists and accepts — so only the
	// two rules git actually states are applied here.
	for _, component := range strings.Split(strings.TrimPrefix(name, "refs/"), "/") {
		if component == "" || strings.HasPrefix(component, ".") ||
			strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	for _, r := range name {
		if r <= 0x20 || r == 0x7f {
			return false
		}
		switch r {
		case '~', '^', ':', '?', '*', '[', '\\':
			return false
		}
	}
	return true
}

// seedRefs writes the agent's current refs into the transfer directory, before
// the credentialed fetch runs. See the file header for why this is load-bearing
// rather than an optimisation.
//
// The refs are written straight into `packed-refs` rather than through
// update-ref. That is one file write instead of a subprocess per ref, and the
// transfer directory is freshly initialised, so there is no loose ref for a
// packed one to shadow. The file is deliberately written WITHOUT a traits
// header: `peeled` / `fully-peeled` would promise peel lines for annotated tags
// that are not there, and git sorts an unsorted file for itself.
func (x *gitProxyXfer) seedRefs(ctx context.Context, s *gitProxySession, remoteName string) *proxyFault {
	mirrored, fault := listMirroredRefs(ctx, s.runner(), refTransferPatterns(remoteName)...)
	if fault != nil {
		return fault
	}
	// Negotiation-only. A branch the agent committed locally may well be a
	// commit the server is about to offer — a colleague pushed it — and saying
	// so keeps the transfer to what is genuinely missing.
	heads, fault := listNegotiationRefs(ctx, s.runner(), "refs/heads")
	if fault != nil {
		return fault
	}
	// Retained as the BASELINE the import compares against. Reading the agent's
	// refs a second time at import would compare the fetch's results against
	// whatever is there by then, which is a different question: a concurrent
	// proxied fetch that already imported a newer tip would look like a ref
	// this fetch had changed, and this one would revert it. Against the seed,
	// "the fetch changed it" and "the repository still holds what we read" are
	// both exact.
	x.seeded = mirrored
	x.seedDone = true

	seeds := make([]gitRef, 0, len(mirrored)+len(heads))
	seeds = append(seeds, mirrored...)
	for _, ref := range heads {
		seeds = append(seeds, gitRef{
			OID:  ref.OID,
			Name: haveRefNamespace + strings.TrimPrefix(ref.Name, "refs/"),
		})
	}
	if len(seeds) == 0 {
		return nil
	}
	sort.Slice(seeds, func(i, j int) bool { return seeds[i].Name < seeds[j].Name })

	var buf strings.Builder
	for _, ref := range seeds {
		buf.WriteString(ref.OID)
		buf.WriteByte(' ')
		buf.WriteString(ref.Name)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(x.dir, "packed-refs"), []byte(buf.String()), 0o600); err != nil {
		return faultf(http.StatusInternalServerError, "io",
			"could not seed the transfer directory with this repository's refs: %v", err)
	}
	return nil
}

// refImport is the outcome of moving the fetched refs back.
type refImport struct {
	Updated  int
	Deleted  int
	ExitCode int
	Stderr   string
}

// importRefs applies what the fetch changed to the agent's repository.
//
// The comparison is against the SEED — what the agent's repository held when
// the transfer directory was built — not against a fresh reading of it. That
// makes both halves of each command exact:
//
//   - "the fetch changed this ref" is `fetched != seeded`, which is the same
//     predicate `git fetch` uses. Comparing against a fresh read would instead
//     mean "differs from the repository now", and a concurrent proxied fetch
//     that already imported a newer tip would be REVERTED by this one.
//   - "the repository still holds what we read" is the seeded value, named as
//     update-ref's expected value. So the whole import is one atomic
//     compare-and-swap over the window the fetch actually spans.
//
// A ref that moved underneath the fetch therefore aborts the import rather than
// silently discarding that write. The objects are already in place, so
// re-running the fetch is cheap and correct.
//
// Which namespaces move is a MIRROR of what the refspecs write, which keeps
// `git fetch` semantics without re-deriving them: refs/remotes/<name>/… is
// mirrored including `--prune` deletions, and refs/tags/… is mirrored MINUS
// deletions, because git does not prune tags on an ordinary fetch and
// `--prune-tags` is not a verb the proxy offers.
func (x *gitProxyXfer) importRefs(
	ctx context.Context, s *gitProxySession, remoteName string, prune bool,
) (refImport, *proxyFault) {
	if !x.seedDone {
		return refImport{}, faultf(http.StatusInternalServerError, "ref_import_failed",
			"internal: the transfer directory was not seeded, so there is no baseline "+
				"to import against")
	}
	fetched, fault := listMirroredRefs(ctx, x.runner(s), refTransferPatterns(remoteName)...)
	if fault != nil {
		return refImport{}, fault
	}

	baseline := make(map[string]string, len(x.seeded))
	for _, ref := range x.seeded {
		baseline[ref.Name] = ref.OID
	}
	seen := make(map[string]bool, len(fetched))

	var out refImport
	var txn strings.Builder
	for _, ref := range fetched {
		seen[ref.Name] = true
		old, existed := baseline[ref.Name]
		if existed && old == ref.OID {
			continue // the fetch left this one alone
		}
		// An empty expected value is git's "this ref must not already exist",
		// which is exactly the claim being made for a ref the repository did
		// not have when this fetch started.
		if !existed {
			old = ""
		}
		appendRefCommand(&txn, "update", ref.Name, ref.OID, old)
		out.Updated++
	}
	if prune {
		tracking := "refs/remotes/" + remoteName + "/"
		// x.seeded is in git's listing order, so the transaction is
		// deterministic for a given repository state.
		for _, ref := range x.seeded {
			if seen[ref.Name] || !strings.HasPrefix(ref.Name, tracking) {
				continue
			}
			appendRefCommand(&txn, "delete", ref.Name, "", ref.OID)
			out.Deleted++
		}
	}
	if txn.Len() == 0 {
		return out, nil
	}

	probeCtx, cancel := contextWithProbeTimeout(ctx)
	defer cancel()
	// --no-deref is what `git fetch` itself uses for remote-tracking refs, and
	// leaving it off is not a cosmetic difference. An agent that writes a
	// SYMBOLIC ref at refs/remotes/<name>/<branch> pointing into refs/heads/
	// makes update-ref follow it and land on the agent's own branch — and the
	// expected-value check follows it too, so the compare-and-swap does not
	// stop it. Verified on git 2.43: without this flag, updating such a symref
	// rewrote refs/heads/master; with it, only the symref itself is touched.
	res, err := s.gitWith(probeCtx, gitOpts{Stdin: txn.String()},
		"update-ref", "--stdin", "-z", "--no-deref")
	if err != nil {
		return refImport{}, faultf(http.StatusBadGateway, "ref_import_failed",
			"the fetch reached the remote and its objects are in place, but the refs could "+
				"not be updated in your repository: %v", err)
	}
	if res.TimedOut {
		return refImport{}, faultf(http.StatusBadGateway, "ref_import_failed",
			"the fetch reached the remote and its objects are in place, but updating the "+
				"refs in your repository timed out; re-running the fetch is safe")
	}
	out.ExitCode = res.ExitCode
	out.Stderr = res.Stderr
	return out, nil
}

// appendRefCommand writes one NUL-delimited update-ref command.
//
// The -z form is used rather than the line-based one because ref names are
// data: the line form quotes them, and a quoting bug in either direction is a
// ref update aimed somewhere other than where it was meant. The field layout is
// git's: `<command> SP <ref> NUL [<newvalue> NUL] [<oldvalue>] NUL`, where an
// empty oldvalue asserts the ref does not currently exist.
func appendRefCommand(buf *strings.Builder, command, ref, newOID, oldOID string) {
	buf.WriteString(command)
	buf.WriteByte(' ')
	buf.WriteString(ref)
	buf.WriteByte(0)
	if command != "delete" {
		buf.WriteString(newOID)
		buf.WriteByte(0)
	}
	buf.WriteString(oldOID)
	buf.WriteByte(0)
}

// fetchRefspecs is what the credentialed fetch is told to retrieve.
//
// They are always spelled out by the daemon, and that is a deliberate departure
// from `git fetch <remote>`, which reads `remote.<name>.fetch` from the
// repository. That key is agent-writable, it is not one of the ones the gates
// inspect, and a value like `+refs/*:refs/*` would have the fetch overwrite the
// agent's own branches — a repository-configured refspec is simply not
// something the daemon has any reason to honour.
func fetchRefspecs(remoteName, branch string, tags bool) []string {
	var specs []string
	if branch != "" {
		// Fully qualified on both sides: it says exactly what will be written
		// locally and cannot be reinterpreted by the remote's own configuration.
		specs = append(specs, fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branch, remoteName, branch))
	} else {
		specs = append(specs, fmt.Sprintf("+refs/heads/*:refs/remotes/%s/*", remoteName))
	}
	if tags {
		// Deliberately NOT forced, which is `git fetch --tags` exactly: an
		// existing tag is left alone and git says it would clobber. The seed put
		// the agent's own tags in front of that check.
		specs = append(specs, "refs/tags/*:refs/tags/*")
	}
	return specs
}

// contextWithProbeTimeout bounds a local git call the same way gitProbeStrict
// does. The ref helpers cannot use gitProbeStrict itself: they need the raw
// result for its exit code, its stderr and its truncation flag.
func contextWithProbeTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, gitProxyProbeTimeout)
}

// note renders what the import did, for the tail of git's own output.
// An agent reading a fetch summary should be able to see that the refs landed,
// and how many, without inferring it from silence.
func (r refImport) note() string {
	if r.Updated == 0 && r.Deleted == 0 {
		return ""
	}
	parts := make([]string, 0, 2)
	if r.Updated > 0 {
		parts = append(parts, fmt.Sprintf("%d updated", r.Updated))
	}
	if r.Deleted > 0 {
		parts = append(parts, fmt.Sprintf("%d pruned", r.Deleted))
	}
	return "refs imported into your repository (" + strings.Join(parts, ", ") + ")"
}
