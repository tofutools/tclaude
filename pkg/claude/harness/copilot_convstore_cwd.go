package harness

import (
	"path/filepath"
)

// Matching a caller's working directory against the one Copilot recorded.
//
// The two spellings arrive from different places. Copilot writes `cwd` into
// workspace.yaml already RESOLVED — it is the directory the process ended up
// in, with every symlink collapsed. A caller's cwd is whatever its environment
// handed it, which on macOS is routinely the unresolved spelling of the same
// physical directory: `/var/folders/...` is a symlink to `/private/var/...`,
// `/tmp` to `/private/tmp`, and a `$TMPDIR`-rooted worktree inherits that
// spelling all the way down. `/home` symlinked onto another volume produces the
// same shape on Linux.
//
// Comparing the two lexically therefore answers "different project" for one
// physical directory, which is not a cosmetic defect: a cwd-scoped `conv ls`
// shows nothing, and `conv resume <prefix>` reports no such conversation, for a
// session the operator is sitting inside. The contract this file implements is
// that a cwd filter matches through SYMLINKS rather than by spelling.
//
// Symlinks are the whole of it, deliberately. Two spellings that differ only by
// case on a case-insensitive volume, or two bind mounts of one directory, are
// still two directories to this filter — EvalSymlinks preserves casing and
// cannot see mount identity. Neither is a regression (the lexical comparison
// missed both too), and neither is the reported defect; folding case here would
// also need the whole case-restoration machinery the sandbox policy carries,
// which conversation lookup has no business depending on.
//
// The comparison is deliberately staged so the cheap answers stay cheap:
//
//  1. the cleaned spellings are equal — the whole answer on Linux, and on
//     macOS whenever the caller's cwd is already resolved;
//  2. the entry equals the caller's cwd RESOLVED — one EvalSymlinks per
//     listing, which is the macOS case above, since Copilot's side is already
//     physical;
//  3. only then is the entry itself resolved, memoized per distinct directory.
//
// Step 3 is what a cold listing must not turn into a filesystem sweep, and two
// things bound it: it runs only for entries the first two steps rejected, and
// it does not run at all when the caller's own cwd could not be resolved — a
// cwd that does not exist matches lexically or not at all rather than
// provoking a probe of every other project on disk.
type copilotCwdFilter struct {
	// want is the caller's cwd, cleaned. Never empty: the empty cwd sentinel
	// means "everything, everywhere" and is handled before a filter is built.
	want string
	// wantReal is want with its symlinks resolved, or "" when it could not be
	// resolved (missing, unreadable, or relative). "" disables steps 2 and 3
	// entirely, leaving the lexical comparison as the only answer.
	wantReal string
	// real memoizes the physical spelling of each entry cwd examined, so a home
	// holding many sessions in one project resolves that project once. A path
	// that could not be resolved is memoized as "" and never retried.
	real map[string]string
}

func newCopilotCwdFilter(cwd string) *copilotCwdFilter {
	f := &copilotCwdFilter{want: filepath.Clean(cwd)}
	f.wantReal, _ = physicalDirSpelling(f.want)
	return f
}

// matches reports whether projectPath names the same directory as the caller's
// cwd.
func (f *copilotCwdFilter) matches(projectPath string) bool {
	clean := filepath.Clean(projectPath)
	if clean == f.want {
		return true
	}
	if f.wantReal == "" {
		// The caller's cwd is not a resolvable directory, so there is no
		// physical directory to compare against. Anything beyond the lexical
		// answer above would be a guess.
		return false
	}
	if clean == f.wantReal {
		return true
	}
	real, ok := f.real[clean]
	if !ok {
		real, _ = physicalDirSpelling(clean)
		if f.real == nil {
			f.real = map[string]string{}
		}
		f.real[clean] = real
	}
	return real != "" && real == f.wantReal
}

// physicalDirSpelling returns path with every symlink in it resolved, or
// ("", false) when it has no physical spelling to speak of.
//
// A relative path is refused rather than resolved. filepath.EvalSymlinks would
// interpret it against the PROCESS's working directory, which for agentd or a
// long-running TUI has nothing to do with the caller whose cwd this is; a
// lexical comparison is the honest answer there.
//
// A missing or unreadable path is likewise ("", false) rather than an error.
// Both are ordinary during a listing — a project directory can be deleted while
// its conversations remain — and neither should hide a conversation whose
// recorded spelling still matches lexically.
func physicalDirSpelling(path string) (string, bool) {
	if path == "" || !filepath.IsAbs(path) {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	return resolved, true
}
