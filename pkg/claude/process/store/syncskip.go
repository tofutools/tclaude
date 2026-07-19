package store

import (
	"os"
	"testing"
)

// skipDurabilitySyncs elides every fsync in this package inside test binaries.
//
// The store fsyncs files and parent directories on each append/atomic-write so
// a power loss cannot lose acknowledged state. Tests cannot observe that
// property (they simulate crashes by truncating or rewriting files, never by
// dropping the page cache), yet the suite pays for thousands of real syncs per
// run — on some filesystems several milliseconds each — which made the
// process-store-backed packages among the slowest in `go test ./...`. Skipping
// the syncs changes nothing about write ordering, visibility, or the bytes on
// disk; only crash durability is forfeited, and only in test binaries.
//
// TCLAUDE_TEST_KEEP_FSYNC=1 restores production sync behavior for a manual
// run, e.g. when investigating a durability-adjacent failure.
var skipDurabilitySyncs = testing.Testing() && os.Getenv("TCLAUDE_TEST_KEEP_FSYNC") == ""

// maybeSync is the gate every file/dir Sync in this package goes through; see
// skipDurabilitySyncs for why tests skip it.
func maybeSync(f *os.File) error {
	if skipDurabilitySyncs {
		return nil
	}
	return f.Sync()
}
