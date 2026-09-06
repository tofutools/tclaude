package agentd

// permissionReadPolicy preserves the two historical source-read contracts.
// Legacy gates treat an unreadable tier as absent; route gates fail closed.
type permissionReadPolicy uint8

const (
	permissionReadLegacy permissionReadPolicy = iota
	permissionReadRoute
)

type permissionReadDiagnostic struct {
	Tier string
	Err  error
}
