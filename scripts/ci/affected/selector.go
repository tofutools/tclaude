package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// selfPkgDir is this tool's own directory, relative to the module root. The
// selector must never grade a change to itself.
const selfPkgDir = "scripts/ci/affected"

// goPkg is the slice of `go list -json` output this tool consumes. Deps is the
// transitive import closure of the non-test package; the Test/XTest imports
// are direct only, so their own Deps are folded in when the graph is built.
type goPkg struct {
	ImportPath   string
	Dir          string
	Deps         []string
	TestImports  []string
	XTestImports []string
}

// selector holds the module's package graph plus the filesystem facts needed
// to map a changed file back to the package that owns it.
type selector struct {
	root string // module root, absolute

	// dirToPkg maps a module-root-relative directory to the import path of
	// the package `go list` reports there, for the current GOOS/GOARCH.
	dirToPkg map[string]string
	// goDirs is every module-root-relative directory that contains at least
	// one .go file, build tags ignored. A directory in goDirs but not in
	// dirToPkg is excluded on this platform, which the mapper treats as
	// unknown rather than guessing.
	goDirs map[string]bool

	// testClosure maps an import path to every in-module package its test
	// binary is built from, itself included.
	testClosure map[string]map[string]bool
}

func newSelector() (*selector, error) {
	root, err := runGit("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("locating the module root: %w", err)
	}
	root = strings.TrimSpace(root)

	pkgs, err := listPackages([]string{"./..."})
	if err != nil {
		return nil, err
	}
	goDirs, err := scanGoDirs(root)
	if err != nil {
		return nil, err
	}

	s := &selector{
		root:     root,
		dirToPkg: make(map[string]string, len(pkgs)),
		goDirs:   goDirs,
	}
	for _, p := range pkgs {
		rel, err := filepath.Rel(root, p.Dir)
		if err != nil {
			return nil, fmt.Errorf("relativizing %s: %w", p.Dir, err)
		}
		s.dirToPkg[filepath.ToSlash(rel)] = p.ImportPath
	}
	s.testClosure = buildTestClosure(pkgs)
	return s, nil
}

// buildTestClosure computes, for every package, the set of in-module packages
// that its test binary compiles in: the package itself, its own dependencies,
// and the dependency closure of everything its internal and external test
// files import.
func buildTestClosure(pkgs []goPkg) map[string]map[string]bool {
	inModule := make(map[string]goPkg, len(pkgs))
	for _, p := range pkgs {
		inModule[p.ImportPath] = p
	}
	closure := make(map[string]map[string]bool, len(pkgs))
	for _, p := range pkgs {
		seen := map[string]bool{p.ImportPath: true}
		add := func(imports []string) {
			for _, dep := range imports {
				if _, ok := inModule[dep]; !ok {
					continue // outside the module: it cannot be a changed package
				}
				seen[dep] = true
			}
		}
		add(p.Deps)
		for _, list := range [][]string{p.TestImports, p.XTestImports} {
			add(list)
			for _, dep := range list {
				if d, ok := inModule[dep]; ok {
					add(d.Deps)
				}
			}
		}
		closure[p.ImportPath] = seen
	}
	return closure
}

// affectedBy returns every in-module package whose test binary is built from
// at least one changed package.
func (s *selector) affectedBy(changed map[string]bool) map[string]bool {
	out := make(map[string]bool)
	for p, deps := range s.testClosure {
		for c := range changed {
			if deps[c] {
				out[p] = true
				break
			}
		}
	}
	return out
}

// mapChangedFiles turns changed paths into the packages that own them. A
// non-empty reason means the caller must fall back to a full run; it is
// returned instead of, not alongside, a package set.
func (s *selector) mapChangedFiles(changed []string) (map[string]bool, string) {
	out := make(map[string]bool)
	for _, f := range changed {
		f = filepath.ToSlash(f)
		if f == selfPkgDir || strings.HasPrefix(f, selfPkgDir+"/") {
			return nil, fmt.Sprintf("%s changed, running the full shard", selfPkgDir)
		}
		dir, ok := s.ownerDir(f)
		if !ok {
			return nil, fmt.Sprintf("%s is not owned by a Go package, running the full shard", f)
		}
		ip, ok := s.dirToPkg[dir]
		if !ok {
			return nil, fmt.Sprintf("%s maps to %s, which `go list` does not report on this platform, running the full shard", f, dir)
		}
		out[ip] = true
	}
	return out, ""
}

// ownerDir walks up from a changed file to the nearest directory that holds Go
// source, which is the package that embeds, reads, or compiles it. Reaching
// the module root means the file belongs to no package below it — go.mod,
// workflows, shell scripts, docs — and is reported as unowned so the caller
// runs everything. Only a .go file at the root maps to the root package.
func (s *selector) ownerDir(file string) (string, bool) {
	dir := path.Dir(file)
	if dir == "." {
		if strings.HasSuffix(file, ".go") && s.goDirs["."] {
			return ".", true
		}
		return "", false
	}
	for d := dir; d != "." && d != "/"; d = path.Dir(d) {
		if s.goDirs[d] {
			return d, true
		}
	}
	return "", false
}

// changedFiles lists the paths that differ between base and head — the working
// tree when head is empty — as module-root-relative slash paths.
func (s *selector) changedFiles(base, head string) ([]string, error) {
	args := []string{"diff", "--name-only", base}
	if head != "" {
		args = append(args, head)
	}
	out, err := runGit(append(args, "--")...)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	sort.Strings(files)
	return files, nil
}

// scanGoDirs records every directory under root holding at least one .go file,
// build tags ignored, so a file excluded on this platform still marks its
// directory as Go-owned.
func scanGoDirs(root string) (map[string]bool, error) {
	dirs := make(map[string]bool)
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); p != root && (name == ".git" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return err
		}
		dirs[filepath.ToSlash(rel)] = true
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scanning for Go directories: %w", err)
	}
	return dirs, nil
}

// resolvePackages expands the caller's package patterns to import paths,
// preserving the order `go list` reports.
func resolvePackages(patterns []string) ([]string, error) {
	pkgs, err := listPackages(patterns)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		out = append(out, p.ImportPath)
	}
	return out, nil
}

func listPackages(patterns []string) ([]goPkg, error) {
	args := append([]string{"list", "-json=ImportPath,Dir,Deps,TestImports,XTestImports"}, patterns...)
	cmd := exec.Command("go", args...)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
	}
	dec := json.NewDecoder(strings.NewReader(string(stdout)))
	var pkgs []goPkg
	for {
		var p goPkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decoding go list output: %w", err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

func runGit(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
