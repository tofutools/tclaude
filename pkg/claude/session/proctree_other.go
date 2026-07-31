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

// descendantCommandLinesViaChildren reports supported=false here. No process enumeration on this platform at all, so there is nothing
// cheaper to prefer.
func descendantCommandLinesViaChildren(int) (out []string, ok bool, supported bool) {
	return nil, false, false
}
