// Command affected prints the subset of a shard's test packages that a change
// can actually reach, so CI does not re-run tests whose inputs did not move.
//
// It is deliberately conservative. Anything it cannot map with certainty — a
// changed file outside a Go package directory, a missing diff base, a change
// to its own source — degrades to "run everything the caller asked for". A
// wrong skip is a silent false green, so every uncertainty resolves toward
// more testing, and the reason is printed to stderr on every run.
//
// Usage:
//
//	go run ./scripts/ci/affected -base <rev> <pkg>...
//
// The arguments are the shard's own package list, in any spelling `go list`
// accepts. The selected subset is printed to stdout as import paths, one per
// line; empty stdout means the shard has nothing to run.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	base := flag.String("base", "", "git revision to diff against; empty selects everything")
	head := flag.String("head", "", "git revision to diff; empty means the working tree")
	flag.Parse()

	requested := flag.Args()
	if len(requested) == 0 {
		fmt.Fprintln(os.Stderr, "affected: no packages requested")
		os.Exit(2)
	}

	// An unusable graph is an uncertainty like any other: print why, hand the
	// caller back exactly what it asked for, and let `go test` be the thing
	// that fails if the tree is genuinely broken.
	want, err := resolvePackages(requested)
	if err != nil {
		fmt.Fprintf(os.Stderr, "affected: %v — running the full shard\n", err)
		printPackages(requested)
		return
	}
	sel, err := newSelector()
	if err != nil {
		fmt.Fprintf(os.Stderr, "affected: %v — running the full shard\n", err)
		printPackages(want)
		return
	}

	selected, reason := sel.selectFor(*base, *head, want)
	fmt.Fprintf(os.Stderr, "affected: %s — %d of %d requested packages selected\n",
		reason, len(selected), len(want))

	printPackages(selected)
}

func printPackages(pkgs []string) {
	out := bufio.NewWriter(os.Stdout)
	defer func() { _ = out.Flush() }()
	for _, p := range pkgs {
		fmt.Fprintln(out, p)
	}
}

// selectFor grades the requested packages against the diff from base to the
// working tree. It returns the packages to test and a human-readable reason.
func (s *selector) selectFor(base, head string, want []string) ([]string, string) {
	if strings.TrimSpace(base) == "" {
		return want, "no diff base given, running the full shard"
	}
	if v := os.Getenv("TCLAUDE_CI_FULL_TESTS"); v != "" && v != "0" {
		return want, "TCLAUDE_CI_FULL_TESTS is set, running the full shard"
	}
	changed, err := s.changedFiles(base, head)
	if err != nil {
		return want, fmt.Sprintf("cannot diff against %s (%v), running the full shard", base, err)
	}
	if len(changed) == 0 {
		return want, fmt.Sprintf("no files changed since %s, running the full shard", base)
	}
	changedPkgs, reason := s.mapChangedFiles(changed)
	if reason != "" {
		return want, reason
	}
	affected := s.affectedBy(changedPkgs)
	var out []string
	for _, p := range want {
		if affected[p] {
			out = append(out, p)
		}
	}
	return out, fmt.Sprintf("%d package(s) changed since %s, %d package(s) reachable from them",
		len(changedPkgs), base, len(affected))
}
