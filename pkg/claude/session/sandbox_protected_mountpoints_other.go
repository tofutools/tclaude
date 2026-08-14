//go:build !linux

package session

func prepareTclaudeLayerProtectedMountpoints([]string) error { return nil }
