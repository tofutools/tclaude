//go:build !darwin || !cgo

package agentd

// releaseTrayStatusItem is darwin only: elsewhere an exec drops the tray
// with the old image.
func releaseTrayStatusItem() {}
