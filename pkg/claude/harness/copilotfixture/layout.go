package copilotfixture

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Disk-layout observation for the sandbox baseline (TCL-975).
//
// The wire fixtures next door answer "what does Copilot SEND"; this answers
// "what does Copilot TOUCH", which is the evidence a sandbox policy is built
// from. It is a post-run walk rather than a syscall trace on purpose: the
// created tree is deterministic, needs no privileged tooling, and is directly
// comparable against the pre-approved catalog a launch would be given.
//
// What it cannot see is a read that created nothing, so the walk is evidence
// for the WRITE rows only. The read/exec rows in the catalog rest on the
// traced launches recorded in the baseline's own documentation.

var (
	// The extraction staging directory carries a pid and a millisecond
	// timestamp; the per-launch lock carries a pid.
	extractingRE = regexp.MustCompile(`^\.extracting-[0-9.]+-\d+-\d+$`)
	inuseLockRE  = regexp.MustCompile(`^inuse\.\d+\.lock$`)
	// The CLI writes config.json through a uuid-suffixed temp file.
	tmpSuffixRE = regexp.MustCompile(`\.tmp\.[0-9a-fA-F-]{36}$`)
	// linux-x64, darwin-arm64, …: host-dependent, so normalized away.
	pkgPlatformRE = regexp.MustCompile(`^pkg/[a-z0-9]+-[a-z0-9]+(/|$)`)
)

// BaselineLayout is the committable projection of one run's disposable
// directory set: which of the baseline's roots the CLI actually wrote under,
// and — the row that matters most — whether it wrote anything in HOME that no
// baseline entry covers.
type BaselineLayout struct {
	// CopilotHome is the COPILOT_HOME tree (the mandatory read/write grant).
	CopilotHome SessionLayout `json:"copilotHome"`

	// Cache is the COPILOT_CACHE_HOME / XDG_CACHE_HOME tree. The unpacked
	// platform payload is collapsed to its versioned root: it is thousands of
	// files, and the grant is the root either way.
	Cache SessionLayout `json:"cache"`

	// XDGCache is the XDG_CACHE_HOME tree, which is a DIFFERENT root from the
	// package cache: COPILOT_CACHE_HOME selects the latter, while the bundled
	// runtime resolves its Microsoft/DeveloperTools device-id file here on
	// every platform, macOS included.
	XDGCache SessionLayout `json:"xdgCache"`

	// WorkDir is the launch working directory, whose grant belongs to the
	// caller and is deliberately not part of the baseline catalog.
	WorkDir SessionLayout `json:"workDir"`

	// HomeOutsideBaseline is every path created under HOME that is not inside
	// one of the trees above. It is expected to stay EMPTY: a non-empty list
	// means Copilot grew a state location the catalog does not pre-approve,
	// and a confined launch would start failing on it.
	HomeOutsideBaseline []string `json:"homeOutsideBaseline"`
}

// ObserveBaselineLayout walks the disposable directory set after a run.
//
// Paths are returned relative to their root, with volatile segments (session
// uuids, pids, extraction timestamps, the host platform tuple) normalized, so
// the result is committable as a golden.
func ObserveBaselineLayout(dirs Dirs) (BaselineLayout, error) {
	copilotHome, err := walkNormalized(dirs.Home, collapsePackagePayload)
	if err != nil {
		return BaselineLayout{}, err
	}
	cache, err := walkNormalized(dirs.Cache, collapsePackagePayload)
	if err != nil {
		return BaselineLayout{}, err
	}
	xdgCache, err := walkNormalized(dirs.XDGCache, collapsePackagePayload)
	if err != nil {
		return BaselineLayout{}, err
	}
	work, err := walkNormalized(dirs.WorkDir, nil)
	if err != nil {
		return BaselineLayout{}, err
	}

	covered := []string{dirs.Home, dirs.Cache, dirs.XDGCache, dirs.WorkDir}
	var outside []string
	err = filepath.WalkDir(dirs.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == dirs.Root {
			return nil
		}
		if slices.Contains(covered, path) {
			// fs.SkipDir means "skip this subtree" only for a directory; for a
			// file it skips the rest of the PARENT's entries. Every covered
			// root is a directory today, but a future Dirs field naming a file
			// would silently drop that file's siblings from
			// HomeOutsideBaseline — the one row this walk exists to compute.
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(dirs.Root, path)
		if relErr != nil {
			return relErr
		}
		outside = append(outside, normalizeLayoutPath(rel))
		return nil
	})
	if err != nil {
		return BaselineLayout{}, fmt.Errorf("copilotfixture: walking HOME %s: %w", dirs.Root, err)
	}
	sort.Strings(outside)

	return BaselineLayout{
		CopilotHome:         copilotHome,
		Cache:               cache,
		XDGCache:            xdgCache,
		WorkDir:             work,
		HomeOutsideBaseline: dedupeSorted(outside),
	}, nil
}

// walkNormalized lists root's contents as normalized relative paths. collapse,
// when non-nil, may replace an entry with a shorter prefix and stop the walk
// beneath it.
func walkNormalized(root string, collapse func(rel string) (string, bool)) (SessionLayout, error) {
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return SessionLayout{}, nil
	}
	var entries []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = normalizeLayoutPath(rel)
		if collapse != nil {
			if collapsed, stop := collapse(rel); stop {
				entries = append(entries, collapsed)
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		entries = append(entries, rel)
		return nil
	})
	if err != nil {
		return SessionLayout{}, fmt.Errorf("copilotfixture: walking %s: %w", root, err)
	}
	sort.Strings(entries)
	return SessionLayout{Entries: dedupeSorted(entries)}, nil
}

// collapsePackagePayload stops the walk at pkg/<platform>/<version>: the
// unpacked payload is thousands of files, and the sandbox grant is the cache
// root regardless of what is inside it.
func collapsePackagePayload(rel string) (string, bool) {
	parts := strings.Split(rel, "/")
	if len(parts) >= 3 && parts[0] == "pkg" {
		return strings.Join(parts[:3], "/"), true
	}
	return rel, false
}

// normalizeLayoutPath replaces the volatile segments of one relative path.
func normalizeLayoutPath(rel string) string {
	rel = filepath.ToSlash(rel)
	rel = uuidRE.ReplaceAllString(rel, uuidPlaceholder)
	rel = pkgPlatformRE.ReplaceAllString(rel, "pkg/<platform>$1")
	rel = tmpSuffixRE.ReplaceAllString(rel, ".tmp.<uuid>")
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		switch {
		case extractingRE.MatchString(p):
			parts[i] = ".extracting-<version>-<pid>-<timestamp>"
		case inuseLockRE.MatchString(p):
			parts[i] = "inuse.<pid>.lock"
		}
	}
	return strings.Join(parts, "/")
}

func dedupeSorted(in []string) []string {
	out := make([]string, 0, len(in))
	for i, s := range in {
		if i > 0 && s == in[i-1] {
			continue
		}
		out = append(out, s)
	}
	return out
}
