//go:build !linux && !darwin

package session

// readProcTable has no implementation outside the supported platforms
// (Linux and macOS — see CLAUDE.md). Reporting "cannot enumerate" rather
// than "nothing is running" is the safe answer: DescendantCommandLines'
// contract is that ok=false leaves the background-shell ledger to its TTL
// instead of retiring every entry.
func readProcTable() (procTable, bool) {
	return procTable{}, false
}
