//go:build !linux

package agentd

// Non-Linux systray backends do not depend on a session DBus connection.
func checkTrayPrerequisites() error { return nil }
